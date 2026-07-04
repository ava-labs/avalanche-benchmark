// Command fuji-wallet manages the per-deploy Fuji fund/fee wallet (the key that
// pays for subnet/chain creation and validator continuous fees on Fuji's public
// P-chain). The key is GENERATED per deploy and gitignored: never committed
// (see FUJI_PLAN.md "KEY POLICY").
//
//	fuji-wallet gen  -key staking/fuji-wallet.key   generate the key, print addresses
//	fuji-wallet fund -key staking/fuji-wallet.key   faucet flow: print the C-chain
//	    address, poll until the (manual, C-chain-only) faucet transfer arrives,
//	    then automatically move everything C -> P (export + import), so the
//	    P-chain wallet ends up funded with zero extra user steps.
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
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/utils/formatting/address"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
)

const (
	defaultAPI = "https://api.avax-test.network"
	// exportFeeMargin is left on the C-chain to cover the atomic export fee
	// (dynamic base fee; observed well under 0.005 AVAX).
	exportFeeMargin = uint64(10 * units.MilliAvax) // nAVAX
	// minFundNavax is the smallest C-chain balance worth moving; below it the
	// poll keeps waiting (a faucet drip is 2 AVAX, so this only filters dust).
	minFundNavax = uint64(100 * units.MilliAvax)
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
	pAddr, err := address.Format("P", constants.FujiHRP, key.Address().Bytes())
	if err != nil {
		fatalf("format P-chain address: %v", err)
	}
	fmt.Printf("  P-chain address: %s\n", pAddr)
	fmt.Printf("  C-chain address: %s\n", key.EthAddress().Hex())
}

func fund(keyPath, api string) {
	key, err := fujikey.Load(keyPath)
	if err != nil {
		fatalf("load wallet key (run ./00_gen_secrets.sh first): %v", err)
	}
	ctx := context.Background()
	cAddr := key.EthAddress().Hex()

	// Report current P-chain balance so re-runs / already-funded wallets are obvious.
	pBal := pBalance(ctx, api, key.Address())
	fmt.Printf("Current P-chain balance: %s AVAX\n\n", avaxString(pBal))

	fmt.Println("================================================================")
	fmt.Println("  FUND THIS ADDRESS AT THE FUJI FAUCET (C-chain: there is no")
	fmt.Println("  P-chain faucet; this tool moves the funds C -> P for you):")
	fmt.Println()
	fmt.Printf("      %s\n", cAddr)
	fmt.Println()
	fmt.Println("  Faucet: https://core.app/tools/testnet-faucet/  (2 AVAX/request)")
	fmt.Println("  Budget: ~0.1 AVAX fees + 1 AVAX per validator (continuous fee,")
	fmt.Println("          lasts ~50-60 days). If the P-chain balance above already")
	fmt.Println("          covers that, Ctrl-C now: nothing more to do.")
	fmt.Println("================================================================")
	fmt.Println()

	fmt.Println("Polling C-chain balance (every 5s)...")
	var cBalNavax uint64
	for {
		wei := cBalanceWei(api, cAddr)
		cBalNavax = weiToNavax(wei)
		if cBalNavax >= minFundNavax {
			break
		}
		fmt.Printf("  %s  C balance: %s AVAX: waiting for faucet\n", time.Now().Format("15:04:05"), avaxString(cBalNavax))
		time.Sleep(5 * time.Second)
	}
	fmt.Printf("  C balance arrived: %s AVAX\n\n", avaxString(cBalNavax))

	exportAmt := cBalNavax - exportFeeMargin
	fmt.Printf("Moving %s AVAX C -> P (leaving %s AVAX for the export fee)...\n",
		avaxString(exportAmt), avaxString(exportFeeMargin))

	// The wallet snapshots UTXO/account state at creation, so build it only now,
	// after the faucet funds are on-chain.
	kc := secp256k1fx.NewKeychain(key)
	wallet, err := primary.MakeWallet(ctx, api, kc, kc, primary.WalletConfig{})
	if err != nil {
		fatalf("create wallet: %v", err)
	}
	owner := secp256k1fx.OutputOwners{
		Threshold: 1,
		Addrs:     []ids.ShortID{key.Address()},
	}

	fmt.Println("[1/2] C-chain ExportTx -> P...")
	exportTx, err := wallet.C().IssueExportTx(constants.PlatformChainID, []*secp256k1fx.TransferOutput{{
		Amt:          exportAmt,
		OutputOwners: owner,
	}})
	if err != nil {
		fatalf("export C->P: %v", err)
	}
	fmt.Printf("  accepted: %s\n", exportTx.ID())

	fmt.Println("[2/2] P-chain ImportTx...")
	cChainID := wallet.C().Builder().Context().BlockchainID
	// The exported atomic UTXO can take a moment to appear on the API node's
	// shared memory; retry briefly instead of failing the whole run.
	var importErr error
	for attempt := 1; attempt <= 5; attempt++ {
		var tx interface{ ID() ids.ID }
		tx, importErr = wallet.P().IssueImportTx(cChainID, &owner)
		if importErr == nil {
			fmt.Printf("  accepted: %s\n", tx.ID())
			break
		}
		fmt.Printf("  attempt %d/5 failed (%v), retrying in 5s...\n", attempt, importErr)
		time.Sleep(5 * time.Second)
	}
	if importErr != nil {
		fatalf("import to P: %v", importErr)
	}

	fmt.Printf("\nDone. P-chain balance: %s AVAX\n", avaxString(pBalance(ctx, api, key.Address())))
	printAddresses(key)
}

func pBalance(ctx context.Context, api string, addr ids.ShortID) uint64 {
	resp, err := platformvm.NewClient(api).GetBalance(ctx, []ids.ShortID{addr})
	if err != nil {
		fatalf("platform.getBalance against %s: %v", api, err)
	}
	return uint64(resp.Balance)
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
