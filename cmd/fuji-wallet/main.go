// Command fuji-wallet manages the per-deploy Fuji fund/fee wallet (the key
// that pays for subnet/chain creation and validator continuous fees on Fuji's
// public P-chain, and for the ValidatorManager contract gas on the Fuji
// C-chain). The key is GENERATED per deploy and gitignored: never committed
// (see FUJI_PLAN.md "KEY POLICY").
//
//	fuji-wallet gen  -key staking/fuji-wallet.key   generate the key, print addresses
//	fuji-wallet fund -key staking/fuji-wallet.key   print the P- and C-chain
//	    addresses with the required amounts and poll until BOTH are funded at
//	    the faucet. No cross-chain moves: fund each chain directly.
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
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/constants"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fujikey"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/utils/formatting/address"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm"
)

const (
	defaultAPI = "https://api.avax-test.network"
	// Keep in sync with cmd/create-l1/main.go. This is the continuous-fee
	// balance per registered validator paid by ConvertSubnetToL1Tx.
	validatorBalance = uint64(100 * units.MilliAvax)
	feeBudget        = uint64(100 * units.MilliAvax)
	// requiredCNavax covers the ValidatorManager deploy + initialize +
	// initializeValidatorSet + a generous margin for later weight-seesaw ops.
	// Keep in sync with create-l1's cChainGasBudgetAvax.
	requiredCNavax = uint64(150 * units.MilliAvax)
)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fuji-wallet: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: fuji-wallet <gen|fund> -key <path> [-api <uri>]")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	keyPath := fs.String("key", "staking/fuji-wallet.key", "wallet private key file (hex)")
	api := fs.String("api", envOr("PCHAIN_API", defaultAPI), "public Fuji API base URI")
	_ = fs.Parse(os.Args[2:])

	switch os.Args[1] {
	case "gen":
		gen(*keyPath)
	case "fund":
		fund(*keyPath, *api)
	default:
		fatalf("unknown command %q (want gen or fund)", os.Args[1])
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
	pAddr, err := address.Format("P", constants.FujiHRP, key.Address().Bytes())
	if err != nil {
		fatalf("format P-chain address: %v", err)
	}
	return pAddr
}

// fund prints both funding targets and polls until each chain holds its
// budget. Idempotent: already-satisfied chains are skipped, so re-runs (and
// topping up just one side) behave sensibly.
func fund(keyPath, api string) {
	key, err := fujikey.Load(keyPath)
	if err != nil {
		fatalf("load wallet key (run ./00_gen_secrets.sh first): %v", err)
	}
	ctx := context.Background()

	requiredP := requiredPBalance()
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
	fmt.Println("  FUND AT THE FUJI FAUCET (https://core.app/tools/testnet-faucet/,")
	fmt.Println("  2 AVAX/request — pick the chain per request, no cross-chain moves):")
	fmt.Println()
	printFaucetTarget("P-Chain", pAddress(key), pBal, requiredP, "0.1 per registered validator + fees")
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
func requiredPBalance() uint64 {
	t, _, err := topo.FromEnv(os.Getenv)
	if err != nil {
		fatalf("topology from env (source .env via the scripts): %v", err)
	}
	return uint64(len(t.StakingSlots()))*validatorBalance + feeBudget
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
	return fmt.Sprintf("%d.%09d", navax/units.Avax, navax%units.Avax)
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
