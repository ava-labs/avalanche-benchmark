// Command warp-signer manages our L1's validator set directly on the P-chain,
// with no ValidatorManager contract, no courier and no external signature
// aggregator. We hold every validator's BLS signer key (staking/l1/N/signer.key),
// so the tool crafts the warp payload, signs it with all the keys it holds,
// aggregates locally and submits the P-chain tx itself. The only external
// dependency is one P-chain RPC.
//
// OPERATING MODEL: this only works on an L1 whose ConvertSubnetToL1Tx recorded
// the validator manager as living on the L1's OWN chain (sourceChainID =
// CHAIN_ID, sourceAddress = MANAGER_ADDRESS). The P-chain then verifies these
// messages against OUR validator set, which we can sign for because we hold
// all the keys. It is NOT usable on an L1 whose manager was recorded on the
// C-chain: there the P-chain expects primary-network signatures we do not have.
//
//	warp-signer status                                        print the current set
//	warp-signer set-weight --node <N|nodeID|validationID> --weight <w>
//	                                                          set a validator's weight (0 removes)
//	warp-signer register   --node <N> --weight <w> --balance <avax>
//	                                                          register staking slot N as a validator
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
	"strings"
	"time"

	"github.com/ava-labs/libevm/common"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/platformvm/signer"
	warpmessage "github.com/ava-labs/avalanchego/vms/platformvm/warp/message"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	pwallet "github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	"github.com/joho/godotenv"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fujikey"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/netcfg"
)

// registerExpiry is how far in the future a RegisterL1Validator message
// expires. The P-chain rejects anything past now+24h; 1h clears clock skew
// and is far more than one tx needs.
const registerExpiry = time.Hour

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "warp-signer: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: warp-signer <status|set-weight|register> [flags]")
	}
	loadEnvFiles()
	cfg := loadConfig()

	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	node := fs.String("node", "", "staking slot number, NodeID-... or validationID")
	weight := fs.Uint64("weight", 0, "target validator weight (0 removes)")
	balance := fs.Float64("balance", 0, "register: initial continuous-fee balance in AVAX")
	_ = fs.Parse(os.Args[2:])

	ctx := context.Background()
	switch os.Args[1] {
	case "status":
		status(ctx, cfg)
	case "set-weight":
		if *node == "" {
			fatalf("set-weight: --node is required")
		}
		setWeight(ctx, cfg, *node, *weight)
	case "register":
		if *node == "" {
			fatalf("register: --node is required")
		}
		register(ctx, cfg, *node, *weight, *balance)
	default:
		fatalf("unknown command %q (want status, set-weight or register)", os.Args[1])
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

// config is everything the subcommands need, resolved once from the env.
type config struct {
	net         netcfg.Config
	subnetID    ids.ID
	chainID     ids.ID // the L1's blockchainID: warp sourceChainID
	managerAddr []byte // recorded manager address: warp sourceAddress
	stakingDir  string
}

func loadConfig() config {
	c := config{net: netcfg.Get(), stakingDir: "staking"}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), "..", "staking"); dirExists(p) {
			c.stakingDir = p
		}
	}

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
		fatalf("%s not set (network.env missing? run from the repo root)", key)
	}
	return v
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// loadSigners loads every staking/l1/N/signer.key, keyed by slot number so
// register can find the new validator's key for its PoP.
func loadSigners(stakingDir string) (map[int]bls.Signer, error) {
	entries, err := os.ReadDir(filepath.Join(stakingDir, "l1"))
	if err != nil {
		return nil, fmt.Errorf("read %s/l1: %w", stakingDir, err)
	}
	signers := make(map[int]bls.Signer, len(entries))
	for _, e := range entries {
		key, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(stakingDir, "l1", e.Name(), "signer.key"))
		if err != nil {
			return nil, fmt.Errorf("read signer.key for slot %d: %w", key, err)
		}
		s, err := localsigner.FromBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("load signer.key for slot %d: %w", key, err)
		}
		signers[key] = s
	}
	if len(signers) == 0 {
		return nil, fmt.Errorf("no signer.key files under %s/l1", stakingDir)
	}
	return signers, nil
}

func signerList(m map[int]bls.Signer) []bls.Signer {
	out := make([]bls.Signer, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	return out
}

// nodeIDsBySlot reads staking/node-ids.env (L1_<N>_NODE_ID entries).
func nodeIDsBySlot(stakingDir string) (map[int]ids.NodeID, error) {
	p := filepath.Join(stakingDir, "node-ids.env")
	vars, err := godotenv.Read(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	out := make(map[int]ids.NodeID, len(vars))
	for k, v := range vars {
		var slot int
		if _, err := fmt.Sscanf(k, "L1_%d_NODE_ID", &slot); err != nil {
			continue
		}
		id, err := ids.NodeIDFromString(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", k, err)
		}
		out[slot] = id
	}
	return out, nil
}

// currentSet fetches the registered L1 validators and flattens them into the
// canonical warp set, exactly as the P-chain will at verification time.
func currentSet(ctx context.Context, pc *platformvm.Client, subnetID ids.ID) ([]platformvm.ClientPermissionlessValidator, validators.WarpSet, error) {
	vs, err := pc.GetCurrentValidators(ctx, subnetID, nil)
	if err != nil {
		return nil, validators.WarpSet{}, fmt.Errorf("platform.getCurrentValidators: %w", err)
	}
	m := make(map[ids.NodeID]*validators.GetValidatorOutput, len(vs))
	for _, v := range vs {
		out := &validators.GetValidatorOutput{NodeID: v.NodeID, Weight: v.Weight}
		if v.Signer != nil {
			pk, err := bls.PublicKeyFromCompressedBytes(v.Signer.PublicKey[:])
			if err != nil {
				return nil, validators.WarpSet{}, fmt.Errorf("parse BLS key of %s: %w", v.NodeID, err)
			}
			out.PublicKey = pk
		}
		m[v.NodeID] = out
	}
	warpSet, err := validators.FlattenValidatorSet(m)
	if err != nil {
		return nil, validators.WarpSet{}, err
	}
	return vs, warpSet, nil
}

// resolveValidator maps --node (slot number, NodeID or validationID) to a
// registered validator.
func resolveValidator(cfg config, vs []platformvm.ClientPermissionlessValidator, node string) platformvm.ClientPermissionlessValidator {
	var match func(v platformvm.ClientPermissionlessValidator) bool
	if slot, err := strconv.Atoi(node); err == nil {
		id, ok := mustNodeIDs(cfg.stakingDir)[slot]
		if !ok {
			fatalf("no L1_%d_NODE_ID in %s/node-ids.env", slot, cfg.stakingDir)
		}
		match = func(v platformvm.ClientPermissionlessValidator) bool { return v.NodeID == id }
	} else if nodeID, err := ids.NodeIDFromString(node); err == nil {
		match = func(v platformvm.ClientPermissionlessValidator) bool { return v.NodeID == nodeID }
	} else if vid, err := ids.FromString(node); err == nil {
		match = func(v platformvm.ClientPermissionlessValidator) bool {
			return v.ValidationID != nil && *v.ValidationID == vid
		}
	} else {
		fatalf("--node %q is not a slot number, NodeID or validationID", node)
	}
	for _, v := range vs {
		if match(v) {
			if v.ValidationID == nil {
				fatalf("validator %s has no validationID (not an L1 validator?)", v.NodeID)
			}
			return v
		}
	}
	fatalf("--node %q does not match any registered validator (see warp-signer status)", node)
	panic("unreachable")
}

func mustNodeIDs(stakingDir string) map[int]ids.NodeID {
	by, err := nodeIDsBySlot(stakingDir)
	if err != nil {
		fatalf("%v", err)
	}
	return by
}

func status(ctx context.Context, cfg config) {
	pc := platformvm.NewClient(cfg.net.API)
	vs, warpSet, err := currentSet(ctx, pc, cfg.subnetID)
	if err != nil {
		fatalf("%v", err)
	}
	slotOf := map[ids.NodeID]int{}
	if by, err := nodeIDsBySlot(cfg.stakingDir); err == nil {
		for slot, id := range by {
			slotOf[id] = slot
		}
	}
	sort.Slice(vs, func(i, j int) bool { return slotOf[vs[i].NodeID] < slotOf[vs[j].NodeID] })

	fmt.Printf("subnet %s: %d validators, total weight %d\n", cfg.subnetID, len(vs), warpSet.TotalWeight)
	for _, v := range vs {
		name := "?"
		if slot, ok := slotOf[v.NodeID]; ok {
			name = strconv.Itoa(slot)
		}
		if v.ValidationID == nil {
			fmt.Printf("  %-3s %s  weight %d  (no validationID)\n", name, v.NodeID, v.Weight)
			continue
		}
		l1v, _, err := pc.GetL1Validator(ctx, *v.ValidationID)
		if err != nil {
			fatalf("platform.getL1Validator(%s): %v", *v.ValidationID, err)
		}
		fmt.Printf("  %-3s %s  %s  weight %d  balance %s AVAX  minNonce %d\n",
			name, v.NodeID, *v.ValidationID, l1v.Weight, avaxString(l1v.Balance), l1v.MinNonce)
	}
}

func setWeight(ctx context.Context, cfg config, node string, weight uint64) {
	pc := platformvm.NewClient(cfg.net.API)
	vs, warpSet, err := currentSet(ctx, pc, cfg.subnetID)
	if err != nil {
		fatalf("%v", err)
	}
	v := resolveValidator(cfg, vs, node)
	l1v, _, err := pc.GetL1Validator(ctx, *v.ValidationID)
	if err != nil {
		fatalf("platform.getL1Validator(%s): %v", *v.ValidationID, err)
	}

	payload, err := warpmessage.NewL1ValidatorWeight(*v.ValidationID, l1v.MinNonce, weight)
	if err != nil {
		fatalf("build L1ValidatorWeight: %v", err)
	}
	unsigned, err := addressedCall(cfg.net.NetworkID, cfg.chainID, cfg.managerAddr, payload.Bytes())
	if err != nil {
		fatalf("build warp message: %v", err)
	}

	signers, err := loadSigners(cfg.stakingDir)
	if err != nil {
		fatalf("%v", err)
	}
	signed, err := signAndAggregate(unsigned, warpSet, signerList(signers))
	if err != nil {
		fatalf("%v", err)
	}

	action := fmt.Sprintf("weight %d -> %d", l1v.Weight, weight)
	if weight == 0 {
		action = fmt.Sprintf("remove (weight %d -> 0)", l1v.Weight)
	}
	fmt.Printf("%s %s (%s), nonce %d: submitting SetL1ValidatorWeightTx...\n", v.NodeID, *v.ValidationID, action, l1v.MinNonce)

	wallet := makeWallet(ctx, cfg)
	tx, err := wallet.IssueSetL1ValidatorWeightTx(signed.Bytes())
	if err != nil {
		fatalf("SetL1ValidatorWeightTx: %v (safe to re-run: each run refetches the set and re-signs)", err)
	}
	fmt.Printf("accepted: %s\n", tx.ID())
}

func register(ctx context.Context, cfg config, node string, weight uint64, balanceAvax float64) {
	slot, err := strconv.Atoi(node)
	if err != nil {
		fatalf("register: --node must be a staking slot number (needs the slot's signer.key)")
	}
	if weight == 0 {
		fatalf("register: --weight must be > 0")
	}
	if balanceAvax <= 0 {
		fatalf("register: --balance (AVAX) must be > 0")
	}
	nodeID, ok := mustNodeIDs(cfg.stakingDir)[slot]
	if !ok {
		fatalf("no L1_%d_NODE_ID in %s/node-ids.env", slot, cfg.stakingDir)
	}

	signers, err := loadSigners(cfg.stakingDir)
	if err != nil {
		fatalf("%v", err)
	}
	newSigner, ok := signers[slot]
	if !ok {
		fatalf("no signer.key for slot %d under %s/l1", slot, cfg.stakingDir)
	}
	pop, err := signer.NewProofOfPossession(newSigner)
	if err != nil {
		fatalf("build PoP for slot %d: %v", slot, err)
	}

	walletKey, err := fujikey.Load(filepath.Join(cfg.stakingDir, "fuji-wallet.key"))
	if err != nil {
		fatalf("load fee wallet key: %v", err)
	}
	owner := warpmessage.PChainOwner{Threshold: 1, Addresses: []ids.ShortID{walletKey.Address()}}

	payload, err := warpmessage.NewRegisterL1Validator(
		cfg.subnetID,
		nodeID,
		pop.PublicKey,
		uint64(time.Now().Add(registerExpiry).Unix()),
		owner, // remaining balance owner
		owner, // disable owner
		weight,
	)
	if err != nil {
		fatalf("build RegisterL1Validator: %v", err)
	}
	unsigned, err := addressedCall(cfg.net.NetworkID, cfg.chainID, cfg.managerAddr, payload.Bytes())
	if err != nil {
		fatalf("build warp message: %v", err)
	}

	pc := platformvm.NewClient(cfg.net.API)
	_, warpSet, err := currentSet(ctx, pc, cfg.subnetID)
	if err != nil {
		fatalf("%v", err)
	}
	signed, err := signAndAggregate(unsigned, warpSet, signerList(signers))
	if err != nil {
		fatalf("%v", err)
	}

	balance := uint64(balanceAvax * float64(units.Avax))
	fmt.Printf("register slot %d (%s), weight %d, balance %s AVAX, validationID %s: submitting RegisterL1ValidatorTx...\n",
		slot, nodeID, weight, avaxString(balance), payload.ValidationID())

	wallet := makeWallet(ctx, cfg)
	tx, err := wallet.IssueRegisterL1ValidatorTx(balance, pop.ProofOfPossession, signed.Bytes())
	if err != nil {
		fatalf("RegisterL1ValidatorTx: %v (safe to re-run: each run refetches the set and re-signs)", err)
	}
	fmt.Printf("accepted: %s\n", tx.ID())
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

func avaxString(navax uint64) string {
	return fmt.Sprintf("%d.%02d", navax/units.Avax, navax%units.Avax/(units.Avax/100))
}
