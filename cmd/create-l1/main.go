// Command create-l1 creates the Fuji-anchored benchmark L1 with a
// C-chain-managed validator set:
//
//  1. create subnet + chain on the Fuji P-chain,
//  2. deploy + initialize a stock icm-contracts v2 ValidatorManager on the
//     Fuji C-chain (churnPeriodSeconds=0, so churn degrades to a per-op 20%
//     size cap and DC failover works as a pure weight seesaw),
//  3. convert the subnet to an L1 with the manager (C-chain blockchainID,
//     contract address) pair and ALL staking slots of BOTH sites registered
//     as validators (active site's validator slots at ActiveWeight, everything
//     else at StandbyWeight): validators are registered exactly once, here;
//     every later change is weight-only through the contract,
//  4. replay the conversion data into the contract (initializeValidatorSet).
//
// Every step's result is persisted to the -output file the moment it exists,
// and every step is skipped when its result is already present, so a run that
// dies anywhere (aggregator hiccup, C-chain congestion) is resumed by simply
// re-running the command: no AVAX is stranded.
package main

import (
	"context"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/api/info"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/staking"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/platformvm/signer"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	warpmessage "github.com/ava-labs/avalanchego/vms/platformvm/warp/message"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	ethcommon "github.com/ava-labs/libevm/common"
	"github.com/joho/godotenv"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fujikey"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

var (
	outputFile string
	deployOnly bool
)

const (
	// defaultPChainAPI is the public Fuji API. Our own RPC tier is follow-only,
	// so its platform.* API is gated forever: P-chain txs MUST go against a
	// public node (FUJI_PLAN.md gotchas). Override with PCHAIN_API in .env.
	defaultPChainAPI = "https://api.avax-test.network"
	// validatorBalance is the per-validator continuous-fee deposit paid by the
	// conversion tx. 0.1 AVAX lasts ~5-6 days on Fuji, plenty per run; balance 0
	// makes the validator INACTIVE and takes the whole L1 down. Top up any time
	// with IncreaseL1ValidatorBalanceTx (anyone can fund, no owner auth needed).
	validatorBalance = 100 * units.MilliAvax
	// cChainGasBudget is the minimum C-chain balance (wei) required before we
	// start: ValidatorManager deploy (~2.5M gas) + initialize +
	// initializeValidatorSet + headroom for later weight ops.
	cChainGasBudgetAvax = 0.15
)

func main() {
	flag.StringVar(&outputFile, "output", "", "Persist/resume SUBNET_ID, CHAIN_ID, MANAGER_ADDRESS in this file")
	flag.BoolVar(&deployOnly, "deploy-only", false, "Smoke test: deploy+initialize a throwaway ValidatorManager on Fuji C, read back settings, exit")
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// progress is the resumable on-disk state (the -output file). Values appear as
// their steps complete; a re-run skips anything already present.
type progress struct {
	path    string
	subnet  ids.ID
	chain   ids.ID
	manager ethcommon.Address
}

func loadProgress(path string) (*progress, error) {
	p := &progress{path: path}
	if path == "" {
		return p, nil
	}
	vars, err := godotenv.Read(path)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if v := vars["SUBNET_ID"]; v != "" {
		if p.subnet, err = ids.FromString(v); err != nil {
			return nil, fmt.Errorf("parse SUBNET_ID in %s: %w", path, err)
		}
	}
	if v := vars["CHAIN_ID"]; v != "" {
		if p.chain, err = ids.FromString(v); err != nil {
			return nil, fmt.Errorf("parse CHAIN_ID in %s: %w", path, err)
		}
	}
	if v := vars["MANAGER_ADDRESS"]; v != "" {
		p.manager = ethcommon.HexToAddress(v)
	}
	return p, nil
}

func (p *progress) save() error {
	if p.path == "" {
		return nil
	}
	var b strings.Builder
	if p.subnet != ids.Empty {
		fmt.Fprintf(&b, "SUBNET_ID=%s\n", p.subnet)
	}
	if p.chain != ids.Empty {
		fmt.Fprintf(&b, "CHAIN_ID=%s\n", p.chain)
	}
	if p.manager != (ethcommon.Address{}) {
		fmt.Fprintf(&b, "MANAGER_ADDRESS=%s\n", p.manager.Hex())
	}
	if err := os.WriteFile(p.path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", p.path, err)
	}
	return nil
}

// stakingValidator carries one staking slot's registration material plus its
// position bookkeeping (slot and conversion order).
type stakingValidator struct {
	slot   int // pool slot index
	key    int // committed staking key index
	nodeID ids.NodeID
	pop    *signer.ProofOfPossession
	weight uint64
}

func run() error {
	envPath := findEnvFile()
	if envPath == "" {
		return fmt.Errorf(".env file not found")
	}
	if err := godotenv.Load(envPath); err != nil {
		return fmt.Errorf("failed to load .env: %w", err)
	}

	pchainURI := defaultPChainAPI
	if v := os.Getenv("PCHAIN_API"); v != "" {
		pchainURI = v
	}
	cChainRPC := pchainURI + "/ext/bc/C/rpc"

	stakingDir := findStakingDir()
	if stakingDir == "" {
		return fmt.Errorf("staking directory not found")
	}
	walletKey, err := fujikey.Load(filepath.Join(stakingDir, "fuji-wallet.key"))
	if err != nil {
		return fmt.Errorf("load Fuji wallet key (run ./setup/00_gen_secrets.sh and ./setup/01_fund_wallet.sh first): %w", err)
	}

	ctx := context.Background()

	if deployOnly {
		return deployOnlySmoke(ctx, cChainRPC, walletKey)
	}

	t, poolIPs, err := topo.FromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("topology from .env: %w", err)
	}
	stakingSlots := t.StakingSlots()

	prog, err := loadProgress(outputFile)
	if err != nil {
		return err
	}

	fmt.Println("=== Create L1 (on Fuji: this SPENDS AVAX, resumable via -output) ===")
	fmt.Printf("  P-chain API: %s\n", pchainURI)
	fmt.Printf("  Registered validators: %d staking slots (both sites; RPC slots excluded)\n", len(stakingSlots))
	for _, s := range stakingSlots {
		fmt.Printf("    %-3s %-15s key=staking/l1/%d weight=%d\n",
			t.MachineName(s), poolIPs[s], t.KeyOf(s), slotWeight(t, s))
	}
	fmt.Println()

	// Pre-flight both balances so a short wallet fails BEFORE any tx is issued.
	pClient := platformvm.NewClient(pchainURI)
	neededP := uint64(len(stakingSlots))*validatorBalance + 100*units.MilliAvax
	balResp, err := pClient.GetBalance(ctx, []ids.ShortID{walletKey.Address()})
	if err != nil {
		return fmt.Errorf("platform.getBalance: %w", err)
	}
	if prog.subnet == ids.Empty && uint64(balResp.Balance) < neededP {
		return fmt.Errorf("P-chain balance %d nAVAX < %d nAVAX needed (%d validators x 0.1 AVAX + fees); run ./setup/01_fund_wallet.sh",
			balResp.Balance, neededP, len(stakingSlots))
	}
	cli, err := valmgr.Dial(ctx, cChainRPC, walletKey, prog.manager)
	if err != nil {
		return fmt.Errorf("dial C-chain: %w", err)
	}
	cBal, err := cli.Balance(ctx)
	if err != nil {
		return fmt.Errorf("C-chain balance: %w", err)
	}
	neededC := avaxToWei(cChainGasBudgetAvax)
	if prog.manager == (ethcommon.Address{}) && cBal.Cmp(neededC) < 0 {
		return fmt.Errorf("C-chain balance %s wei < %s wei needed for ValidatorManager gas (address %s); run ./setup/01_fund_wallet.sh",
			cBal, neededC, cli.Sender())
	}

	genesisPath := findGenesisFile()
	if genesisPath == "" {
		return fmt.Errorf("genesis.json not found")
	}
	genesisBytes, err := os.ReadFile(genesisPath)
	if err != nil {
		return fmt.Errorf("failed to read genesis: %w", err)
	}

	fmt.Println("[1/6] Creating P-chain wallet...")
	kc := secp256k1fx.NewKeychain(walletKey)
	wallet, err := primary.MakePWallet(ctx, pchainURI, kc, primary.WalletConfig{})
	if err != nil {
		return fmt.Errorf("failed to create wallet: %w", err)
	}

	if prog.subnet == ids.Empty {
		fmt.Println("[2/6] Creating subnet...")
		owner := &secp256k1fx.OutputOwners{
			Threshold: 1,
			Addrs:     []ids.ShortID{walletKey.Address()},
		}
		subnetTx, err := wallet.IssueCreateSubnetTx(owner)
		if err != nil {
			return fmt.Errorf("failed to create subnet: %w", err)
		}
		prog.subnet = subnetTx.ID()
		if err := prog.save(); err != nil {
			return err
		}
	} else {
		fmt.Println("[2/6] Subnet already created (resume)")
	}
	fmt.Printf("  Subnet ID: %s\n", prog.subnet)

	// Re-sync wallet with the subnet so it can sign subnet-authorized txs.
	wallet, err = primary.MakePWallet(ctx, pchainURI, kc, primary.WalletConfig{
		SubnetIDs: []ids.ID{prog.subnet},
	})
	if err != nil {
		return fmt.Errorf("failed to re-sync wallet: %w", err)
	}

	if prog.chain == ids.Empty {
		fmt.Println("[3/6] Creating chain...")
		chainTx, err := wallet.IssueCreateChainTx(
			prog.subnet,
			genesisBytes,
			constants.SubnetEVMID,
			nil,
			"benchmarkchain",
		)
		if err != nil {
			return fmt.Errorf("failed to create chain: %w", err)
		}
		prog.chain = chainTx.ID()
		if err := prog.save(); err != nil {
			return err
		}
	} else {
		fmt.Println("[3/6] Chain already created (resume)")
	}
	fmt.Printf("  Chain ID:  %s\n", prog.chain)

	if prog.manager == (ethcommon.Address{}) {
		fmt.Println("[4/6] Deploying ValidatorManager on the Fuji C-chain...")
		addr, err := cli.Deploy(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("  Deployed at %s; initializing (churnPeriod=0, maxChurn=20%%)...\n", addr.Hex())
		if err := cli.Initialize(ctx, cli.Sender(), prog.subnet); err != nil {
			return fmt.Errorf("initialize ValidatorManager: %w", err)
		}
		// Persisted only after initialize succeeds: a deploy whose initialize
		// failed is abandoned and a resume deploys a fresh contract.
		prog.manager = addr
		if err := prog.save(); err != nil {
			return err
		}
	} else {
		fmt.Println("[4/6] ValidatorManager already deployed (resume)")
		cli.Manager = prog.manager
	}
	fmt.Printf("  Manager:   %s\n", prog.manager.Hex())

	// The manager pair committed on-chain is (Fuji C-chain blockchainID,
	// contract address): the manager lives on the C-chain, NOT on our L1, so
	// recovery works even while the L1 itself is stalled.
	cChainID, err := info.NewClient(pchainURI).GetBlockchainID(ctx, "C")
	if err != nil {
		return fmt.Errorf("info.getBlockchainID(C): %w", err)
	}

	validators, err := buildStakingValidators(t, stakingDir)
	if err != nil {
		return err
	}
	// The conversion tx sorts validators by NodeID bytes; conversion index
	// (and thus validationID) follows that order. Sort once here and reuse the
	// exact order for the tx, the conversion hash, and the printed mapping.
	sort.Slice(validators, func(a, b int) bool {
		return validators[a].nodeID.Compare(validators[b].nodeID) < 0
	})

	converted, err := isConverted(ctx, pClient, prog.subnet)
	if err != nil {
		return err
	}
	if !converted {
		fmt.Println("[5/6] Converting subnet to L1 (manager = Fuji C-chain contract)...")
		txValidators := make([]*txs.ConvertSubnetToL1Validator, len(validators))
		owner := warpmessage.PChainOwner{
			Threshold: 1,
			Addresses: []ids.ShortID{walletKey.Address()},
		}
		for i, v := range validators {
			txValidators[i] = &txs.ConvertSubnetToL1Validator{
				NodeID:                v.nodeID.Bytes(),
				Weight:                v.weight,
				Balance:               validatorBalance,
				Signer:                *v.pop,
				RemainingBalanceOwner: owner,
				DeactivationOwner:     owner,
			}
		}
		if _, err := wallet.IssueConvertSubnetToL1Tx(
			prog.subnet,
			cChainID,
			prog.manager.Bytes(),
			txValidators,
		); err != nil {
			return fmt.Errorf("failed to convert subnet to L1: %w", err)
		}
		// The wallet waits for acceptance, but the public API is load-balanced:
		// give the fleet a beat before anything queries the new L1 state.
		fmt.Println("  Conversion accepted; settling...")
		time.Sleep(5 * time.Second)
	} else {
		fmt.Println("[5/6] Subnet already converted (resume)")
	}

	initialized, err := cli.IsValidatorSetInitialized(ctx)
	if err != nil {
		return fmt.Errorf("isValidatorSetInitialized: %w", err)
	}
	if !initialized {
		fmt.Println("[6/6] Replaying conversion into the contract (initializeValidatorSet)...")
		if err := initializeValidatorSet(ctx, cli, prog, cChainID, validators); err != nil {
			return err
		}
	} else {
		fmt.Println("[6/6] Validator set already initialized (resume)")
	}

	fmt.Println()
	fmt.Println("=== L1 Created Successfully ===")
	fmt.Println()
	fmt.Printf("Subnet ID:       %s\n", prog.subnet)
	fmt.Printf("Chain ID:        %s\n", prog.chain)
	fmt.Printf("Manager address: %s (Fuji C-chain)\n", prog.manager.Hex())
	fmt.Println()
	fmt.Println("Registered validators (conversion order = validationID index):")
	for i, v := range validators {
		fmt.Printf("  [%d] %-3s key=%d weight=%-10d %s  validationID=%s\n",
			i, t.MachineName(v.slot), v.key, v.weight, v.nodeID, valmgr.ValidationID(prog.subnet, uint32(i)))
	}
	return nil
}

// slotWeight is the conversion-time weight policy: the primary site's
// validator slots carry the consensus at ValidatorWeight, everything else
// (the standby site and hot spares) registers at SpareWeight.
func slotWeight(t topo.Topology, slot int) uint64 {
	if t.Site(slot) == topo.SiteA && t.IsValidatorSlot(slot) {
		return valmgr.ValidatorWeight
	}
	return valmgr.SpareWeight
}

// isConverted probes the first conversion validationID: present means the
// ConvertSubnetToL1Tx landed (all conversion validators appear atomically).
func isConverted(ctx context.Context, pClient *platformvm.Client, subnetID ids.ID) (bool, error) {
	_, _, err := pClient.GetL1Validator(ctx, valmgr.ValidationID(subnetID, 0))
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "not found") {
		return false, nil
	}
	return false, fmt.Errorf("platform.getL1Validator: %w", err)
}

func initializeValidatorSet(
	ctx context.Context,
	cli *valmgr.Client,
	prog *progress,
	cChainID ids.ID,
	validators []stakingValidator, // conversion (NodeID-sorted) order
) error {
	// Recompute the conversionID from the exact data the tx committed; the
	// contract recomputes it from our calldata and the P-chain signs it, so all
	// three views must be byte-identical.
	data := warpmessage.SubnetToL1ConversionData{
		SubnetID:       prog.subnet,
		ManagerChainID: cChainID,
		ManagerAddress: prog.manager.Bytes(),
		Validators:     make([]warpmessage.SubnetToL1ConversionValidatorData, len(validators)),
	}
	contractValidators := make([]valmgr.InitialValidator, len(validators))
	for i, v := range validators {
		data.Validators[i] = warpmessage.SubnetToL1ConversionValidatorData{
			NodeID:       v.nodeID.Bytes(),
			BLSPublicKey: v.pop.PublicKey,
			Weight:       v.weight,
		}
		contractValidators[i] = valmgr.InitialValidator{
			NodeID:       v.nodeID.Bytes(),
			BLSPublicKey: v.pop.PublicKey[:],
			Weight:       v.weight,
		}
	}
	conversionID, err := warpmessage.SubnetToL1ConversionID(data)
	if err != nil {
		return fmt.Errorf("compute conversionID: %w", err)
	}

	unsigned, err := valmgr.ConversionMessage(constants.FujiID, conversionID)
	if err != nil {
		return fmt.Errorf("build conversion message: %w", err)
	}
	fmt.Println("  Aggregating P-chain signatures over the conversion message...")
	signed, err := valmgr.Aggregate(ctx, unsigned, prog.subnet[:])
	if err != nil {
		return fmt.Errorf("aggregate conversion message: %w", err)
	}
	fmt.Println("  Delivering initializeValidatorSet...")
	if err := cli.InitializeValidatorSet(ctx, prog.subnet, cChainID, contractValidators, signed.Bytes()); err != nil {
		return fmt.Errorf("initializeValidatorSet: %w", err)
	}
	return nil
}

func buildStakingValidators(t topo.Topology, stakingDir string) ([]stakingValidator, error) {
	nodeIDsPath := filepath.Join(stakingDir, "node-ids.env")
	nodeIDs, err := godotenv.Read(nodeIDsPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", nodeIDsPath, err)
	}

	slots := t.StakingSlots()
	fmt.Printf("  Computing BLS PoPs for %d committed staking identities...\n", len(slots))
	validators := make([]stakingValidator, 0, len(slots))
	for _, s := range slots {
		key := t.KeyOf(s)
		nodeIDStr := strings.TrimSpace(nodeIDs[fmt.Sprintf("L1_%d_NODE_ID", key)])
		if nodeIDStr == "" {
			return nil, fmt.Errorf("missing L1_%d_NODE_ID in %s (run ./setup/00_gen_secrets.sh)", key, nodeIDsPath)
		}
		nodeID, err := ids.NodeIDFromString(nodeIDStr)
		if err != nil {
			return nil, fmt.Errorf("parse L1_%d_NODE_ID %q: %w", key, nodeIDStr, err)
		}

		keyDir := filepath.Join(stakingDir, "l1", strconv.Itoa(key))
		certNodeID, err := nodeIDFromCertFile(filepath.Join(keyDir, "staker.crt"))
		if err != nil {
			return nil, err
		}
		if certNodeID != nodeID {
			return nil, fmt.Errorf("staking/l1/%d staker.crt yields %s but node-ids.env says %s", key, certNodeID, nodeID)
		}

		skBytes, err := os.ReadFile(filepath.Join(keyDir, "signer.key"))
		if err != nil {
			return nil, fmt.Errorf("read staking/l1/%d signer.key: %w", key, err)
		}
		sk, err := localsigner.FromBytes(skBytes)
		if err != nil {
			return nil, fmt.Errorf("load staking/l1/%d signer.key: %w", key, err)
		}
		pop, err := signer.NewProofOfPossession(sk)
		if err != nil {
			return nil, fmt.Errorf("build PoP for staking/l1/%d: %w", key, err)
		}

		validators = append(validators, stakingValidator{
			slot:   s,
			key:    key,
			nodeID: nodeID,
			pop:    pop,
			weight: slotWeight(t, s),
		})
	}
	return validators, nil
}

// deployOnlySmoke proves the contract assets, ABI packing, gas handling and
// wallet against the real Fuji C-chain for ~0.1 AVAX, without touching the
// P-chain: deploy a throwaway instance, initialize it with a zero subnetID,
// read the settings back.
func deployOnlySmoke(ctx context.Context, cChainRPC string, key *secp256k1.PrivateKey) error {
	cli, err := valmgr.Dial(ctx, cChainRPC, key, ethcommon.Address{})
	if err != nil {
		return err
	}
	fmt.Printf("deploy-only smoke: sender %s\n", cli.Sender())
	addr, err := cli.Deploy(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  deployed throwaway ValidatorManager at %s\n", addr.Hex())
	if err := cli.Initialize(ctx, cli.Sender(), ids.Empty); err != nil {
		return err
	}
	period, err := cli.ChurnPeriodSeconds(ctx)
	if err != nil {
		return err
	}
	owner, err := cli.Owner(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("  read back: churnPeriodSeconds=%d owner=%s\n", period, owner.Hex())
	if period != 0 || owner != cli.Sender() {
		return fmt.Errorf("smoke mismatch: churnPeriodSeconds=%d owner=%s", period, owner.Hex())
	}
	fmt.Println("  OK (contract is throwaway; real deploy happens in the full run)")
	return nil
}

// avaxToWei converts an AVAX amount to C-chain wei (1 AVAX = 1e18 wei).
func avaxToWei(avax float64) *big.Int {
	i, _ := new(big.Float).Mul(big.NewFloat(avax), big.NewFloat(1e18)).Int(nil)
	return i
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
	exe, err := os.Executable()
	if err == nil {
		envPath := filepath.Join(filepath.Dir(exe), "..", ".env")
		if _, err := os.Stat(envPath); err == nil {
			return envPath
		}
	}
	if _, err := os.Stat(".env"); err == nil {
		return ".env"
	}
	return ""
}

func findGenesisFile() string {
	exe, err := os.Executable()
	if err == nil {
		genesisPath := filepath.Join(filepath.Dir(exe), "..", "genesis.json")
		if _, err := os.Stat(genesisPath); err == nil {
			return genesisPath
		}
	}
	if _, err := os.Stat("genesis.json"); err == nil {
		return "genesis.json"
	}
	return ""
}

func findStakingDir() string {
	exe, err := os.Executable()
	if err == nil {
		stakingPath := filepath.Join(filepath.Dir(exe), "..", "staking")
		if info, err := os.Stat(stakingPath); err == nil && info.IsDir() {
			return stakingPath
		}
	}
	if info, err := os.Stat("staking"); err == nil && info.IsDir() {
		return "staking"
	}
	return ""
}
