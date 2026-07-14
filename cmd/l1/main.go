// Command l1 is the validator manager for our P-chain-anchored L1: it creates
// the chain and moves validator weights with no ValidatorManager contract, no
// courier and no external signature aggregator. We hold every validator's BLS
// signer key (staking/l1/N/signer.key), so the tool crafts each warp payload,
// signs it with all the keys it holds, aggregates locally and submits the
// P-chain tx itself. The only external dependency is one P-chain RPC.
//
// OPERATING MODEL: `create` records the validator manager as living on the
// L1's OWN chain (sourceChainID = the new blockchainID, sourceAddress =
// 0x0000000000000000000000000000000000000001). The P-chain then verifies our
// weight messages against OUR validator set, which we can sign for because we
// hold all the keys. The validator list NEVER changes after create; only
// weights move. It is NOT usable on an L1 whose manager was recorded on the
// C-chain: there the P-chain expects primary-network signatures we do not have.
//
//	l1 create     [--validators N] [--active-weight w] [--standby-weight w]
//	              [--balance avax] [--force]
//	              generate missing node identities, create subnet + chain,
//	              convert to an L1 with all N validators registered; writes
//	              network.env (the ONLY registration point, run once per chain)
//	l1 set-weight --node <name|N|nodeID|validationID> --weight <w>
//	              set one validator's weight (0 removes; we never remove)
//	l1 apply      --weights a1=100000,a2=100000,b1=1,...
//	              declarative target weights, applied all raises first then
//	              lowers, one tx at a time, each verified on-chain
//	l1 status     print the registered set with balances and a runway warning
//
// Config comes from .env / network.env (NETWORK, SUBNET_ID, CHAIN_ID,
// MANAGER_ADDRESS; PCHAIN_API overrides the RPC) plus the staking/ directory
// (l1/N/signer.key BLS keys, node-ids.env, fuji-wallet.key fee wallet).
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
	"strconv"

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
		fs.IntVar(&opts.validators, "validators", 0, "number of registered validators (default: the .env topology's staking slots, else 8)")
		fs.Uint64Var(&opts.activeWeight, "active-weight", defaultActiveWeight, "conversion weight of the site A validators")
		fs.Uint64Var(&opts.standbyWeight, "standby-weight", defaultStandbyWeight, "conversion weight of the site B (standby) validators")
		fs.Float64Var(&opts.balanceAvax, "balance", 0, "per-validator continuous-fee balance in AVAX (default: the network's standard deposit)")
		fs.BoolVar(&opts.force, "force", false, "resume/verify even though network.env already records a SUBNET_ID")
		_ = fs.Parse(os.Args[2:])
		create(ctx, opts)
	case "set-weight":
		node := fs.String("node", "", "validator name (a1..), staking slot number, NodeID-... or validationID")
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
type config struct {
	net         netcfg.Config
	subnetID    ids.ID
	chainID     ids.ID // the L1's blockchainID: warp sourceChainID
	managerAddr []byte // recorded manager address: warp sourceAddress
	stakingDir  string
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

// loadSigners loads every staking/l1/N/signer.key. We must sign with every
// registered validator's key to clear the 67% warp quorum, so a slot
// directory without its signer.key is fatal here rather than a silent
// below-quorum signature later.
func loadSigners(stakingDir string) ([]bls.Signer, error) {
	entries, err := os.ReadDir(filepath.Join(stakingDir, "l1"))
	if err != nil {
		return nil, fmt.Errorf("read %s/l1: %w", stakingDir, err)
	}
	var signers []bls.Signer
	for _, e := range entries {
		key, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(stakingDir, "l1", e.Name(), "signer.key"))
		if err != nil {
			return nil, fmt.Errorf("read signer.key for slot %d: %w (without every registered key the local signatures cannot clear the 67%% warp quorum)", key, err)
		}
		s, err := localsigner.FromBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("load signer.key for slot %d: %w", key, err)
		}
		signers = append(signers, s)
	}
	if len(signers) == 0 {
		return nil, fmt.Errorf("no signer.key files under %s/l1", stakingDir)
	}
	return signers, nil
}

// namesByNodeID reads the manifest's validator names (written by `l1 create`;
// key number for entries without a name). Best effort: an unreadable manifest
// just means unnamed output.
func namesByNodeID(stakingDir string) map[ids.NodeID]string {
	out := map[ids.NodeID]string{}
	entries, err := vset.ReadManifest(stakingDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.Name != "" {
			out[e.NodeID] = e.Name
		} else {
			out[e.NodeID] = strconv.Itoa(e.Key)
		}
	}
	return out
}

// resolveValidator maps a --node argument (validator name from the manifest,
// staking slot number, NodeID or validationID) to a registered validator.
func resolveValidator(cfg config, vs []vset.Validator, node string) vset.Validator {
	var match func(v vset.Validator) bool
	if nodeID, ok := manifestNodeID(cfg.stakingDir, node); ok {
		match = func(v vset.Validator) bool { return v.NodeID == nodeID }
	} else if nodeID, err := ids.NodeIDFromString(node); err == nil {
		match = func(v vset.Validator) bool { return v.NodeID == nodeID }
	} else if vid, err := ids.FromString(node); err == nil {
		match = func(v vset.Validator) bool { return v.ValidationID == vid }
	} else {
		fatalf("--node %q is not a validator name, slot number, NodeID or validationID", node)
	}
	for _, v := range vs {
		if match(v) {
			return v
		}
	}
	fatalf("--node %q does not match any registered validator (see l1 status)", node)
	panic("unreachable")
}

// keysByNodeID maps registered NodeIDs to their manifest key, for stable
// slot-ordered output. Best effort like namesByNodeID.
func keysByNodeID(stakingDir string) map[ids.NodeID]int {
	out := map[ids.NodeID]int{}
	entries, err := vset.ReadManifest(stakingDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		out[e.NodeID] = e.Key
	}
	return out
}

// manifestNodeID resolves a validator name (a1..) or staking slot number
// against the manifest.
func manifestNodeID(stakingDir, node string) (ids.NodeID, bool) {
	entries, err := vset.ReadManifest(stakingDir)
	if err != nil {
		return ids.EmptyNodeID, false
	}
	for _, e := range entries {
		if e.Name == node || strconv.Itoa(e.Key) == node {
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
	keys := keysByNodeID(cfg.stakingDir)
	sort.Slice(vs, func(i, j int) bool { return keys[vs[i].NodeID] < keys[vs[j].NodeID] })
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
}

// runwayDays converts a continuous-fee balance to days of runway at rate.
func runwayDays(balance, ratePerSec uint64) float64 {
	return float64(balance) / float64(ratePerSec) / 86400
}

func avaxString(navax uint64) string {
	return fmt.Sprintf("%d.%02d", navax/units.Avax, navax%units.Avax/(units.Avax/100))
}
