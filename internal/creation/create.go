package creation

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/apps/settlement-feed/oraclecontracts"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/funding"
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
	ethcrypto "github.com/ava-labs/libevm/crypto"
)

const (
	initialBalance     = units.Avax / 10
	creationFeeReserve = units.Avax / 10
)

var (
	managerAddress = ethcommon.HexToAddress("0x0000000000000000000000000000000000000001")
	// The oracle contracts have no constructors; their deployed bytecode sits
	// at these fixed addresses in the respective Genesis allocs, configured
	// entirely through explicit storage slots.
	AggregatorAddress = ethcommon.HexToAddress("0x000000000000000000000000000000000000FEED")
	ReceiverAddress   = ethcommon.HexToAddress("0x0000000000000000000000000000000000FeedED")
	// The direct feed is Chainlink-shaped: consumers point at the proxy, the
	// publisher writes to the aggregator behind it.
	PriceFeedAddress           = ethcommon.HexToAddress("0x00000000000000000000000000000000FeedF00d")
	PriceFeedAggregatorAddress = ethcommon.HexToAddress("0x00000000000000000000000000000000FeedFacE")
)

// priceFeedPair is the aggregator's description(). One pair for now; more
// pairs mean more aggregator+proxy instances at their own addresses.
const priceFeedPair = "USDC / USD"

func shortString(value string) ethcommon.Hash {
	if len(value) > 31 {
		panic("short-string slot encoding holds at most 31 bytes")
	}
	var out ethcommon.Hash
	copy(out[:], value)
	out[31] = byte(len(value))
	return out
}

// phaseAggregatorsSlot returns the storage slot of phaseAggregators[phase] in
// PriceFeedProxy: keccak256(pad32(phase) . pad32(4)), mapping base slot 4.
func phaseAggregatorsSlot(phase uint64) ethcommon.Hash {
	var key [64]byte
	copy(key[0:32], ethcommon.BigToHash(new(big.Int).SetUint64(phase)).Bytes())
	copy(key[32:64], ethcommon.BigToHash(big.NewInt(4)).Bytes())
	return ethcrypto.Keccak256Hash(key[:])
}

// DirectFeedAllocations is the single source of truth for the direct price
// feed's on-chain state: the aggregator the feeder publishes to, behind the
// proxy consumers read, seeded at phase 1. `oracle upgrade` renders these
// accounts as a stateUpgrades entry; the app installs through the upgrade
// history, never through genesis. Every seeded value is non-zero BY
// CONSTRUCTION: an explicit zero in upgrade.json passes the first restart
// and then bricks the node, because the database reads the zero back as
// absent and the deep-equal check fails.
func DirectFeedAllocations(feederAddress ethcommon.Address) []ContractAllocation {
	return []ContractAllocation{
		{
			Address:     PriceFeedAggregatorAddress,
			RuntimeCode: oraclecontracts.PriceAggregatorRuntime,
			Storage: map[ethcommon.Hash]ethcommon.Hash{
				{}:                                  ethcommon.BytesToHash(feederAddress.Bytes()),
				ethcommon.BigToHash(ethcommon.Big1): shortString(priceFeedPair),
			},
		},
		{
			Address:     PriceFeedAddress,
			RuntimeCode: oraclecontracts.PriceFeedProxyRuntime,
			Storage: map[ethcommon.Hash]ethcommon.Hash{
				{}:                                  ethcommon.BytesToHash(feederAddress.Bytes()),
				ethcommon.BigToHash(ethcommon.Big1): ethcommon.BytesToHash(PriceFeedAggregatorAddress.Bytes()),
				ethcommon.BigToHash(ethcommon.Big2): ethcommon.BigToHash(ethcommon.Big1),
				phaseAggregatorsSlot(1):             ethcommon.BytesToHash(PriceFeedAggregatorAddress.Bytes()),
			},
		},
	}
}

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

// chainTemplatePath resolves a chain's genesis template: an override at
// chains/<name>/genesis-template.json wins, the root template is the
// default. The oracle chain's root default keeps its dedicated file.
func chainTemplatePath(root, chain string) string {
	override := filepath.Join(root, "chains", chain, "genesis-template.json")
	if info, err := os.Stat(override); err == nil && !info.IsDir() {
		return override
	}
	if chain == config.OracleChain {
		return filepath.Join(root, "oracle-genesis-template.json")
	}
	return filepath.Join(root, "genesis-template.json")
}

// genesisFileName is the creation output of one chain's genesis. Main keeps
// the bare name; the oracle chain's file follows the same rule it always had.
func genesisFileName(chain string) string {
	if chain == config.MainChain {
		return "genesis.json"
	}
	return "genesis-" + chain + ".json"
}

// validateDistinctChainIDs refuses two chains that share one EVM chainId.
// The genesis funds the same addresses on every chain, at nonce 0, so a
// shared chainId lets an EIP-155 transaction from one chain replay on the
// other. This happens exactly when a second chain falls back to the root
// genesis template.
func validateDistinctChainIDs(root string, chains []string) error {
	owner := make(map[string]string, len(chains))
	for _, chain := range chains {
		path := chainTemplatePath(root, chain)
		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read required genesis template %s: %w", path, err)
		}
		var document struct {
			Config struct {
				ChainID json.Number `json:"chainId"`
			} `json:"config"`
		}
		if err := json.Unmarshal(contents, &document); err != nil {
			return fmt.Errorf("parse genesis template %s: %w", path, err)
		}
		chainID := document.Config.ChainID.String()
		if chainID == "" {
			return fmt.Errorf("genesis template %s: required config.chainId is not provided", path)
		}
		if previous, taken := owner[chainID]; taken {
			return fmt.Errorf(
				"chains %q and %q share EVM chainId %s; a shared chainId lets a transaction replay across the chains. Give chain %q its own template with a distinct chainId at chains/%s/genesis-template.json",
				previous, chain, chainID, chain, chain,
			)
		}
		owner[chainID] = chain
	}
	return nil
}

func Create(ctx context.Context, environment config.Environment, outputDirectory, root string) (Result, error) {
	statePath := filepath.Join(outputDirectory, "network.env")
	if err := requireMissing(statePath); err != nil {
		return Result{}, err
	}
	publicPath := filepath.Join(outputDirectory, "public.json")
	// Key generation already held these values in memory. Loading the published
	// file anyway is deliberate: every run exercises the same public-only
	// handover used when keygen and create execute in different trust domains.
	public, publicDigest, err := LoadPublic(publicPath)
	if err != nil {
		return Result{}, err
	}
	for _, chain := range public.Chains() {
		if err := requireMissing(filepath.Join(outputDirectory, genesisFileName(chain))); err != nil {
			return Result{}, err
		}
	}
	fundingInfo, err := funding.Inspect(ctx, environment)
	if err != nil {
		return Result{}, fmt.Errorf("funding preflight: %w", err)
	}
	requiredBalance := requiredFreshCreateBalance(public)
	if fundingInfo.Balance < requiredBalance {
		return Result{}, fmt.Errorf(
			"funding preflight: P-chain address %s has %s, fresh creation requires at least %s; add AVAX and run `go run ./cmd/l1 address` before retrying",
			fundingInfo.Addresses.PChain,
			formatAVAX(fundingInfo.Balance),
			formatAVAX(requiredBalance),
		)
	}
	fmt.Printf(
		"funding preflight passed: %s has %s, required minimum %s\n",
		fundingInfo.Addresses.PChain,
		formatAVAX(fundingInfo.Balance),
		formatAVAX(requiredBalance),
	)
	printPublic(public, publicPath, publicDigest)
	return create(ctx, environment, outputDirectory, root, public, primary.MakePWallet)
}

func ValidateManagerCommittee(size int) error {
	if size != 1 && size != 4 {
		return fmt.Errorf("create manager committee must be 1 or 4, got %d", size)
	}
	return nil
}

func requiredFreshCreateBalance(public Public) uint64 {
	validatorCount := 0
	for _, node := range public.Nodes {
		if node.Role == config.RoleValidator || node.Role == config.RoleOracleValidator {
			validatorCount++
		}
	}
	registrations := validatorCount + len(public.Managers)
	return uint64(registrations)*initialBalance + creationFeeReserve
}

func formatAVAX(amount uint64) string {
	return fmt.Sprintf("%d.%09d AVAX", amount/units.Avax, amount%units.Avax)
}

func printPublic(public Public, path, digest string) {
	fmt.Printf("public chain inputs: %s sha256:%s\n", path, digest)
	fmt.Printf("Genesis EVM address: %s\n", public.GenesisAddress)
	for _, manager := range public.Managers {
		fmt.Printf("management identity %s: %s weight %d\n", manager.Identity, manager.NodeID, manager.Weight)
	}
	for _, node := range public.Nodes {
		switch node.Role {
		case config.RoleValidator, config.RoleOracleValidator:
			fmt.Printf("%s identity %s: %s weight %d\n", node.ChainName(), node.Identity, node.NodeID, node.Weight)
		}
	}
	fmt.Printf("price feeder EVM address: %s\n", public.FeederAddress)
}

func create(
	ctx context.Context,
	environment config.Environment,
	outputDirectory string,
	root string,
	public Public,
	newWallet walletFactory,
) (Result, error) {
	keyBytes, err := hex.DecodeString(environment.FundingPrivateKey)
	if err != nil {
		return Result{}, fmt.Errorf("decode FUNDING_PRIVATE_KEY: %w", err)
	}
	fundingKey, err := secp256k1.ToPrivateKey(keyBytes)
	if err != nil {
		return Result{}, fmt.Errorf("load FUNDING_PRIVATE_KEY: %w", err)
	}
	mainTemplatePath := chainTemplatePath(root, config.MainChain)
	mainTemplate, err := os.ReadFile(mainTemplatePath)
	if err != nil {
		return Result{}, fmt.Errorf("read required genesis template %s: %w", mainTemplatePath, err)
	}
	genesisAddress := ethcommon.HexToAddress(public.GenesisAddress)
	createdAt := time.Now()
	// The management chain never runs, so its genesis stays contract-free even
	// when the main chain's genesis later embeds the oracle receiver.
	managementGenesis, err := RenderGenesis(mainTemplate, []ethcommon.Address{genesisAddress}, nil, nil, createdAt)
	if err != nil {
		return Result{}, err
	}
	hasOracle := public.HasOracle()
	feederAddress := ethcommon.HexToAddress(public.FeederAddress)
	// Every chain's genesis is BASE LAYER ONLY: funded accounts, consensus,
	// and network shape. App contracts never bake into it; they install onto
	// the running chain through the upgrade history
	// (playbooks/08-install-app.md), so adding an app never forces a chain
	// re-creation. The feeder address stays funded on every chain because a
	// balance is an allocation, not app state. Known exception, flagged for
	// the same treatment: the oracle-L1 shape below still bakes its Warp
	// receiver, whose seed needs the oracle chain ID that only exists
	// mid-creation.
	var oracleGenesis []byte
	if hasOracle {
		oracleTemplatePath := chainTemplatePath(root, config.OracleChain)
		oracleTemplate, err := os.ReadFile(oracleTemplatePath)
		if err != nil {
			return Result{}, fmt.Errorf("read required oracle genesis template %s: %w", oracleTemplatePath, err)
		}
		// The feeder also administers the oracle chain's FeeManager precompile,
		// so fee/delay parameters are tunable live without a chain recreation.
		oracleGenesis, err = RenderGenesis(
			oracleTemplate,
			[]ethcommon.Address{genesisAddress, feederAddress},
			[]ContractAllocation{{
				Address:     AggregatorAddress,
				RuntimeCode: oraclecontracts.AggregatorRuntime,
				Storage: map[ethcommon.Hash]ethcommon.Hash{
					{}: ethcommon.BytesToHash(feederAddress.Bytes()),
				},
			}},
			&feederAddress,
			createdAt,
		)
		if err != nil {
			return Result{}, err
		}
	}
	identities, err := public.IdentitySet()
	if err != nil {
		return Result{}, fmt.Errorf("load public identities: %w", err)
	}
	// evmChains is every chain this creation converts against the management
	// chain, main first. The oracle chain is excluded: its creation stays a
	// dedicated block below because the main genesis needs its chain ID.
	var evmChains []string
	for _, chain := range public.Chains() {
		if chain != config.OracleChain {
			evmChains = append(evmChains, chain)
		}
	}
	// The check covers every chain, the oracle chain included: it also
	// serves the shared funded addresses.
	if err := validateDistinctChainIDs(root, public.Chains()); err != nil {
		return Result{}, err
	}
	genesisPath := filepath.Join(outputDirectory, "genesis.json")
	oracleGenesisPath := filepath.Join(outputDirectory, "genesis-oracle.json")
	if err := requireMissing(oracleGenesisPath); err != nil {
		return Result{}, err
	}
	genesisByChain := make(map[string][]byte, len(evmChains))
	for _, chain := range evmChains {
		path := filepath.Join(outputDirectory, genesisFileName(chain))
		if err := requireMissing(path); err != nil {
			return Result{}, err
		}
		if chain == config.MainChain && hasOracle {
			// The main genesis embeds the oracle receiver, so it can only be
			// rendered after the oracle CreateChainTx is accepted.
			continue
		}
		template := mainTemplate
		if chain != config.MainChain {
			templatePath := chainTemplatePath(root, chain)
			if template, err = os.ReadFile(templatePath); err != nil {
				return Result{}, fmt.Errorf("read required genesis template %s: %w", templatePath, err)
			}
		}
		// A genesis with no chain-dependent content is published before the
		// first transaction, exactly as before.
		genesis, err := RenderGenesis(template, []ethcommon.Address{genesisAddress, feederAddress}, nil, nil, createdAt)
		if err != nil {
			return Result{}, fmt.Errorf("render %s genesis: %w", chain, err)
		}
		if err := os.WriteFile(path, genesis, 0o644); err != nil {
			return Result{}, fmt.Errorf("write generated genesis %s: %w", path, err)
		}
		fmt.Printf("generated %s\n", path)
		genesisByChain[chain] = genesis
	}

	state := State{
		Path:                       filepath.Join(outputDirectory, "network.env"),
		Network:                    environment.Network,
		Chains:                     make(map[string]ChainRecord),
		ManagerAddress:             managerAddress.Hex(),
		GenesisEVMAddress:          public.GenesisAddress,
		FeederEVMAddress:           public.FeederAddress,
		PriceFeedAddress:           PriceFeedAddress.Hex(),
		PriceFeedAggregatorAddress: PriceFeedAggregatorAddress.Hex(),
	}
	if hasOracle {
		state.OracleAggregatorAddress = AggregatorAddress.Hex()
		state.OracleReceiverAddress = ReceiverAddress.Hex()
	}
	if err := state.Save(); err != nil {
		return Result{}, err
	}

	keychain := secp256k1fx.NewKeychain(fundingKey)
	issue := func(action string, build func(pwallet.Wallet) (*txs.Tx, error)) (*txs.Tx, error) {
		return issueTx(ctx, action, environment.PChainAPI, keychain, &state, newWallet, build)
	}
	subnetOwner := &secp256k1fx.OutputOwners{Threshold: 1, Addrs: []ids.ShortID{fundingKey.Address()}}
	validatorOwner := warpmessage.PChainOwner{Threshold: 1, Addresses: []ids.ShortID{fundingKey.Address()}}

	fmt.Println("creating manager subnet")
	managerSubnetTx, err := issue("manager CreateSubnetTx", func(w pwallet.Wallet) (*txs.Tx, error) {
		return w.IssueCreateSubnetTx(subnetOwner)
	})
	if err != nil {
		return Result{}, err
	}
	state.ManagerSubnetID = managerSubnetTx.ID()
	fmt.Printf("accepted manager CreateSubnetTx %s\n", managerSubnetTx.ID())
	if err := state.Save(); err != nil {
		return Result{}, err
	}

	fmt.Println("creating management chain")
	managerChainTx, err := issue("manager CreateChainTx", func(w pwallet.Wallet) (*txs.Tx, error) {
		return w.IssueCreateChainTx(state.ManagerSubnetID, managementGenesis, constants.SubnetEVMID, nil, "management")
	})
	if err != nil {
		return Result{}, err
	}
	state.ManagerChainID = managerChainTx.ID()
	fmt.Printf("accepted manager CreateChainTx %s\n", managerChainTx.ID())
	if err := state.Save(); err != nil {
		return Result{}, err
	}

	managerWeights := make(map[string]uint64, len(public.Managers))
	for _, manager := range public.Managers {
		managerWeights[manager.Identity] = manager.Weight
	}
	managerValidators, err := conversionValidators(identities.Manager, func(generated identity.Identity) uint64 {
		return managerWeights[generated.Name]
	}, validatorOwner)
	if err != nil {
		return Result{}, err
	}
	fmt.Println("converting manager subnet to a self-managed L1")
	managerConvertTx, err := issue("manager ConvertSubnetToL1Tx", func(w pwallet.Wallet) (*txs.Tx, error) {
		return w.IssueConvertSubnetToL1Tx(
			state.ManagerSubnetID,
			state.ManagerChainID,
			managerAddress.Bytes(),
			managerValidators,
		)
	})
	if err != nil {
		return Result{}, err
	}
	state.ManagerConvertTxID = managerConvertTx.ID()
	fmt.Printf("accepted manager ConvertSubnetToL1Tx %s\n", managerConvertTx.ID())
	if err := state.Save(); err != nil {
		return Result{}, err
	}

	if hasOracle {
		if err := os.WriteFile(oracleGenesisPath, oracleGenesis, 0o644); err != nil {
			return Result{}, fmt.Errorf("write generated oracle genesis %s: %w", oracleGenesisPath, err)
		}
		fmt.Printf("generated %s\n", oracleGenesisPath)
		fmt.Println("creating oracle subnet")
		oracleSubnetTx, err := issue("oracle CreateSubnetTx", func(w pwallet.Wallet) (*txs.Tx, error) {
			return w.IssueCreateSubnetTx(subnetOwner)
		})
		if err != nil {
			return Result{}, err
		}
		state.OracleSubnetID = oracleSubnetTx.ID()
		fmt.Printf("accepted oracle CreateSubnetTx %s\n", oracleSubnetTx.ID())
		if err := state.Save(); err != nil {
			return Result{}, err
		}

		fmt.Println("creating oracle chain")
		oracleChainTx, err := issue("oracle CreateChainTx", func(w pwallet.Wallet) (*txs.Tx, error) {
			return w.IssueCreateChainTx(state.OracleSubnetID, oracleGenesis, constants.SubnetEVMID, nil, "oracle")
		})
		if err != nil {
			return Result{}, err
		}
		state.OracleChainID = oracleChainTx.ID()
		fmt.Printf("accepted oracle CreateChainTx %s\n", oracleChainTx.ID())
		if err := state.Save(); err != nil {
			return Result{}, err
		}

		oracleValidatorIdentities := make([]identity.Identity, 0, len(identities.Nodes))
		for _, generated := range identities.Nodes {
			if generated.Role == config.RoleOracleValidator {
				oracleValidatorIdentities = append(oracleValidatorIdentities, generated)
			}
		}
		oracleWeights := make(map[string]uint64, len(oracleValidatorIdentities))
		for _, node := range public.Nodes {
			if node.Role == config.RoleOracleValidator {
				oracleWeights[node.Identity] = node.Weight
			}
		}
		oracleValidators, err := conversionValidators(oracleValidatorIdentities, func(generated identity.Identity) uint64 {
			return oracleWeights[generated.Name]
		}, validatorOwner)
		if err != nil {
			return Result{}, err
		}
		fmt.Println("converting oracle subnet to an L1 managed by the management chain")
		oracleConvertTx, err := issue("oracle ConvertSubnetToL1Tx", func(w pwallet.Wallet) (*txs.Tx, error) {
			return w.IssueConvertSubnetToL1Tx(
				state.OracleSubnetID,
				state.ManagerChainID,
				managerAddress.Bytes(),
				oracleValidators,
			)
		})
		if err != nil {
			return Result{}, err
		}
		state.OracleConvertTxID = oracleConvertTx.ID()
		fmt.Printf("accepted oracle ConvertSubnetToL1Tx %s\n", oracleConvertTx.ID())
		if err := state.Save(); err != nil {
			return Result{}, err
		}
	}

	if hasOracle {
		// The receiver contract only trusts Warp messages whose source is the
		// oracle chain, so the main genesis can be rendered only after the
		// oracle CreateChainTx is accepted and its blockchain ID is known.
		mainGenesis, err := RenderGenesis(
			mainTemplate,
			[]ethcommon.Address{genesisAddress, feederAddress},
			[]ContractAllocation{{
				Address:     ReceiverAddress,
				RuntimeCode: oraclecontracts.ReceiverRuntime,
				Storage: map[ethcommon.Hash]ethcommon.Hash{
					{}:                                  ethcommon.Hash(state.OracleChainID),
					ethcommon.BigToHash(ethcommon.Big1): ethcommon.BytesToHash(AggregatorAddress.Bytes()),
				},
			}},
			nil,
			createdAt,
		)
		if err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(genesisPath, mainGenesis, 0o644); err != nil {
			return Result{}, fmt.Errorf("write generated genesis %s: %w", genesisPath, err)
		}
		fmt.Printf("generated %s\n", genesisPath)
		genesisByChain[config.MainChain] = mainGenesis
	}

	// setRecord routes one chain's creation output into the state: main keeps
	// its named fields, every other chain its record. Saved after every
	// accepted transaction, exactly like the blocks above.
	setRecord := func(chain string, record ChainRecord) {
		if chain == config.MainChain {
			state.SubnetID, state.ChainID, state.ConvertTxID = record.SubnetID, record.ChainID, record.ConvertTxID
			return
		}
		state.Chains[chain] = record
	}
	for _, chain := range evmChains {
		// The main chain keeps its historical on-chain name.
		chainTxName := chain
		if chain == config.MainChain {
			chainTxName = "benchmark"
		}
		chainValidatorIdentities := make([]identity.Identity, 0, len(identities.Nodes))
		for _, generated := range identities.Nodes {
			if generated.Role == config.RoleValidator && generated.Chain == chain {
				chainValidatorIdentities = append(chainValidatorIdentities, generated)
			}
		}
		weightByIdentity := make(map[string]uint64, len(chainValidatorIdentities))
		for _, node := range public.Nodes {
			if node.Role == config.RoleValidator && node.ChainName() == chain {
				weightByIdentity[node.Identity] = node.Weight
			}
		}
		chainValidators, err := conversionValidators(chainValidatorIdentities, func(generated identity.Identity) uint64 {
			return weightByIdentity[generated.Name]
		}, validatorOwner)
		if err != nil {
			return Result{}, err
		}

		var record ChainRecord
		fmt.Printf("creating %s subnet\n", chain)
		subnetTx, err := issue(chain+" CreateSubnetTx", func(w pwallet.Wallet) (*txs.Tx, error) {
			return w.IssueCreateSubnetTx(subnetOwner)
		})
		if err != nil {
			return Result{}, err
		}
		record.SubnetID = subnetTx.ID()
		setRecord(chain, record)
		fmt.Printf("accepted %s CreateSubnetTx %s\n", chain, subnetTx.ID())
		if err := state.Save(); err != nil {
			return Result{}, err
		}

		fmt.Printf("creating %s chain\n", chain)
		chainTx, err := issue(chain+" CreateChainTx", func(w pwallet.Wallet) (*txs.Tx, error) {
			return w.IssueCreateChainTx(record.SubnetID, genesisByChain[chain], constants.SubnetEVMID, nil, chainTxName)
		})
		if err != nil {
			return Result{}, err
		}
		record.ChainID = chainTx.ID()
		setRecord(chain, record)
		fmt.Printf("accepted %s CreateChainTx %s\n", chain, chainTx.ID())
		if err := state.Save(); err != nil {
			return Result{}, err
		}

		fmt.Printf("converting %s subnet to an L1 managed by the management chain\n", chain)
		convertTx, err := issue(chain+" ConvertSubnetToL1Tx", func(w pwallet.Wallet) (*txs.Tx, error) {
			return w.IssueConvertSubnetToL1Tx(
				record.SubnetID,
				state.ManagerChainID,
				managerAddress.Bytes(),
				chainValidators,
			)
		})
		if err != nil {
			return Result{}, err
		}
		record.ConvertTxID = convertTx.ID()
		setRecord(chain, record)
		fmt.Printf("accepted %s ConvertSubnetToL1Tx %s\n", chain, convertTx.ID())
		if err := state.Save(); err != nil {
			return Result{}, err
		}
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

const issueAttempts = 5

// issueBackoff is a variable so tests can exhaust the retries without sleeping.
var issueBackoff = 3 * time.Second

// issueTx rebuilds the wallet before every attempt and retries a failed
// issuance.
//
// The P-chain wallet caches its UTXO set when it is constructed. After a few
// sequential issuances it can build a transaction that spends an output the
// network has already consumed, which fails verification with "failed to read
// consumed UTXO". Rebuilding before each attempt makes the retry a fresh build
// against current state rather than a replay of the same doomed transaction,
// so this both retries and removes the cause.
//
// A FAILED REBUILD is retried too, and the backoff doubles per attempt. The
// public API rate-limits bursts (429), creation issues its transactions in a
// burst, and creation is not resumable: a rebuild failure that exits the
// command strands the whole deployment over one throttled window (observed
// live 2026-08-05, trading CreateChainTx after eight accepted transactions).
func issueTx(
	ctx context.Context,
	action string,
	api string,
	keys keychain.Keychain,
	state *State,
	newWallet walletFactory,
	build func(pwallet.Wallet) (*txs.Tx, error),
) (*txs.Tx, error) {
	var lastErr error
	backoff := issueBackoff
	for attempt := 1; attempt <= issueAttempts; attempt++ {
		wallet, err := makeWallet(ctx, api, keys, *state, newWallet)
		if err == nil {
			var tx *txs.Tx
			if tx, err = build(wallet); err == nil {
				return tx, nil
			}
		}
		lastErr = err
		if attempt == issueAttempts {
			break
		}
		fmt.Printf("%s attempt %d/%d failed: %v\n", action, attempt, issueAttempts, err)
		fmt.Printf("  rebuilding the wallet UTXO set, retrying in %s\n", backoff)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return nil, fmt.Errorf("%s after %d attempts: %w", action, issueAttempts, lastErr)
}

func makeWallet(
	ctx context.Context,
	api string,
	keys keychain.Keychain,
	state State,
	newWallet walletFactory,
) (pwallet.Wallet, error) {
	subnetIDs := make([]ids.ID, 0, 3+len(state.Chains))
	if state.ManagerSubnetID != ids.Empty {
		subnetIDs = append(subnetIDs, state.ManagerSubnetID)
	}
	if state.OracleSubnetID != ids.Empty {
		subnetIDs = append(subnetIDs, state.OracleSubnetID)
	}
	if state.SubnetID != ids.Empty {
		subnetIDs = append(subnetIDs, state.SubnetID)
	}
	for _, name := range state.chainNames() {
		if record := state.Chains[name]; record.SubnetID != ids.Empty {
			subnetIDs = append(subnetIDs, record.SubnetID)
		}
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
