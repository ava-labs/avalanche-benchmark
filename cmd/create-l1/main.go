package main

import (
	"context"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	"github.com/joho/godotenv"
)

var outputFile string

const (
	pchainURI             = "http://127.0.0.1:9650"
	l1ValidatorStartIndex = 6
	minValidators         = 3
)

// splitTrim parses a comma-separated list, trimming blanks and dropping empties.
func splitTrim(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func main() {
	flag.StringVar(&outputFile, "output", "", "Write SUBNET_ID and CHAIN_ID to this file")
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Find .env file (look in script dir, then current dir)
	envPath := findEnvFile()
	if envPath == "" {
		return fmt.Errorf(".env file not found")
	}

	if err := godotenv.Load(envPath); err != nil {
		return fmt.Errorf("failed to load .env: %w", err)
	}

	// Validator set: prefer the per-role VALIDATOR_IPS (its length is the validator
	// count), else fall back to the legacy positional NODE_IPS (first 3 = validators).
	// The IPs are display-only — registration is by committed staking key (NodeID),
	// IP-agnostic — so the count is what matters here.
	var nodeIPs []string
	if v := os.Getenv("VALIDATOR_IPS"); v != "" {
		nodeIPs = splitTrim(v)
	} else if raw := os.Getenv("NODE_IPS"); raw != "" {
		nodeIPs = splitTrim(raw)
		if len(nodeIPs) > minValidators {
			nodeIPs = nodeIPs[:minValidators]
		}
	} else {
		return fmt.Errorf("set VALIDATOR_IPS (per-role) or NODE_IPS (legacy) in .env")
	}
	if len(nodeIPs) < minValidators {
		return fmt.Errorf("need at least %d validator IPs, got %d", minValidators, len(nodeIPs))
	}
	l1ValidatorCount := len(nodeIPs)

	fmt.Println("=== Create L1 ===")
	fmt.Printf("  P-chain API: %s\n", pchainURI)
	for i, ip := range nodeIPs {
		fmt.Printf("  L1 validator %d: %s staking/l1/%d\n", i+1, ip, l1ValidatorStartIndex+i)
	}
	fmt.Println()

	ctx := context.Background()

	// Load genesis
	genesisPath := findGenesisFile()
	if genesisPath == "" {
		return fmt.Errorf("genesis.json not found")
	}
	genesisBytes, err := os.ReadFile(genesisPath)
	if err != nil {
		return fmt.Errorf("failed to read genesis: %w", err)
	}
	fmt.Printf("Using genesis: %s\n", genesisPath)

	// Create wallet using node1
	fmt.Println("[1/4] Creating wallet...")
	kc := secp256k1fx.NewKeychain(genesis.EWOQKey)
	wallet, err := primary.MakePWallet(ctx, pchainURI, kc, primary.WalletConfig{})
	if err != nil {
		return fmt.Errorf("failed to create wallet: %w", err)
	}

	// Create subnet
	fmt.Println("[2/4] Creating subnet...")
	owner := &secp256k1fx.OutputOwners{
		Threshold: 1,
		Addrs:     []ids.ShortID{genesis.EWOQKey.Address()},
	}
	subnetTx, err := wallet.IssueCreateSubnetTx(owner)
	if err != nil {
		return fmt.Errorf("failed to create subnet: %w", err)
	}
	subnetID := subnetTx.ID()
	fmt.Printf("  Subnet ID: %s\n", subnetID)

	// Re-sync wallet with subnet
	wallet, err = primary.MakePWallet(ctx, pchainURI, kc, primary.WalletConfig{
		SubnetIDs: []ids.ID{subnetID},
	})
	if err != nil {
		return fmt.Errorf("failed to re-sync wallet: %w", err)
	}

	// Create chain
	fmt.Println("[3/4] Creating chain...")
	chainTx, err := wallet.IssueCreateChainTx(
		subnetID,
		genesisBytes,
		constants.SubnetEVMID,
		nil,
		"benchmarkchain",
	)
	if err != nil {
		return fmt.Errorf("failed to create chain: %w", err)
	}
	chainID := chainTx.ID()
	fmt.Printf("  Chain ID: %s\n", chainID)

	fmt.Println("[4/4] Converting subnet to L1...")
	validators, err := buildValidatorsFromCommittedKeys(l1ValidatorStartIndex, l1ValidatorCount)
	if err != nil {
		return err
	}

	// Convert to L1
	fmt.Println("  Issuing ConvertSubnetToL1Tx...")
	_, err = wallet.IssueConvertSubnetToL1Tx(
		subnetID,
		chainID,
		[]byte{}, // Empty manager address
		validators,
	)
	if err != nil {
		return fmt.Errorf("failed to convert subnet to L1: %w", err)
	}

	// Wait for chain
	fmt.Println("  Waiting for chain to be ready...")
	time.Sleep(5 * time.Second)

	// Write output file if requested
	if outputFile != "" {
		content := fmt.Sprintf("SUBNET_ID=%s\nCHAIN_ID=%s\n", subnetID, chainID)
		if err := os.WriteFile(outputFile, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write output file: %w", err)
		}
	}

	// Print results
	fmt.Println()
	fmt.Println("=== L1 Created Successfully ===")
	fmt.Println()
	fmt.Printf("Subnet ID: %s\n", subnetID)
	fmt.Printf("Chain ID:  %s\n", chainID)
	fmt.Printf("Validators: staking/l1/%d..%d (%d total)\n", l1ValidatorStartIndex, l1ValidatorStartIndex+l1ValidatorCount-1, l1ValidatorCount)
	fmt.Println()
	fmt.Println("RPC endpoints (NOT live yet — these start serving only after")
	fmt.Println("./03_wipe_and_deploy_l1.sh deploys and boots the validators):")
	for i, ip := range nodeIPs {
		fmt.Printf("  Node %d: http://%s:9652/ext/bc/%s/rpc\n", i+1, ip, chainID)
	}

	return nil
}

func buildValidatorsFromCommittedKeys(startIndex, count int) ([]*txs.ConvertSubnetToL1Validator, error) {
	if startIndex < 1 {
		return nil, fmt.Errorf("validator start index must be positive, got %d", startIndex)
	}
	if count < 1 {
		return nil, fmt.Errorf("validator count must be positive, got %d", count)
	}

	stakingDir := findStakingDir()
	if stakingDir == "" {
		return nil, fmt.Errorf("staking directory not found")
	}
	nodeIDsPath := filepath.Join(stakingDir, "node-ids.env")
	nodeIDs, err := godotenv.Read(nodeIDsPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", nodeIDsPath, err)
	}

	fmt.Printf("  Computing BLS PoPs for committed L1 identities %d..%d...\n", startIndex, startIndex+count-1)
	validators := make([]*txs.ConvertSubnetToL1Validator, 0, count)
	for i := startIndex; i < startIndex+count; i++ {
		nodeIDStr := strings.TrimSpace(nodeIDs[fmt.Sprintf("L1_%d_NODE_ID", i)])
		if nodeIDStr == "" {
			return nil, fmt.Errorf("missing L1_%d_NODE_ID in %s", i, nodeIDsPath)
		}
		nodeID, err := ids.NodeIDFromString(nodeIDStr)
		if err != nil {
			return nil, fmt.Errorf("parse L1_%d_NODE_ID %q: %w", i, nodeIDStr, err)
		}

		keyDir := filepath.Join(stakingDir, "l1", strconv.Itoa(i))
		certNodeID, err := nodeIDFromCertFile(filepath.Join(keyDir, "staker.crt"))
		if err != nil {
			return nil, err
		}
		if certNodeID != nodeID {
			return nil, fmt.Errorf("staking/l1/%d staker.crt yields %s but node-ids.env says %s", i, certNodeID, nodeID)
		}

		skBytes, err := os.ReadFile(filepath.Join(keyDir, "signer.key"))
		if err != nil {
			return nil, fmt.Errorf("read staking/l1/%d signer.key: %w", i, err)
		}
		sk, err := localsigner.FromBytes(skBytes)
		if err != nil {
			return nil, fmt.Errorf("load staking/l1/%d signer.key: %w", i, err)
		}
		pop, err := signer.NewProofOfPossession(sk)
		if err != nil {
			return nil, fmt.Errorf("build PoP for staking/l1/%d: %w", i, err)
		}

		validators = append(validators, &txs.ConvertSubnetToL1Validator{
			NodeID:  nodeID.Bytes(),
			Weight:  units.Schmeckle,
			Balance: units.Avax,
			Signer:  *pop,
		})
		fmt.Printf("    staking/l1/%d: %s\n", i, nodeID)
	}
	return validators, nil
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

func findEnvFile() string {
	// Check executable directory
	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		envPath := filepath.Join(exeDir, "..", ".env")
		if _, err := os.Stat(envPath); err == nil {
			return envPath
		}
	}

	// Check current directory
	if _, err := os.Stat(".env"); err == nil {
		return ".env"
	}

	return ""
}

func findGenesisFile() string {
	// Check executable directory
	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		genesisPath := filepath.Join(exeDir, "..", "genesis.json")
		if _, err := os.Stat(genesisPath); err == nil {
			return genesisPath
		}
	}

	// Check current directory
	if _, err := os.Stat("genesis.json"); err == nil {
		return "genesis.json"
	}

	return ""
}

func findStakingDir() string {
	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		stakingPath := filepath.Join(exeDir, "..", "staking")
		if info, err := os.Stat(stakingPath); err == nil && info.IsDir() {
			return stakingPath
		}
	}

	if info, err := os.Stat("staking"); err == nil && info.IsDir() {
		return "staking"
	}
	return ""
}
