package main

import (
	"context"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ava-labs/avalanchego/genesis"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/staking"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm/signer"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
)

// l1Result is the output of createL1.
type l1Result struct {
	SubnetID ids.ID
	ChainID  ids.ID
}

// createL1 issues CreateSubnet, CreateChain, and ConvertSubnetToL1 against
// the primary network reachable at controlAPI (e.g. http://127.0.0.1:9650).
//
// Validators (the 5 DC1 NodeIDs) are pre-known. Their BLS PoPs are computed
// locally from staking/dc1/{1..5}/signer.key. Validators do NOT need to be
// online -- ConvertSubnetToL1Tx just registers them on P-chain.
func createL1(ctx context.Context, cfg *config, controlAPI string) (*l1Result, error) {
	genesisPath := filepath.Join(cfg.configDir, "genesis.json")
	genesisBytes, err := os.ReadFile(genesisPath)
	if err != nil {
		return nil, fmt.Errorf("read genesis %s: %w", genesisPath, err)
	}

	fmt.Println("[create-l1] connecting wallet to", controlAPI)
	kc := secp256k1fx.NewKeychain(genesis.EWOQKey)
	wallet, err := primary.MakePWallet(ctx, controlAPI, kc, primary.WalletConfig{})
	if err != nil {
		return nil, fmt.Errorf("connect wallet: %w", err)
	}

	owner := &secp256k1fx.OutputOwners{
		Threshold: 1,
		Addrs:     []ids.ShortID{genesis.EWOQKey.Address()},
	}

	fmt.Println("[create-l1] CreateSubnetTx ...")
	subnetTx, err := wallet.IssueCreateSubnetTx(owner)
	if err != nil {
		return nil, fmt.Errorf("create subnet: %w", err)
	}
	subnetID := subnetTx.ID()
	fmt.Println("[create-l1]   subnet:", subnetID)

	// Re-sync wallet so it tracks the new subnet's UTXOs/auth.
	wallet, err = primary.MakePWallet(ctx, controlAPI, kc, primary.WalletConfig{
		SubnetIDs: []ids.ID{subnetID},
	})
	if err != nil {
		return nil, fmt.Errorf("re-sync wallet: %w", err)
	}

	fmt.Println("[create-l1] CreateChainTx ...")
	chainTx, err := wallet.IssueCreateChainTx(
		subnetID,
		genesisBytes,
		constants.SubnetEVMID,
		nil,
		"failoverchain",
	)
	if err != nil {
		return nil, fmt.Errorf("create chain: %w", err)
	}
	chainID := chainTx.ID()
	fmt.Println("[create-l1]   chain:", chainID)

	fmt.Println("[create-l1] computing BLS PoPs for the 5 DC1 validators ...")
	validators := make([]*txs.ConvertSubnetToL1Validator, 0, 5)
	for i := 1; i <= 5; i++ {
		nodeIDStr := cfg.dc1NodeIDs[i-1]
		nodeID, err := ids.NodeIDFromString(nodeIDStr)
		if err != nil {
			return nil, fmt.Errorf("parse DC1 node ID %d (%q): %w", i, nodeIDStr, err)
		}

		signerKeyPath := filepath.Join(cfg.stakingDir, "dc1", fmt.Sprintf("%d", i), "signer.key")
		skBytes, err := os.ReadFile(signerKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", signerKeyPath, err)
		}
		sk, err := localsigner.FromBytes(skBytes)
		if err != nil {
			return nil, fmt.Errorf("load BLS key %s: %w", signerKeyPath, err)
		}
		pop, err := signer.NewProofOfPossession(sk)
		if err != nil {
			return nil, fmt.Errorf("build PoP for DC1 node %d: %w", i, err)
		}

		validators = append(validators, &txs.ConvertSubnetToL1Validator{
			NodeID:  nodeID.Bytes(),
			Weight:  units.Schmeckle,
			Balance: units.Avax,
			Signer:  *pop,
		})
		fmt.Printf("[create-l1]   dc1-%d: %s\n", i, nodeID)
	}

	// Sanity check: the on-disk staker.crt for each DC1 entry should match
	// the hardcoded NodeID. Catches the case where someone regenerated keys
	// but forgot to re-run staking-tools to refresh node-ids.env.
	for i := 1; i <= 5; i++ {
		certPath := filepath.Join(cfg.stakingDir, "dc1", fmt.Sprintf("%d", i), "staker.crt")
		got, err := nodeIDFromCertFile(certPath)
		if err != nil {
			return nil, err
		}
		if got.String() != cfg.dc1NodeIDs[i-1] {
			return nil, fmt.Errorf("dc1-%d staker.crt yields %s but node-ids.env says %s -- regenerate node-ids.env", i, got, cfg.dc1NodeIDs[i-1])
		}
	}

	fmt.Println("[create-l1] ConvertSubnetToL1Tx ...")
	_, err = wallet.IssueConvertSubnetToL1Tx(
		subnetID,
		chainID,
		[]byte{}, // empty validator-manager address; we manage validators via P-chain only
		validators,
	)
	if err != nil {
		return nil, fmt.Errorf("convert subnet to L1: %w", err)
	}

	// Brief settle window: the conversion needs to be reflected in the
	// node's view of the validator set before chain nodes start querying
	// P-chain on bootstrap.
	time.Sleep(5 * time.Second)

	return &l1Result{SubnetID: subnetID, ChainID: chainID}, nil
}

func nodeIDFromCertFile(certPath string) (ids.NodeID, error) {
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return ids.NodeID{}, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return ids.NodeID{}, fmt.Errorf("no PEM block in %s", certPath)
	}
	cert, err := staking.ParseCertificate(block.Bytes)
	if err != nil {
		return ids.NodeID{}, fmt.Errorf("parse %s: %w", certPath, err)
	}
	return ids.NodeIDFromCert(cert), nil
}
