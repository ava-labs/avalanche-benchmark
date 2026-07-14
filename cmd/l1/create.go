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
	balanceAvax float64
	force       bool
}

// initialWeight is the constant conversion weight every validator is
// registered at. Weights are not inventory: the real distribution is applied
// right after creation via `l1 apply` (scenarios/00_healthy.sh), and from then
// on the on-chain weight is the sole truth.
const initialWeight = 1000

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
	path    string
	network string
	subnet  ids.ID
	chain   ids.ID
	manager string
}

func loadProgress(path string) *progress {
	p := &progress{path: path}
	vars, err := godotenv.Read(path)
	if err != nil {
		return p // no network.env yet: fresh create
	}
	p.network = vars["NETWORK"]
	if v := vars["SUBNET_ID"]; v != "" {
		if p.subnet, err = ids.FromString(v); err != nil {
			fatalf("parse SUBNET_ID in %s: %v", path, err)
		}
	}
	if v := vars["CHAIN_ID"]; v != "" {
		if p.chain, err = ids.FromString(v); err != nil {
			fatalf("parse CHAIN_ID in %s: %v", path, err)
		}
	}
	p.manager = vars["MANAGER_ADDRESS"]
	return p
}

func (p *progress) save() {
	var b strings.Builder
	fmt.Fprintf(&b, "NETWORK=%s\n", p.network)
	if p.subnet != ids.Empty {
		fmt.Fprintf(&b, "SUBNET_ID=%s\n", p.subnet)
	}
	if p.chain != ids.Empty {
		fmt.Fprintf(&b, "CHAIN_ID=%s\n", p.chain)
	}
	if p.manager != "" {
		fmt.Fprintf(&b, "MANAGER_ADDRESS=%s\n", p.manager)
	}
	if err := os.WriteFile(p.path, []byte(b.String()), 0o644); err != nil {
		fatalf("write %s: %v", p.path, err)
	}
}

// create is the ONLY registration point: node identities, subnet, chain,
// conversion. The validator list is frozen here forever; every later change
// is weight-only (set-weight / apply).
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

	balance := net.ValidatorBalance
	if opts.balanceAvax > 0 {
		balance = uint64(opts.balanceAvax * float64(units.Avax))
	}

	// [1] Node identities: the role=validator nodes of the inventory.
	// Generate whatever staking/l1/<name> is missing (generate-if-absent) and
	// (re)write the manifest.
	nodes, err := topo.LoadNear()
	if err != nil {
		fatalf("%v (`l1 create` registers the inventory's role=validator nodes)", err)
	}
	plans := ensureIdentities(stakingDir, planValidators(nodes))

	fmt.Printf("=== l1 create on %s (SPENDS AVAX; resumable via %s) ===\n", net.Name, envPath)
	fmt.Printf("  P-chain API: %s\n", net.API)
	fmt.Printf("  Validators: %d, %s AVAX continuous-fee balance each, all at initial weight %d\n", len(plans), avaxString(balance), initialWeight)
	for _, p := range plans {
		fmt.Printf("    %-6s key=staking/l1/%-6s %s\n", p.name, p.name, p.nodeID)
	}

	walletKey, err := fujikey.Load(filepath.Join(stakingDir, "fuji-wallet.key"))
	if err != nil {
		fatalf("load fee wallet key (run ./setup/00_gen_secrets.sh and ./setup/01_fund_wallet.sh first): %v", err)
	}

	// Pre-flight the wallet BEFORE any tx is issued.
	pc := platformvm.NewClient(net.API)
	if prog.subnet == ids.Empty {
		needed := uint64(len(plans))*balance + 100*units.MilliAvax
		balResp, err := pc.GetBalance(ctx, []ids.ShortID{walletKey.Address()})
		if err != nil {
			fatalf("platform.getBalance: %v", err)
		}
		if uint64(balResp.Balance) < needed {
			fatalf("P-chain balance %s AVAX < %s AVAX needed (%d validators x %s AVAX + fees); run ./setup/01_fund_wallet.sh",
				avaxString(uint64(balResp.Balance)), avaxString(needed), len(plans), avaxString(balance))
		}
	}

	genesisBytes := loadGenesis(walletKey.EthAddress())

	fmt.Println("[1/4] Creating P-chain wallet...")
	kc := secp256k1fx.NewKeychain(walletKey)
	wallet, err := primary.MakePWallet(ctx, net.API, kc, primary.WalletConfig{})
	if err != nil {
		fatalf("make P-chain wallet: %v", err)
	}

	if prog.subnet == ids.Empty {
		fmt.Println("[2/4] Creating subnet...")
		owner := &secp256k1fx.OutputOwners{Threshold: 1, Addrs: []ids.ShortID{walletKey.Address()}}
		subnetTx, err := wallet.IssueCreateSubnetTx(owner)
		if err != nil {
			fatalf("CreateSubnetTx: %v", err)
		}
		prog.subnet = subnetTx.ID()
		prog.save()
	} else {
		fmt.Println("[2/4] Subnet already created (resume)")
	}
	fmt.Printf("  Subnet ID: %s\n", prog.subnet)

	// Re-sync the wallet with the subnet so it can sign subnet-authorized txs.
	wallet, err = primary.MakePWallet(ctx, net.API, kc, primary.WalletConfig{SubnetIDs: []ids.ID{prog.subnet}})
	if err != nil {
		fatalf("re-sync wallet: %v", err)
	}

	if prog.chain == ids.Empty {
		fmt.Println("[3/4] Creating chain...")
		chainTx, err := wallet.IssueCreateChainTx(prog.subnet, genesisBytes, constants.SubnetEVMID, nil, "benchmarkchain")
		if err != nil {
			fatalf("CreateChainTx: %v", err)
		}
		prog.chain = chainTx.ID()
		prog.save()
	} else {
		fmt.Println("[3/4] Chain already created (resume)")
	}
	fmt.Printf("  Chain ID:  %s\n", prog.chain)

	// The conversion tx sorts validators by NodeID bytes; conversion index
	// (and thus validationID = subnetID.Append(index)) follows that order.
	sort.Slice(plans, func(a, b int) bool { return plans[a].nodeID.Compare(plans[b].nodeID) < 0 })

	if converted(ctx, pc, prog.subnet) {
		fmt.Println("[4/4] Subnet already converted (resume)")
	} else {
		fmt.Printf("[4/4] Converting subnet to L1 (manager = %s on the L1's OWN chain)...\n", managerAddress.Hex())
		owner := warpmessage.PChainOwner{Threshold: 1, Addresses: []ids.ShortID{walletKey.Address()}}
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
		if _, err := wallet.IssueConvertSubnetToL1Tx(prog.subnet, prog.chain, managerAddress.Bytes(), txValidators); err != nil {
			fatalf("ConvertSubnetToL1Tx: %v", err)
		}
		// The wallet waits for acceptance, but the public API is load-balanced:
		// give the backends a beat before anything queries the new L1 state.
		fmt.Println("  Conversion accepted; settling...")
		time.Sleep(5 * time.Second)
	}
	prog.manager = managerAddress.Hex()
	prog.save()

	fmt.Println()
	fmt.Println("=== L1 created ===")
	fmt.Printf("Subnet ID: %s\nChain ID:  %s\nManager:   %s (the L1's own chain; weights move via `l1 set-weight` / `l1 apply`)\n",
		prog.subnet, prog.chain, prog.manager)
	fmt.Printf("Registered validators (all at weight %d; conversion order = validationID index):\n", initialWeight)
	for i, p := range plans {
		fmt.Printf("  [%d] %-6s %s  validationID=%s\n",
			i, p.name, p.nodeID, prog.subnet.Append(uint32(i)))
	}
	fmt.Printf("Saved to %s. Next: ./setup/03_backup_secrets.sh, then ./run/01_deploy.sh\n", envPath)
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
			id, err := vset.GenerateIdentity(stakingDir, p.name, true)
			if err != nil {
				fatalf("%v", err)
			}
			fmt.Printf("  generated identity staking/l1/%s (%s)\n", p.name, id)
			p.nodeID = id
		}
		existing[p.name] = vset.Entry{Name: p.name, NodeID: p.nodeID}

		raw, err := os.ReadFile(filepath.Join(dir, "signer.key"))
		if err != nil {
			fatalf("read staking/l1/%s signer.key: %v", p.name, err)
		}
		sk, err := localsigner.FromBytes(raw)
		if err != nil {
			fatalf("load staking/l1/%s signer.key: %v", p.name, err)
		}
		if p.pop, err = signer.NewProofOfPossession(sk); err != nil {
			fatalf("build PoP for staking/l1/%s: %v", p.name, err)
		}
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
