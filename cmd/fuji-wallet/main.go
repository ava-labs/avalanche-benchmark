// Command fuji-wallet manages the per-deploy Fuji fund/fee wallet (the key
// that pays for subnet/chain creation and validator continuous fees on Fuji's
// public P-chain, and for the ValidatorManager contract gas on the Fuji
// C-chain). The key is GENERATED per deploy and gitignored: never committed
// (see FUJI_PLAN.md "KEY POLICY").
//
//	fuji-wallet gen  -key staking/fuji-wallet.key   generate the key, print addresses
//	fuji-wallet fund -key staking/fuji-wallet.key   print the P- and C-chain
//	    addresses with the required amounts and poll until BOTH are funded
//	    (Fuji: at the faucet; mainnet: from your own AVAX). No cross-chain
//	    moves: fund each chain directly.
//	fuji-wallet topup [days]                        add <days> (default 3) of
//	    continuous fee to EVERY staking slot's validator balance
//	    (IncreaseL1ValidatorBalanceTx; anyone may fund any validationID).
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/ids"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fujikey"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/netcfg"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/utils/formatting/address"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	"github.com/joho/godotenv"
)

const (
	// The per-validator continuous-fee deposit is netcfg.ValidatorBalance
	// (per-network; create-l1 pays it in ConvertSubnetToL1Tx).
	feeBudget = uint64(100 * units.MilliAvax)
	// requiredCNavax covers the ValidatorManager deploy + initialize +
	// initializeValidatorSet + a generous margin for later weight-seesaw ops.
	// Keep in sync with create-l1's cChainGasBudgetAvax.
	requiredCNavax = uint64(150 * units.MilliAvax)
	// feeFloorNavaxPerSec is the L1 validator continuous-fee floor (512 nAVAX
	// per validator-second, ACP-77). topup asks platform.getValidatorFeeState
	// for the live rate and uses this floor when the rate reads lower or the
	// call fails.
	feeFloorNavaxPerSec = uint64(512)
)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fuji-wallet: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: fuji-wallet <gen|fund|topup> [-key <path>] [-api <uri>] [topup: days]")
	}
	loadEnvFiles()
	net := netcfg.Get()
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	keyPath := fs.String("key", "staking/fuji-wallet.key", "wallet private key file (hex)")
	api := fs.String("api", net.API, "public API base URI")
	_ = fs.Parse(os.Args[2:])

	switch os.Args[1] {
	case "gen":
		gen(*keyPath)
	case "fund":
		fund(net, *keyPath, *api)
	case "topup":
		topup(*keyPath, *api, fs.Args())
	default:
		fatalf("unknown command %q (want gen, fund or topup)", os.Args[1])
	}
}

// loadEnvFiles best-effort loads .env and network.env (exe-relative, matching
// create-l1's lookup, else cwd) so a standalone invocation sees the topology,
// SUBNET_ID and NETWORK without the setup scripts' shell sourcing. godotenv
// never overrides variables already in the environment.
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

func gen(keyPath string) {
	if _, err := os.Stat(keyPath); err == nil {
		fatalf("%s already exists: refusing to overwrite (it may hold funds / own the L1).\n"+
			"Remove it explicitly if you really want a new wallet.", keyPath)
	}
	key, err := secp256k1.NewPrivateKey()
	if err != nil {
		fatalf("generate key: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(key.Bytes())+"\n"), 0o600); err != nil {
		fatalf("write %s: %v", keyPath, err)
	}
	fmt.Printf("Wrote %s (gitignored: NEVER commit this file)\n", keyPath)
	printAddresses(key)
}

func printAddresses(key *secp256k1.PrivateKey) {
	fmt.Printf("  P-chain address: %s\n", pAddress(key))
	fmt.Printf("  C-chain address: %s\n", key.EthAddress().Hex())
}

func pAddress(key *secp256k1.PrivateKey) string {
	pAddr, err := address.Format("P", netcfg.Get().HRP, key.Address().Bytes())
	if err != nil {
		fatalf("format P-chain address: %v", err)
	}
	return pAddr
}

// fund prints both funding targets and polls until each chain holds its
// budget. Idempotent: already-satisfied chains are skipped, so re-runs (and
// topping up just one side) behave sensibly.
func fund(net netcfg.Config, keyPath, api string) {
	key, err := fujikey.Load(keyPath)
	if err != nil {
		fatalf("load wallet key (run ./setup/00_gen_secrets.sh first): %v", err)
	}
	ctx := context.Background()

	requiredP := requiredPBalance(net.ValidatorBalance)
	pBal := pBalance(ctx, api, key.Address())
	cBal := weiToNavax(cBalanceWei(api, key.EthAddress().Hex()))
	fmt.Printf("Current balances:  %s   %s\n\n",
		chainStatus("P", pBal, requiredP), chainStatus("C", cBal, requiredCNavax))
	if pBal >= requiredP && cBal >= requiredCNavax {
		fmt.Println("Both chains already funded. Nothing to do.")
		printAddresses(key)
		return
	}

	fmt.Println("================================================================")
	if net.Name == "fuji" {
		fmt.Println("  FUND AT THE FUJI FAUCET (https://core.app/tools/testnet-faucet/,")
		fmt.Println("  2 AVAX/request - pick the chain per request, no cross-chain moves):")
	} else {
		fmt.Println("  SEND AVAX FROM YOUR OWN WALLET (mainnet, REAL FUNDS;")
		fmt.Println("  send per chain directly, no cross-chain moves):")
	}
	fmt.Println()
	printFaucetTarget("P-Chain", pAddress(key), pBal, requiredP,
		avaxString(net.ValidatorBalance)+" per registered validator + fees")
	printFaucetTarget("C-Chain", key.EthAddress().Hex(), cBal, requiredCNavax, "ValidatorManager deploy + weight ops gas")
	fmt.Println("================================================================")
	fmt.Println()

	fmt.Println("Polling balances (every 5s)...")
	for {
		pBal = pBalance(ctx, api, key.Address())
		cBal = weiToNavax(cBalanceWei(api, key.EthAddress().Hex()))
		if pBal >= requiredP && cBal >= requiredCNavax {
			break
		}
		fmt.Printf("  %s  %s   %s\n", time.Now().Format("15:04:05"),
			chainStatus("P", pBal, requiredP), chainStatus("C", cBal, requiredCNavax))
		time.Sleep(5 * time.Second)
	}

	fmt.Printf("\nDone. P: %s AVAX, C: %s AVAX\n", avaxString(pBal), avaxString(cBal))
	printAddresses(key)
}

func pBalance(ctx context.Context, api string, addr ids.ShortID) uint64 {
	resp, err := platformvm.NewClient(api).GetBalance(ctx, []ids.ShortID{addr})
	if err != nil {
		fatalf("platform.getBalance against %s: %v", api, err)
	}
	return uint64(resp.Balance)
}

// requiredPBalance budgets a continuous-fee deposit for EVERY registered
// validator: all staking slots of both sites (the conversion registers them
// all; failover only moves weight).
func requiredPBalance(validatorBalance uint64) uint64 {
	t, _, err := topo.FromEnv(os.Getenv)
	if err != nil {
		fatalf("topology from env (source .env via the scripts): %v", err)
	}
	return uint64(len(t.StakingSlots()))*validatorBalance + feeBudget
}

// topup adds <days> (default 3) worth of continuous fee to every staking
// slot's validator balance with one IncreaseL1ValidatorBalanceTx each, and
// prints each balance before/after. Chain state (SUBNET_ID) comes from
// network.env, node IDs from staking/node-ids.env next to the wallet key.
func topup(keyPath, api string, args []string) {
	days := 3
	if len(args) > 0 {
		d, err := strconv.Atoi(args[0])
		if err != nil || d <= 0 {
			fatalf("topup: bad days %q (want a positive integer)", args[0])
		}
		days = d
	}
	key, err := fujikey.Load(keyPath)
	if err != nil {
		fatalf("load wallet key: %v", err)
	}
	ctx := context.Background()
	pClient := platformvm.NewClient(api)

	rate := feeFloorNavaxPerSec
	if _, price, _, err := pClient.GetValidatorFeeState(ctx); err == nil && uint64(price) > rate {
		rate = uint64(price)
	}
	amount := uint64(days) * 24 * 3600 * rate

	vids, names, err := stakingValidationIDs(keyPath)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("Top-up: %d day(s) x %d nAVAX/s = %s AVAX per validator (%d validators, %s AVAX total)\n",
		days, rate, avaxString(amount), len(vids), avaxString(amount*uint64(len(vids))))

	wallet, err := primary.MakePWallet(ctx, api, secp256k1fx.NewKeychain(key), primary.WalletConfig{})
	if err != nil {
		fatalf("make P-chain wallet: %v", err)
	}
	for i, vid := range vids {
		before, _, err := pClient.GetL1Validator(ctx, vid)
		if err != nil {
			fatalf("platform.getL1Validator(%s): %v", names[i], err)
		}
		if _, err := wallet.IssueIncreaseL1ValidatorBalanceTx(vid, amount); err != nil {
			fatalf("IncreaseL1ValidatorBalanceTx(%s): %v", names[i], err)
		}
		after, _, err := pClient.GetL1Validator(ctx, vid)
		if err != nil {
			fatalf("platform.getL1Validator(%s) after top-up: %v", names[i], err)
		}
		fmt.Printf("  %-3s %s  balance %s -> %s AVAX\n", names[i], vid, avaxString(before.Balance), avaxString(after.Balance))
	}
}

// stakingValidationIDs derives every registered validator's validationID the
// same way cmd/reconcile does: the conversion tx sorted validators by NodeID
// bytes, so the conversion index is recomputed from the committed NodeIDs.
func stakingValidationIDs(keyPath string) ([]ids.ID, []string, error) {
	subnetStr := os.Getenv("SUBNET_ID")
	if subnetStr == "" {
		return nil, nil, fmt.Errorf("SUBNET_ID not set (network.env missing? run from the repo root)")
	}
	subnetID, err := ids.FromString(subnetStr)
	if err != nil {
		return nil, nil, fmt.Errorf("parse SUBNET_ID: %w", err)
	}
	t, _, err := topo.FromEnv(os.Getenv)
	if err != nil {
		return nil, nil, fmt.Errorf("topology from env: %w", err)
	}
	nodeIDsPath := filepath.Join(filepath.Dir(keyPath), "node-ids.env")
	vars, err := godotenv.Read(nodeIDsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", nodeIDsPath, err)
	}
	slots := t.StakingSlots()
	nodeIDs := make([]ids.NodeID, len(slots))
	names := make([]string, len(slots))
	for i, s := range slots {
		k := t.KeyOf(s)
		id, err := ids.NodeIDFromString(strings.TrimSpace(vars[fmt.Sprintf("L1_%d_NODE_ID", k)]))
		if err != nil {
			return nil, nil, fmt.Errorf("parse L1_%d_NODE_ID: %w", k, err)
		}
		nodeIDs[i] = id
		names[i] = t.MachineName(s)
	}
	conv := valmgr.ConversionIndices(nodeIDs)
	vids := make([]ids.ID, len(slots))
	for i := range slots {
		vids[i] = valmgr.ValidationID(subnetID, uint32(conv[i]))
	}
	return vids, names, nil
}

// cBalanceWei fetches the EVM account balance with a raw eth_getBalance call
// (stdlib only: not worth a client dependency for one method).
func cBalanceWei(api, addr string) *big.Int {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_getBalance",
		"params": []any{addr, "latest"},
	})
	if err != nil {
		fatalf("marshal eth_getBalance: %v", err)
	}
	resp, err := http.Post(api+"/ext/bc/C/rpc", "application/json", bytes.NewReader(body))
	if err != nil {
		fatalf("eth_getBalance against %s: %v", api, err)
	}
	defer resp.Body.Close()
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		fatalf("decode eth_getBalance response: %v", err)
	}
	if out.Error != nil {
		fatalf("eth_getBalance: %s", out.Error.Message)
	}
	wei, ok := new(big.Int).SetString(strings.TrimPrefix(out.Result, "0x"), 16)
	if !ok {
		fatalf("eth_getBalance: bad result %q", out.Result)
	}
	return wei
}

// weiToNavax converts an 18-decimal EVM balance to 9-decimal nAVAX.
func weiToNavax(wei *big.Int) uint64 {
	navax := new(big.Int).Div(wei, big.NewInt(1_000_000_000))
	if !navax.IsUint64() {
		fatalf("balance overflows uint64 nAVAX: %s wei", wei)
	}
	return navax.Uint64()
}

func avaxString(navax uint64) string {
	return fmt.Sprintf("%d.%02d", navax/units.Avax, navax%units.Avax/(units.Avax/100))
}

// chainStatus renders one chain's funded state: ✅ once it meets the need,
// ⏳ while it is short.
func chainStatus(label string, bal, need uint64) string {
	mark := "⏳"
	if bal >= need {
		mark = "✅"
	}
	return fmt.Sprintf("%s %s %s/%s AVAX", mark, label, avaxString(bal), avaxString(need))
}

// printFaucetTarget always shows a chain's address (so both are copyable); it
// only asks for a faucet drop on the chain that is still short.
func printFaucetTarget(label, addr string, bal, need uint64, note string) {
	if bal >= need {
		fmt.Printf("    ✅ %-8s %s  (funded)\n", label, addr)
		return
	}
	fmt.Printf("    ⏳ %-8s %s\n", label, addr)
	fmt.Printf("                 need %s AVAX (%s)\n", avaxString(need), note)
}
