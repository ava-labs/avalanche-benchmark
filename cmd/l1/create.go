package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/platformvm/signer"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	warpmessage "github.com/ava-labs/avalanchego/vms/platformvm/warp/message"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	pwallet "github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	ethcommon "github.com/ava-labs/libevm/common"
	"github.com/joho/godotenv"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fujikey"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/netcfg"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/vset"
)

// managerAddress is the conversion-recorded validator manager: address 0x..01
// on the L1's OWN chain. No contract ever exists there; the P-chain only
// compares these bytes (and the chainID) against each warp message's source,
// and the L1's own validators (all our keys) are then the signing set.
var managerAddress = ethcommon.HexToAddress("0x0000000000000000000000000000000000000001")

type createOpts struct {
	balanceAvax          float64
	force                bool
	committee            int
	committeeBalanceAvax float64
	allowFragile         bool
}

// initialWeight is the constant conversion weight every validator is
// registered at. Weights are not inventory: the real distribution is applied
// right after creation via `l1 apply` (scenarios/00_healthy.sh), and from then
// on the on-chain weight is the sole truth. The committee is registered at
// this same weight (equal-weight is what makes the 67% quorum math clean).
const initialWeight = 1000

// defaultCommittee / defaultCommitteeBalance: the committee size and per-node
// deposit `l1 create` uses. Defined in netcfg so fuji-wallet budgets the same
// committee cost; see netcfg.DefaultCommittee for the 3-of-4 quorum rationale
// and the keep-it-funded hazard.
const (
	defaultCommittee        = netcfg.DefaultCommittee
	defaultCommitteeBalance = netcfg.DefaultCommitteeBalance
)

// planned is one validator to register: node name (the staking/l1/<name> key
// dir) and the identity material once ensured on disk.
type planned struct {
	name   string
	nodeID ids.NodeID
	pop    *signer.ProofOfPossession
}

// planValidators plans one registration per role=validator node in the
// inventory, all at initialWeight.
func planValidators(nodes []topo.Node) []planned {
	vals := topo.Validators(nodes)
	if len(vals) == 0 {
		fatalf("nodes.ini has no role=validator nodes: nothing to register")
	}
	out := make([]planned, len(vals))
	for i, n := range vals {
		out[i] = planned{name: n.Name}
	}
	return out
}

// progress is the resumable on-disk state (network.env). Values appear as
// their steps complete; a --force re-run skips anything already present, so
// no AVAX is ever re-spent.
type progress struct {
	path          string
	network       string
	subnet        ids.ID // main L1 subnet
	chain         ids.ID // main L1 chain
	manager       string // main L1 recorded manager address (0x..01)
	managerSubnet ids.ID // manager (committee) L1 subnet
	managerChain  ids.ID // manager (committee) L1 chain: the main L1's recorded manager chain
}

func loadProgress(path string) *progress {
	p := &progress{path: path}
	vars, err := godotenv.Read(path)
	if err != nil {
		return p // no network.env yet: fresh create
	}
	p.network = vars["NETWORK"]
	for _, f := range []struct {
		key string
		dst *ids.ID
	}{
		{"SUBNET_ID", &p.subnet},
		{"CHAIN_ID", &p.chain},
		{"MANAGER_SUBNET_ID", &p.managerSubnet},
		{"MANAGER_CHAIN_ID", &p.managerChain},
	} {
		if v := vars[f.key]; v != "" {
			if *f.dst, err = ids.FromString(v); err != nil {
				fatalf("parse %s in %s: %v", f.key, path, err)
			}
		}
	}
	p.manager = vars["MANAGER_ADDRESS"]
	return p
}

func (p *progress) save() {
	var b strings.Builder
	fmt.Fprintf(&b, "NETWORK=%s\n", p.network)
	for _, f := range []struct {
		key string
		val ids.ID
	}{
		{"SUBNET_ID", p.subnet},
		{"CHAIN_ID", p.chain},
		{"MANAGER_SUBNET_ID", p.managerSubnet},
		{"MANAGER_CHAIN_ID", p.managerChain},
	} {
		if f.val != ids.Empty {
			fmt.Fprintf(&b, "%s=%s\n", f.key, f.val)
		}
	}
	if p.manager != "" {
		fmt.Fprintf(&b, "MANAGER_ADDRESS=%s\n", p.manager)
	}
	if err := os.WriteFile(p.path, []byte(b.String()), 0o644); err != nil {
		fatalf("write %s: %v", p.path, err)
	}
}

// create is the ONLY registration point. It builds TWO L1s from the single
// wallet key:
//
//   - the MANAGER L1: its own subnet + a phantom chain (never deployed) +
//     ConvertSubnetToL1Tx registering an equal-weight signing COMMITTEE
//     (staking/manager/m<i>, BLS keys generated locally). The committee never
//     runs blocks; it exists only as P-chain BLS-key records whose 67%-quorum
//     signatures authorize the main L1's weight changes.
//   - the MAIN L1: its own subnet + chain + ConvertSubnetToL1Tx for the
//     fleet's role=validator nodes, but with the recorded manager chain set to
//     the MANAGER L1's blockchainID (address 0x..01). The P-chain then
//     verifies the main L1's weight messages against the MANAGER subnet's
//     validator set, which the committee keys can sign for.
//
// Both validator lists are frozen here forever; every later change is
// weight-only (set-weight / apply) on the main L1, signed by the committee.
func create(ctx context.Context, opts createOpts) {
	net := netcfg.Get()
	stakingDir := stakingDirPath()
	envPath := filepath.Join(filepath.Dir(stakingDir), "network.env")

	prog := loadProgress(envPath)
	if prog.subnet != ids.Empty && !opts.force {
		fatalf("%s already records SUBNET_ID=%s: this deploy has a chain.\n"+
			"    Re-run with --force to resume/verify it (never re-spends completed steps),\n"+
			"    or delete network.env to create a NEW chain (the old one becomes unreachable).",
			envPath, prog.subnet)
	}
	if prog.network != "" && prog.network != net.Name {
		fatalf("%s records NETWORK=%s but the environment resolves to %s; delete network.env to switch networks", envPath, prog.network, net.Name)
	}
	prog.network = net.Name

	if opts.committee < 4 && !opts.allowFragile {
		fatalf("--committee %d cannot tolerate a signer loss at the strict 67%% warp quorum:\n"+
			"    a %d-validator equal-weight committee reaches quorum only when ALL %d sign\n"+
			"    (N=3 needs 3-of-3; 2-of-3 = 66.67%% < 67%% fails). Use --committee 4 (3-of-4 =\n"+
			"    75%%, survives one loss), or pass --allow-fragile-committee to override.",
			opts.committee, opts.committee, opts.committee)
	}
	if opts.committee < 1 {
		fatalf("--committee must be >= 1")
	}

	balance := net.ValidatorBalance
	if opts.balanceAvax > 0 {
		balance = uint64(opts.balanceAvax * float64(units.Avax))
	}
	committeeBalance := uint64(defaultCommitteeBalance)
	if opts.committeeBalanceAvax > 0 {
		committeeBalance = uint64(opts.committeeBalanceAvax * float64(units.Avax))
	}

	// Identities: the main L1's fleet validators (nodes.ini role=validator,
	// staking/l1/<name>, recorded in node-ids.env) and the manager L1's
	// committee (staking/manager/m<i>, NOT fleet inventory). Both
	// generate-if-absent.
	nodes, err := topo.LoadNear()
	if err != nil {
		fatalf("%v (`l1 create` registers the inventory's role=validator nodes)", err)
	}
	mainPlans := ensureIdentities(stakingDir, planValidators(nodes))
	committeePlans := ensureCommittee(stakingDir, opts.committee)

	fmt.Printf("=== l1 create on %s (SPENDS AVAX; resumable via %s) ===\n", net.Name, envPath)
	fmt.Printf("  P-chain API: %s\n", net.API)
	fmt.Printf("  Manager L1 committee: %d validators, equal weight %d, %s AVAX each (signs the main L1's weight changes; keep it funded)\n",
		len(committeePlans), initialWeight, avaxString(committeeBalance))
	for _, p := range committeePlans {
		fmt.Printf("    %-6s key=staking/manager/%-4s %s\n", p.name, p.name, p.nodeID)
	}
	fmt.Printf("  Main L1 validators: %d, %s AVAX each, all at initial weight %d\n", len(mainPlans), avaxString(balance), initialWeight)
	for _, p := range mainPlans {
		fmt.Printf("    %-6s key=staking/l1/%-6s %s\n", p.name, p.name, p.nodeID)
	}

	walletKey, err := fujikey.Load(filepath.Join(stakingDir, "fuji-wallet.key"))
	if err != nil {
		fatalf("load fee wallet key (run ./setup/00_gen_secrets.sh and ./setup/01_fund_wallet.sh first): %v", err)
	}
	owner := warpmessage.PChainOwner{Threshold: 1, Addresses: []ids.ShortID{walletKey.Address()}}
	subnetOwner := &secp256k1fx.OutputOwners{Threshold: 1, Addrs: []ids.ShortID{walletKey.Address()}}

	// Pre-flight the wallet BEFORE any tx is issued: both L1s' deposits plus a
	// margin for the six subnet/chain/convert txs. Gate on the manager subnet
	// (created first): once anything is on-chain this is a resume, where part
	// of the budget is already spent and the check would falsely over-demand.
	pc := platformvm.NewClient(net.API)
	if prog.managerSubnet == ids.Empty {
		needed := uint64(len(mainPlans))*balance + uint64(len(committeePlans))*committeeBalance + 200*units.MilliAvax
		balResp, err := pc.GetBalance(ctx, []ids.ShortID{walletKey.Address()})
		if err != nil {
			fatalf("platform.getBalance: %v", err)
		}
		if uint64(balResp.Balance) < needed {
			fatalf("P-chain balance %s AVAX < %s AVAX needed (%d main x %s + %d committee x %s + fees); run ./setup/01_fund_wallet.sh",
				avaxString(uint64(balResp.Balance)), avaxString(needed), len(mainPlans), avaxString(balance), len(committeePlans), avaxString(committeeBalance))
		}
	}

	genesisBytes := loadGenesis(walletKey.EthAddress())

	fmt.Println("Creating P-chain wallet...")
	kc := secp256k1fx.NewKeychain(walletKey)
	wallet, err := primary.MakePWallet(ctx, net.API, kc, primary.WalletConfig{})
	if err != nil {
		fatalf("make P-chain wallet: %v", err)
	}

	// --- MANAGER L1: subnet + phantom chain + committee conversion --------
	// Recorded manager chain = its OWN chain (self-managed); we never move the
	// committee's weights, so this is a formality.
	fmt.Println("=== Manager L1 (signing committee) ===")
	wallet = createSubnet(ctx, wallet, kc, net, "manager", subnetOwner, &prog.managerSubnet, prog)
	wallet = createChain(ctx, wallet, genesisBytes, "manager", prog.managerSubnet, &prog.managerChain, prog)
	convertToL1(ctx, pc, wallet, "manager", prog.managerSubnet, prog.managerChain, committeePlans, committeeBalance, owner)

	// --- MAIN L1: subnet + chain + fleet conversion -----------------------
	// Recorded manager chain = the MANAGER L1's chain: weight messages are
	// verified against the committee's validator set, which we can sign for.
	fmt.Println("=== Main L1 (fleet) ===")
	wallet = createSubnet(ctx, wallet, kc, net, "main", subnetOwner, &prog.subnet, prog)
	wallet = createChain(ctx, wallet, genesisBytes, "main", prog.subnet, &prog.chain, prog)
	convertToL1(ctx, pc, wallet, "main", prog.subnet, prog.managerChain, mainPlans, balance, owner)

	prog.manager = managerAddress.Hex()
	prog.save()

	fmt.Println()
	fmt.Println("=== L1 created ===")
	fmt.Printf("Main    subnet %s\n        chain  %s\n", prog.subnet, prog.chain)
	fmt.Printf("Manager subnet %s\n        chain  %s (its committee signs weight changes)\n", prog.managerSubnet, prog.managerChain)
	fmt.Printf("Manager address: %s (recorded on the main L1; the committee's keys authorize weight moves)\n", prog.manager)
	fmt.Printf("Registered main-L1 validators (all at weight %d; conversion order = validationID index):\n", initialWeight)
	for i, p := range mainPlans {
		fmt.Printf("  [%d] %-6s %s  validationID=%s\n",
			i, p.name, p.nodeID, prog.subnet.Append(uint32(i)))
	}
	fmt.Printf("Saved to %s. Next: ./setup/03_backup_secrets.sh, then ./run/01_deploy.sh\n", envPath)
}

// createSubnet creates the subnet into *dst if not already recorded (resume),
// saves progress, and returns a wallet re-synced with every subnet recorded so
// far so it can sign subnet-authorized txs.
func createSubnet(ctx context.Context, wallet pwallet.Wallet, kc *secp256k1fx.Keychain, net netcfg.Config, label string, owner *secp256k1fx.OutputOwners, dst *ids.ID, prog *progress) pwallet.Wallet {
	if *dst == ids.Empty {
		fmt.Printf("  Creating %s subnet...\n", label)
		subnetTx, err := wallet.IssueCreateSubnetTx(owner)
		if err != nil {
			fatalf("%s CreateSubnetTx: %v", label, err)
		}
		*dst = subnetTx.ID()
		prog.save()
	} else {
		fmt.Printf("  %s subnet already created (resume)\n", label)
	}
	fmt.Printf("  %s subnet: %s\n", label, *dst)

	w, err := primary.MakePWallet(ctx, net.API, kc, primary.WalletConfig{SubnetIDs: knownSubnets(prog)})
	if err != nil {
		fatalf("re-sync wallet: %v", err)
	}
	return w
}

// createChain creates the subnet-evm chain into *dst if not already recorded
// (resume) and saves progress. The manager L1's chain is phantom: it is never
// deployed, its genesis is irrelevant, it exists only to be a valid recorded
// manager-chain reference.
func createChain(ctx context.Context, wallet pwallet.Wallet, genesisBytes []byte, label string, subnetID ids.ID, dst *ids.ID, prog *progress) pwallet.Wallet {
	if *dst == ids.Empty {
		fmt.Printf("  Creating %s chain...\n", label)
		chainTx, err := wallet.IssueCreateChainTx(subnetID, genesisBytes, constants.SubnetEVMID, nil, "benchmarkchain")
		if err != nil {
			fatalf("%s CreateChainTx: %v", label, err)
		}
		*dst = chainTx.ID()
		prog.save()
	} else {
		fmt.Printf("  %s chain already created (resume)\n", label)
	}
	fmt.Printf("  %s chain: %s\n", label, *dst)
	return wallet
}

// convertToL1 issues the ConvertSubnetToL1Tx registering plans as the subnet's
// L1 validators (equal initialWeight, balance each), recording managerChainID
// + address 0x..01 as the validator manager. Resumes cleanly: a subnet already
// converted is left alone.
func convertToL1(ctx context.Context, pc *platformvm.Client, wallet pwallet.Wallet, label string, subnetID, managerChainID ids.ID, plans []planned, balance uint64, owner warpmessage.PChainOwner) {
	// The conversion tx sorts validators by NodeID bytes; conversion index
	// (and thus validationID = subnetID.Append(index)) follows that order.
	sort.Slice(plans, func(a, b int) bool { return plans[a].nodeID.Compare(plans[b].nodeID) < 0 })

	if converted(ctx, pc, subnetID) {
		fmt.Printf("  %s subnet already converted (resume)\n", label)
		return
	}
	fmt.Printf("  Converting %s subnet to L1 (manager chain %s, address %s)...\n", label, managerChainID, managerAddress.Hex())
	txValidators := make([]*txs.ConvertSubnetToL1Validator, len(plans))
	for i, p := range plans {
		txValidators[i] = &txs.ConvertSubnetToL1Validator{
			NodeID:                p.nodeID.Bytes(),
			Weight:                initialWeight,
			Balance:               balance,
			Signer:                *p.pop,
			RemainingBalanceOwner: owner,
			DeactivationOwner:     owner,
		}
	}
	if _, err := wallet.IssueConvertSubnetToL1Tx(subnetID, managerChainID, managerAddress.Bytes(), txValidators); err != nil {
		fatalf("%s ConvertSubnetToL1Tx: %v", label, err)
	}
	// The wallet waits for acceptance, but the public API is load-balanced:
	// give the backends a beat before anything queries the new L1 state.
	fmt.Println("  Conversion accepted; settling...")
	time.Sleep(5 * time.Second)
}

// knownSubnets is the subnets recorded so far, for wallet re-sync.
func knownSubnets(p *progress) []ids.ID {
	var out []ids.ID
	for _, id := range []ids.ID{p.managerSubnet, p.subnet} {
		if id != ids.Empty {
			out = append(out, id)
		}
	}
	return out
}

// ensureIdentities makes sure every planned staking/l1/<name> identity exists
// (generating missing VALIDATOR identities, BLS signer key included), loads
// each BLS signer's proof of possession, and (re)writes the manifest: planned
// nodes get their lines, any other committed identities (the rpc tier's,
// generated by setup/00_gen_secrets.sh) keep theirs.
func ensureIdentities(stakingDir string, plans []planned) []planned {
	if err := vset.CheckNamedKeyDirs(stakingDir); err != nil {
		fatalf("%v", err)
	}
	existing := map[string]vset.Entry{}
	if entries, err := vset.ReadManifest(stakingDir); err == nil {
		for _, e := range entries {
			existing[e.Name] = e
		}
	}
	for i := range plans {
		p := &plans[i]
		dir := filepath.Join(stakingDir, "l1", p.name)
		if dirExists(dir) {
			id, err := vset.NodeIDFromCertFile(filepath.Join(dir, "staker.crt"))
			if err != nil {
				fatalf("%v", err)
			}
			p.nodeID = id
			if e, ok := existing[p.name]; ok && e.NodeID != id {
				fatalf("staking/l1/%s staker.crt yields %s but node-ids.env says %s: manifest and keys disagree, fix staking/ before creating a chain", p.name, id, e.NodeID)
			}
		} else {
			id, err := vset.GenerateIdentity(stakingDir, "l1", p.name, true)
			if err != nil {
				fatalf("%v", err)
			}
			fmt.Printf("  generated identity staking/l1/%s (%s)\n", p.name, id)
			p.nodeID = id
		}
		existing[p.name] = vset.Entry{Name: p.name, NodeID: p.nodeID}

		pop, err := loadPoP(dir)
		if err != nil {
			fatalf("staking/l1/%s: %v", p.name, err)
		}
		p.pop = pop
	}
	entries := make([]vset.Entry, 0, len(existing))
	for _, e := range existing {
		entries = append(entries, e)
	}
	if err := vset.WriteManifest(stakingDir, entries); err != nil {
		fatalf("write node-ids.env: %v", err)
	}
	return plans
}

// ensureCommittee makes sure the n manager-L1 committee identities exist under
// staking/manager/m<i> (generate-if-absent, BLS signer key included) and loads
// each PoP. The committee is NOT fleet inventory: it is never in nodes.ini nor
// the node-ids.env manifest, and its nodes never run - they exist only as
// P-chain BLS-key records whose signatures move the MAIN L1's weights. On a
// --force resume it reuses the existing dirs unchanged (they may already be
// registered on-chain).
func ensureCommittee(stakingDir string, n int) []planned {
	out := make([]planned, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("m%d", i+1)
		dir := filepath.Join(stakingDir, "manager", name)
		var (
			id  ids.NodeID
			err error
		)
		if dirExists(dir) {
			if id, err = vset.NodeIDFromCertFile(filepath.Join(dir, "staker.crt")); err != nil {
				fatalf("%v", err)
			}
		} else {
			if id, err = vset.GenerateIdentity(stakingDir, "manager", name, true); err != nil {
				fatalf("%v", err)
			}
			fmt.Printf("  generated committee identity staking/manager/%s (%s)\n", name, id)
		}
		pop, err := loadPoP(dir)
		if err != nil {
			fatalf("staking/manager/%s: %v", name, err)
		}
		out[i] = planned{name: name, nodeID: id, pop: pop}
	}
	return out
}

// loadPoP loads the BLS signer.key in dir and builds its proof of possession.
func loadPoP(dir string) (*signer.ProofOfPossession, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "signer.key"))
	if err != nil {
		return nil, fmt.Errorf("read signer.key: %w", err)
	}
	sk, err := localsigner.FromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("load signer.key: %w", err)
	}
	return signer.NewProofOfPossession(sk)
}

// converted probes the first conversion validationID: present means the
// ConvertSubnetToL1Tx landed (all conversion validators appear atomically).
func converted(ctx context.Context, pc *platformvm.Client, subnetID ids.ID) bool {
	_, _, err := pc.GetL1Validator(ctx, subnetID.Append(0))
	if err == nil {
		return true
	}
	if strings.Contains(err.Error(), "not found") {
		return false
	}
	fatalf("platform.getL1Validator: %v", err)
	panic("unreachable")
}

// loadGenesis reads the committed genesis.json template (exe-relative, else
// cwd) and rewrites its ewoq placeholder prefund to the deploy wallet.
func loadGenesis(walletAddr ethcommon.Address) []byte {
	path := "genesis.json"
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), "..", "genesis.json"); fileExists(p) {
			path = p
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fatalf("read genesis template: %v", err)
	}
	out, err := templateGenesis(raw, walletAddr)
	if err != nil {
		fatalf("template genesis: %v", err)
	}
	fmt.Printf("  Genesis prefund: %s (deploy wallet; ewoq placeholder replaced)\n", walletAddr.Hex())
	return out
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
