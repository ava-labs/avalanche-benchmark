package creation

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/keychain"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	warpmessage "github.com/ava-labs/avalanchego/vms/platformvm/warp/message"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	pwallet "github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	ethcommon "github.com/ava-labs/libevm/common"
)

const (
	highValidatorCount = 3
	highWeight         = 100000
	lowWeight          = 1000
	managerWeight      = 1000
	initialBalance     = units.Avax / 10
)

var managerAddress = ethcommon.HexToAddress("0x0000000000000000000000000000000000000001")

type Result struct {
	OutputDirectory string
	State           State
}

type walletFactory func(
	context.Context,
	string,
	keychain.Keychain,
	primary.WalletConfig,
) (pwallet.Wallet, error)

func Create(ctx context.Context, cfg config.Config, outputDirectory, genesisTemplatePath string) (Result, error) {
	return create(ctx, cfg, outputDirectory, genesisTemplatePath, primary.MakePWallet)
}

func create(
	ctx context.Context,
	cfg config.Config,
	outputDirectory string,
	genesisTemplatePath string,
	newWallet walletFactory,
) (Result, error) {
	keyBytes, err := hex.DecodeString(cfg.Environment.FundingPrivateKey)
	if err != nil {
		return Result{}, fmt.Errorf("decode FUNDING_PRIVATE_KEY: %w", err)
	}
	fundingKey, err := secp256k1.ToPrivateKey(keyBytes)
	if err != nil {
		return Result{}, fmt.Errorf("load FUNDING_PRIVATE_KEY: %w", err)
	}
	template, err := os.ReadFile(genesisTemplatePath)
	if err != nil {
		return Result{}, fmt.Errorf("read required genesis template %s: %w", genesisTemplatePath, err)
	}
	genesis, err := RenderGenesis(template, fundingKey)
	if err != nil {
		return Result{}, err
	}
	if err := requireMissing(outputDirectory); err != nil {
		return Result{}, err
	}
	if err := os.Mkdir(outputDirectory, 0o700); err != nil {
		return Result{}, fmt.Errorf("create fresh output directory %s: %w", outputDirectory, err)
	}
	genesisPath := filepath.Join(outputDirectory, "genesis.json")
	if err := os.WriteFile(genesisPath, genesis, 0o644); err != nil {
		return Result{}, fmt.Errorf("write generated genesis %s: %w", genesisPath, err)
	}
	fmt.Printf("generated %s\n", genesisPath)

	identities, err := identity.Generate(outputDirectory, cfg.Nodes, cfg.Environment.ManagerCommittee)
	if err != nil {
		return Result{}, err
	}
	validatorCount := 0
	rpcCount := 0
	for _, generated := range identities.Nodes {
		switch generated.Role {
		case config.RoleValidator:
			validatorCount++
		case config.RoleRPC:
			rpcCount++
		}
	}
	fmt.Printf(
		"generated identities: validators=%d rpc=%d managers=%d root=%s\n",
		validatorCount,
		rpcCount,
		len(identities.Manager),
		outputDirectory,
	)

	state := State{
		Path:              filepath.Join(outputDirectory, "network.env"),
		Network:           cfg.Environment.Network,
		ManagerAddress:    managerAddress.Hex(),
		FundingEVMAddress: fundingKey.EthAddress().Hex(),
	}
	if err := state.Save(); err != nil {
		return Result{}, err
	}

	keychain := secp256k1fx.NewKeychain(fundingKey)
	wallet, err := newWallet(ctx, cfg.Environment.PChainAPI, keychain, primary.WalletConfig{})
	if err != nil {
		return Result{}, fmt.Errorf("connect P-chain wallet to %s: %w", cfg.Environment.PChainAPI, err)
	}
	subnetOwner := &secp256k1fx.OutputOwners{Threshold: 1, Addrs: []ids.ShortID{fundingKey.Address()}}
	validatorOwner := warpmessage.PChainOwner{Threshold: 1, Addresses: []ids.ShortID{fundingKey.Address()}}

	fmt.Println("creating manager subnet")
	managerSubnetTx, err := wallet.IssueCreateSubnetTx(subnetOwner)
	if err != nil {
		return Result{}, fmt.Errorf("manager CreateSubnetTx: %w", err)
	}
	state.ManagerSubnetID = managerSubnetTx.ID()
	fmt.Printf("accepted manager CreateSubnetTx %s\n", managerSubnetTx.ID())
	if err := state.Save(); err != nil {
		return Result{}, err
	}

	wallet, err = makeWallet(ctx, cfg.Environment.PChainAPI, keychain, state, newWallet)
	if err != nil {
		return Result{}, err
	}
	fmt.Println("creating management chain")
	managerChainTx, err := wallet.IssueCreateChainTx(state.ManagerSubnetID, genesis, constants.SubnetEVMID, nil, "management")
	if err != nil {
		return Result{}, fmt.Errorf("manager CreateChainTx: %w", err)
	}
	state.ManagerChainID = managerChainTx.ID()
	fmt.Printf("accepted manager CreateChainTx %s\n", managerChainTx.ID())
	if err := state.Save(); err != nil {
		return Result{}, err
	}

	managerValidators, err := conversionValidators(identities.Manager, func(identity.Identity) uint64 { return managerWeight }, validatorOwner)
	if err != nil {
		return Result{}, err
	}
	fmt.Println("converting manager subnet to a self-managed L1")
	managerConvertTx, err := wallet.IssueConvertSubnetToL1Tx(
		state.ManagerSubnetID,
		state.ManagerChainID,
		managerAddress.Bytes(),
		managerValidators,
	)
	if err != nil {
		return Result{}, fmt.Errorf("manager ConvertSubnetToL1Tx: %w", err)
	}
	state.ManagerConvertTxID = managerConvertTx.ID()
	fmt.Printf("accepted manager ConvertSubnetToL1Tx %s\n", managerConvertTx.ID())
	if err := state.Save(); err != nil {
		return Result{}, err
	}

	fmt.Println("creating main subnet")
	mainSubnetTx, err := wallet.IssueCreateSubnetTx(subnetOwner)
	if err != nil {
		return Result{}, fmt.Errorf("main CreateSubnetTx: %w", err)
	}
	state.SubnetID = mainSubnetTx.ID()
	fmt.Printf("accepted main CreateSubnetTx %s\n", mainSubnetTx.ID())
	if err := state.Save(); err != nil {
		return Result{}, err
	}

	wallet, err = makeWallet(ctx, cfg.Environment.PChainAPI, keychain, state, newWallet)
	if err != nil {
		return Result{}, err
	}
	fmt.Println("creating main chain")
	mainChainTx, err := wallet.IssueCreateChainTx(state.SubnetID, genesis, constants.SubnetEVMID, nil, "benchmark")
	if err != nil {
		return Result{}, fmt.Errorf("main CreateChainTx: %w", err)
	}
	state.ChainID = mainChainTx.ID()
	fmt.Printf("accepted main CreateChainTx %s\n", mainChainTx.ID())
	if err := state.Save(); err != nil {
		return Result{}, err
	}

	validators := make([]identity.Identity, 0, len(identities.Nodes))
	for _, generated := range identities.Nodes {
		if generated.Role == config.RoleValidator {
			validators = append(validators, generated)
		}
	}
	weightByNode := make(map[int]uint64, len(validators))
	for i, validator := range validators {
		weightByNode[validator.NodeNumber] = lowWeight
		if i < highValidatorCount {
			weightByNode[validator.NodeNumber] = highWeight
		}
	}
	mainValidators, err := conversionValidators(validators, func(generated identity.Identity) uint64 {
		return weightByNode[generated.NodeNumber]
	}, validatorOwner)
	if err != nil {
		return Result{}, err
	}
	fmt.Println("converting main subnet to an L1 managed by the management chain")
	mainConvertTx, err := wallet.IssueConvertSubnetToL1Tx(
		state.SubnetID,
		state.ManagerChainID,
		managerAddress.Bytes(),
		mainValidators,
	)
	if err != nil {
		return Result{}, fmt.Errorf("main ConvertSubnetToL1Tx: %w", err)
	}
	state.ConvertTxID = mainConvertTx.ID()
	fmt.Printf("accepted main ConvertSubnetToL1Tx %s\n", mainConvertTx.ID())
	if err := state.Save(); err != nil {
		return Result{}, err
	}

	fmt.Printf("creation complete; generated state %s\n", state.Path)
	return Result{OutputDirectory: outputDirectory, State: state}, nil
}

func requireMissing(path string) error {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return fmt.Errorf("creation output %s already exists; remove it explicitly before starting a clean creation", path)
	case os.IsNotExist(err):
		return nil
	default:
		return fmt.Errorf("inspect creation output %s: %w", path, err)
	}
}

func makeWallet(
	ctx context.Context,
	api string,
	keys keychain.Keychain,
	state State,
	newWallet walletFactory,
) (pwallet.Wallet, error) {
	subnetIDs := make([]ids.ID, 0, 2)
	if state.ManagerSubnetID != ids.Empty {
		subnetIDs = append(subnetIDs, state.ManagerSubnetID)
	}
	if state.SubnetID != ids.Empty {
		subnetIDs = append(subnetIDs, state.SubnetID)
	}
	wallet, err := newWallet(ctx, api, keys, primary.WalletConfig{SubnetIDs: subnetIDs})
	if err != nil {
		return nil, fmt.Errorf("refresh P-chain wallet for subnet ownership: %w", err)
	}
	return wallet, nil
}

func conversionValidators(
	identities []identity.Identity,
	weight func(identity.Identity) uint64,
	owner warpmessage.PChainOwner,
) ([]*txs.ConvertSubnetToL1Validator, error) {
	validators := make([]*txs.ConvertSubnetToL1Validator, 0, len(identities))
	for _, generated := range identities {
		if generated.Proof == nil {
			return nil, fmt.Errorf("identity %s has no BLS proof of possession", generated.Name)
		}
		validators = append(validators, &txs.ConvertSubnetToL1Validator{
			NodeID:                generated.NodeID.Bytes(),
			Weight:                weight(generated),
			Balance:               initialBalance,
			Signer:                *generated.Proof,
			RemainingBalanceOwner: owner,
			DeactivationOwner:     owner,
		})
	}
	sort.Slice(validators, func(i, j int) bool {
		return bytes.Compare(validators[i].NodeID, validators[j].NodeID) < 0
	})
	return validators, nil
}
