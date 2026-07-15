// Command l1 is the validator manager for our P-chain-anchored L1: it creates
// the chain and moves validator weights with no ValidatorManager contract, no
// courier and no external signature aggregator. We hold every SIGNING BLS key,
// so the tool crafts each warp payload, signs it with all the keys it holds,
// aggregates locally and submits the P-chain tx itself. The only external
// dependency is one P-chain RPC.
//
// OPERATING MODEL (committee): `create` builds TWO L1s from the single wallet
// key. A small MANAGER L1 (its own subnet + a phantom chain that never runs)
// registers an equal-weight signing COMMITTEE whose BLS keys we generate and
// hold under staking/manager/. The MAIN L1 (the fleet) is then converted with
// its recorded validator manager set to the MANAGER L1's blockchainID
// (sourceChainID) + address 0x0000000000000000000000000000000000000001. The
// P-chain verifies the main L1's weight messages against the MANAGER subnet's
// validator set, which the committee keys sign for. Both validator lists NEVER
// change after create; only the main L1's weights move. The default committee
// is 4 (3-of-4 = 75% survives one signer loss at the strict 67% warp quorum).
// The committee must stay FUNDED: a drained committee validator goes INACTIVE
// (drops its BLS key, keeps its weight in the denominator) and dilutes quorum.
//
// A network.env without MANAGER_CHAIN_ID/MANAGER_SUBNET_ID is a legacy
// self-managed chain: weight messages then carry sourceChainID = the L1's own
// chain and are signed by the L1's own validators (staking/l1/), verified
// against the L1's own set. `create` always writes the committee fields.
//
//	l1 create     [--balance avax] [--committee N] [--committee-balance avax]
//	              [--allow-fragile-committee] [--force]
//	              generate the committee + the inventory's role=validator
//	              nodes, create both subnets + chains, convert both to L1s;
//	              writes network.env (the ONLY registration point, run once)
//	l1 set-weight --node <name|nodeID|validationID> --weight <w>
//	              set one validator's weight (0 removes; we never remove)
//	l1 apply      --weights a1=100000,a2=100000,b1=1,...
//	              declarative target weights, applied all raises first then
//	              lowers, one tx at a time, each verified on-chain
//	l1 status     print the registered set + the committee, with balances and
//	              runway warnings
//
// Config comes from nodes.ini (the fleet inventory) and .env / network.env
// (NETWORK, SUBNET_ID, CHAIN_ID, MANAGER_ADDRESS, MANAGER_SUBNET_ID,
// MANAGER_CHAIN_ID; PCHAIN_API overrides the RPC) plus the staking/ directory
// (manager/<name>/signer.key committee keys, l1/<name>/signer.key fleet BLS
// keys, node-ids.env, fuji-wallet.key fee wallet).
//
// Txs verify at the proposer's P-chain height, which can lag the tip. On a
// "signature is invalid" or set-mismatch rejection just re-run the command:
// every run refetches the registered set and re-signs fresh.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ava-labs/libevm/common"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	pwallet "github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	"github.com/joho/godotenv"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fujikey"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/netcfg"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/vset"
)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "l1: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: l1 <create|set-weight|apply|status> [flags]")
	}
	loadEnvFiles()

	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	ctx := context.Background()
	switch os.Args[1] {
	case "create":
		var opts createOpts
		fs.Float64Var(&opts.balanceAvax, "balance", 0, "per-validator continuous-fee balance in AVAX (default: the network's standard deposit)")
		fs.BoolVar(&opts.force, "force", false, "resume/verify even though network.env already records a SUBNET_ID")
		fs.IntVar(&opts.committee, "committee", defaultCommittee, "manager-L1 signing committee size (equal-weight); must be >= 4 so 67% quorum survives one signer loss")
		fs.Float64Var(&opts.committeeBalanceAvax, "committee-balance", 0, "per-committee-validator continuous-fee balance in AVAX (default: keeps the committee ACTIVE well past a benchmark)")
		fs.BoolVar(&opts.allowFragile, "allow-fragile-committee", false, "permit a committee < 4 (cannot tolerate one signer loss at the 67% quorum)")
		_ = fs.Parse(os.Args[2:])
		create(ctx, opts)
	case "set-weight":
		node := fs.String("node", "", "validator name (from nodes.ini), NodeID-... or validationID")
		weight := fs.Uint64("weight", 0, "target validator weight (0 removes)")
		_ = fs.Parse(os.Args[2:])
		if *node == "" {
			fatalf("set-weight: --node is required")
		}
		setWeight(ctx, loadConfig(), *node, *weight)
	case "apply":
		weights := fs.String("weights", "", "comma-separated name=weight target list, e.g. a1=100000,b1=1")
		_ = fs.Parse(os.Args[2:])
		if *weights == "" {
			fatalf("apply: --weights is required")
		}
		apply(ctx, loadConfig(), *weights)
	case "status":
		_ = fs.Parse(os.Args[2:])
		status(ctx, loadConfig())
	default:
		fatalf("unknown command %q (want create, set-weight, apply or status)", os.Args[1])
	}
}

// loadEnvFiles best-effort loads .env and network.env (exe-relative, matching
// fuji-wallet's lookup, else cwd). godotenv never overrides variables already
// in the environment.
func loadEnvFiles() {
	for _, name := range []string{".env", "network.env"} {
		if exe, err := os.Executable(); err == nil {
			p := filepath.Join(filepath.Dir(exe), "..", name)
			if _, err := os.Stat(p); err == nil {
				_ = godotenv.Load(p)
				continue
			}
		}
		_ = godotenv.Load(name)
	}
}

// config is everything the weight/status subcommands need, resolved once from
// the env. `create` runs before network.env exists and resolves its own.
//
// Two signing models coexist, chosen by whether MANAGER_CHAIN_ID is set:
//   - committee (managerChainID != Empty): weight-change warp messages carry
//     sourceChainID = the manager L1's blockchainID and are signed by the
//     manager L1's committee (staking/manager/<name>), verified by the
//     P-chain against the MANAGER subnet's validator set. This is what
//     `l1 create` writes.
//   - self-managed (managerChainID == Empty): the legacy model where the L1
//     manages itself; messages carry sourceChainID = the L1's own chainID and
//     are signed by the L1's own validators (staking/l1/<name>). Kept so a
//     chain created before the committee model still works.
type config struct {
	net             netcfg.Config
	subnetID        ids.ID
	chainID         ids.ID // the main L1's blockchainID
	managerAddr     []byte // recorded manager address: warp sourceAddress (0x..01)
	managerChainID  ids.ID // committee model: the manager L1's blockchainID (warp sourceChainID); Empty => self-managed
	managerSubnetID ids.ID // committee model: the manager L1's subnetID (its validator set signs; also used for reads/reclaim)
	stakingDir      string
}

// committee reports whether the committee signing model is in force.
func (c config) committee() bool { return c.managerChainID != ids.Empty }

// signChainID is the warp sourceChainID for weight messages: the manager L1's
// chain under the committee model, the L1's own chain when self-managed.
func (c config) signChainID() ids.ID {
	if c.committee() {
		return c.managerChainID
	}
	return c.chainID
}

// signSubnetID is the subnet whose validator set the P-chain verifies weight
// messages against: the manager subnet under the committee model, the L1's own
// subnet when self-managed.
func (c config) signSubnetID() ids.ID {
	if c.committee() {
		return c.managerSubnetID
	}
	return c.subnetID
}

// signTier is the staking/<tier> directory holding the signing BLS keys.
func (c config) signTier() string {
	if c.committee() {
		return "manager"
	}
	return "l1"
}

// stakingDirPath resolves the staking/ directory exe-relative first, cwd
// otherwise (mirroring loadEnvFiles).
func stakingDirPath() string {
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), "..", "staking"); dirExists(p) {
			return p
		}
	}
	return "staking"
}

func loadConfig() config {
	c := config{net: netcfg.Get(), stakingDir: stakingDirPath()}
	var err error
	if c.subnetID, err = ids.FromString(envOrFatal("SUBNET_ID")); err != nil {
		fatalf("parse SUBNET_ID: %v", err)
	}
	if c.chainID, err = ids.FromString(envOrFatal("CHAIN_ID")); err != nil {
		fatalf("parse CHAIN_ID: %v", err)
	}
	addr := envOrFatal("MANAGER_ADDRESS")
	if !common.IsHexAddress(addr) {
		fatalf("MANAGER_ADDRESS %q is not a hex address", addr)
	}
	c.managerAddr = common.HexToAddress(addr).Bytes()
	// Committee model: both MANAGER_CHAIN_ID and MANAGER_SUBNET_ID are written
	// together by `l1 create`; either alone is a corrupt network.env.
	mc, ms := os.Getenv("MANAGER_CHAIN_ID"), os.Getenv("MANAGER_SUBNET_ID")
	if (mc == "") != (ms == "") {
		fatalf("network.env has only one of MANAGER_CHAIN_ID / MANAGER_SUBNET_ID; both or neither")
	}
	if mc != "" {
		if c.managerChainID, err = ids.FromString(mc); err != nil {
			fatalf("parse MANAGER_CHAIN_ID: %v", err)
		}
		if c.managerSubnetID, err = ids.FromString(ms); err != nil {
			fatalf("parse MANAGER_SUBNET_ID: %v", err)
		}
	}
	return c
}

func envOrFatal(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fatalf("%s not set (network.env missing? run `l1 create` first, from the repo root)", key)
	}
	return v
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// loadSigners loads every BLS signer key we hold under staking/<tier>/<name>/
// signer.key. tier is "manager" (the committee) or "l1" (self-managed). Only
// validator identities carry a key - rpc identities never do - so a dir
// without a signer.key is simply skipped; if the loaded keys cannot clear the
// 67% warp quorum, signAndAggregate refuses with the exact shortfall.
func loadSigners(stakingDir, tier string) ([]bls.Signer, error) {
	if tier == "l1" {
		// CheckNamedKeyDirs guards the retired numbered l1 layout only.
		if err := vset.CheckNamedKeyDirs(stakingDir); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(filepath.Join(stakingDir, tier))
	if err != nil {
		return nil, fmt.Errorf("read %s/%s: %w", stakingDir, tier, err)
	}
	var signers []bls.Signer
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(stakingDir, tier, e.Name(), "signer.key"))
		if os.IsNotExist(err) {
			continue // an rpc identity: no BLS key, never registered
		}
		if err != nil {
			return nil, fmt.Errorf("read signer.key for %s: %w", e.Name(), err)
		}
		s, err := localsigner.FromBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("load signer.key for %s: %w", e.Name(), err)
		}
		signers = append(signers, s)
	}
	if len(signers) == 0 {
		return nil, fmt.Errorf("no signer.key files under %s/%s (validator identities missing?)", stakingDir, tier)
	}
	return signers, nil
}

// namesByNodeID reads the manifest's node names. Best effort: an unreadable
// manifest just means unnamed output.
func namesByNodeID(stakingDir string) map[ids.NodeID]string {
	out := map[ids.NodeID]string{}
	entries, err := vset.ReadManifest(stakingDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		out[e.NodeID] = e.Name
	}
	return out
}

// resolveValidator maps a --node argument (node name from the manifest,
// NodeID or validationID) to a registered validator. A name that resolves to
// a role=rpc node fails with the role named: rpc nodes are never registered.
func resolveValidator(cfg config, vs []vset.Validator, node string) vset.Validator {
	var match func(v vset.Validator) bool
	if nodeID, ok := manifestNodeID(cfg.stakingDir, node); ok {
		match = func(v vset.Validator) bool { return v.NodeID == nodeID }
	} else if nodeID, err := ids.NodeIDFromString(node); err == nil {
		match = func(v vset.Validator) bool { return v.NodeID == nodeID }
	} else if vid, err := ids.FromString(node); err == nil {
		match = func(v vset.Validator) bool { return v.ValidationID == vid }
	} else {
		fatalf("--node %q is not a node name, NodeID or validationID", node)
	}
	for _, v := range vs {
		if match(v) {
			return v
		}
	}
	if nodeRole(node) == topo.RoleRPC {
		fatalf("%s is role=rpc, not a registered validator", node)
	}
	fatalf("--node %q does not match any registered validator (see l1 status)", node)
	panic("unreachable")
}

// nodeRole looks a name up in the inventory, "" when nodes.ini is unreadable
// or the name is not in it. Used only to sharpen error messages.
func nodeRole(name string) string {
	nodes, err := topo.LoadNear()
	if err != nil {
		return ""
	}
	for _, n := range nodes {
		if n.Name == name {
			return n.Role
		}
	}
	return ""
}

// manifestNodeID resolves a node name against the manifest.
func manifestNodeID(stakingDir, node string) (ids.NodeID, bool) {
	entries, err := vset.ReadManifest(stakingDir)
	if err != nil {
		return ids.EmptyNodeID, false
	}
	for _, e := range entries {
		if e.Name == node {
			return e.NodeID, true
		}
	}
	return ids.EmptyNodeID, false
}

func makeWallet(ctx context.Context, cfg config) pwallet.Wallet {
	key, err := fujikey.Load(filepath.Join(cfg.stakingDir, "fuji-wallet.key"))
	if err != nil {
		fatalf("load fee wallet key: %v", err)
	}
	w, err := primary.MakePWallet(ctx, cfg.net.API, secp256k1fx.NewKeychain(key), primary.WalletConfig{})
	if err != nil {
		fatalf("make P-chain wallet: %v", err)
	}
	return w
}

// feeRate returns the continuous-fee rate (nAVAX per validator-second) used
// for runway math: the live platform.getValidatorFeeState price, floored at
// the ACP-77 512 nAVAX/s minimum (also the fallback when the call fails).
func feeRate(ctx context.Context, pc *platformvm.Client) uint64 {
	const floor = 512
	if _, price, _, err := pc.GetValidatorFeeState(ctx); err == nil && uint64(price) > floor {
		return uint64(price)
	}
	return floor
}

// runwayWarnDays is the balance-runway threshold `status` warns at: under a
// week of continuous fee left.
const runwayWarnDays = 7.0

func status(ctx context.Context, cfg config) {
	pc := platformvm.NewClient(cfg.net.API)
	vs, err := vset.Fetch(ctx, pc, cfg.subnetID, 1)
	if err != nil {
		fatalf("%v", err)
	}
	names := namesByNodeID(cfg.stakingDir)
	sort.Slice(vs, func(i, j int) bool { return names[vs[i].NodeID] < names[vs[j].NodeID] })
	rate := feeRate(ctx, pc)

	var total uint64
	for _, v := range vs {
		total += v.Weight
	}
	fmt.Printf("subnet %s: %d validators, total weight %d, fee rate %d nAVAX/s\n",
		cfg.subnetID, len(vs), total, rate)
	var short []string
	for _, v := range vs {
		name := names[v.NodeID]
		if name == "" {
			name = "?"
		}
		days := runwayDays(v.Balance, rate)
		fmt.Printf("  %-3s %s  %s  weight %-8d balance %s AVAX (%.1f days)  minNonce %d\n",
			name, v.NodeID, v.ValidationID, v.Weight, avaxString(v.Balance), days, v.MinNonce)
		if days < runwayWarnDays {
			short = append(short, name)
		}
	}
	if len(short) > 0 {
		fmt.Printf("WARNING: %v under %.0f days of continuous-fee runway; a drained validator goes\n", short, runwayWarnDays)
		fmt.Println("         INACTIVE and dilutes the warp quorum. Top up with: fuji-wallet topup")
	}

	if cfg.committee() {
		statusCommittee(ctx, pc, cfg, rate)
	}
}

// statusCommittee prints the manager L1's signing committee: the set whose
// BLS keys actually sign this L1's weight changes. A committee validator that
// drains its continuous-fee balance goes INACTIVE - it drops its BLS key but
// KEEPS its weight in the quorum denominator, diluting the quorum - so the
// committee must stay funded. INACTIVE is read as a nil public key from
// getL1Validator.
func statusCommittee(ctx context.Context, pc *platformvm.Client, cfg config, rate uint64) {
	cvs, err := vset.Fetch(ctx, pc, cfg.managerSubnetID, 1)
	if err != nil {
		fmt.Printf("committee (manager subnet %s): FETCH FAILED: %v\n", cfg.managerSubnetID, err)
		return
	}
	sort.Slice(cvs, func(i, j int) bool { return cvs[i].NodeID.Compare(cvs[j].NodeID) < 0 })
	var total, active uint64
	for _, v := range cvs {
		total += v.Weight
		if v.PublicKey != nil {
			active += v.Weight
		}
	}
	// The P-chain passes weight messages at the strict 67% quorum; report the
	// live active-weight headroom against it.
	quorumOK := active*quorumDen >= total*quorumNum
	fmt.Printf("committee (manager chain %s, subnet %s): %d validators, active weight %d/%d, quorum %d%% %s\n",
		cfg.managerChainID, cfg.managerSubnetID, len(cvs), active, total, quorumNum,
		map[bool]string{true: "OK", false: "LOST (weight changes will be REJECTED)"}[quorumOK])
	var short []string
	for i, v := range cvs {
		state := "ACTIVE"
		if v.PublicKey == nil {
			state = "INACTIVE(key dropped, still dilutes quorum)"
		}
		days := runwayDays(v.Balance, rate)
		fmt.Printf("  [%d] %s  %s  weight %-6d balance %s AVAX (%.1f days)  %s\n",
			i, v.NodeID, v.ValidationID, v.Weight, avaxString(v.Balance), days, state)
		if days < runwayWarnDays {
			short = append(short, v.NodeID.String())
		}
	}
	if len(short) > 0 {
		fmt.Printf("WARNING: committee %v under %.0f days of runway; a drained committee validator goes\n", short, runwayWarnDays)
		fmt.Println("         INACTIVE and dilutes the SIGNING quorum. Keep the committee funded: fuji-wallet topup")
	}
}

// runwayDays converts a continuous-fee balance to days of runway at rate.
func runwayDays(balance, ratePerSec uint64) float64 {
	return float64(balance) / float64(ratePerSec) / 86400
}

func avaxString(navax uint64) string {
	return fmt.Sprintf("%d.%02d", navax/units.Avax, navax%units.Avax/(units.Avax/100))
}
