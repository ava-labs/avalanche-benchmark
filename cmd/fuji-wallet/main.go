// Command fuji-wallet manages the per-deploy fund/fee wallet (the key that
// pays for subnet/chain creation and validator continuous fees on the anchor
// network's P-chain). The key is GENERATED per deploy and gitignored: never
// committed (see FUJI_PLAN.md "KEY POLICY"). Everything is P-chain only:
// there is no ValidatorManager contract and no C-chain gas to fund.
//
//	fuji-wallet gen  -key staking/fuji-wallet.key   generate the key, print addresses
//	fuji-wallet fund -key staking/fuji-wallet.key   print the P-chain address
//	    with the required amount and poll until funded (Fuji: at the faucet;
//	    mainnet: from your own AVAX).
//	fuji-wallet topup [days]                        top up EVERY staking
//	    slot's validator balance so each has at least <days> (default 3) of
//	    continuous-fee runway (IncreaseL1ValidatorBalanceTx; anyone may fund
//	    any validationID). Validators already at or above the target are
//	    untouched; deficits are rounded down to whole days, so re-running
//	    right away is a no-op.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ava-labs/avalanchego/ids"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fujikey"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/netcfg"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/vset"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/utils/formatting/address"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	pwallet "github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	"github.com/joho/godotenv"
)

const (
	// The per-validator continuous-fee deposit is netcfg.ValidatorBalance
	// (per-network; `l1 create` pays it in ConvertSubnetToL1Tx).
	feeBudget = uint64(100 * units.MilliAvax)
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
		fatalf("usage: fuji-wallet <gen|fund|topup> [-key <path>] [-api <uri>] [topup: target days of runway]")
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

// fund prints the P-chain funding target and polls until the wallet holds
// its budget. Idempotent: an already-funded wallet is a no-op, so re-runs
// behave sensibly. P-chain only: chain creation, validator deposits and
// weight txs all pay from here (there is no contract gas to fund).
func fund(net netcfg.Config, keyPath, api string) {
	key, err := fujikey.Load(keyPath)
	if err != nil {
		fatalf("load wallet key (run ./setup/00_gen_secrets.sh first): %v", err)
	}
	ctx := context.Background()

	// The chain already exists: creation funding is moot, and the default
	// pre-create budget (which assumes standard deposits) can exceed the
	// leftover balance and poll forever. Point at the post-create tools.
	if os.Getenv("SUBNET_ID") != "" {
		fmt.Printf("network.env records SUBNET_ID=%s: the chain is already created.\n", os.Getenv("SUBNET_ID"))
		fmt.Printf("Current P balance: %s AVAX. Creation funding is done; from here use\n", avaxString(pBalance(ctx, api, key.Address())))
		fmt.Println("`l1 status` to see validator/committee runway and `fuji-wallet topup` to extend it.")
		printAddresses(key)
		return
	}

	requiredP := requiredPBalance(net.ValidatorBalance)
	pBal := pBalance(ctx, api, key.Address())
	fmt.Printf("Current balance:  %s\n\n", chainStatus("P", pBal, requiredP))
	if pBal >= requiredP {
		fmt.Println("Already funded. Nothing to do.")
		printAddresses(key)
		return
	}

	fmt.Println("================================================================")
	if net.Name == "fuji" {
		fmt.Println("  FUND AT THE FUJI FAUCET (https://core.app/tools/testnet-faucet/,")
		fmt.Println("  2 AVAX/request, P-Chain):")
	} else {
		fmt.Println("  SEND AVAX FROM YOUR OWN WALLET (mainnet, REAL FUNDS; P-Chain):")
	}
	fmt.Println()
	printFaucetTarget("P-Chain", pAddress(key), pBal, requiredP,
		avaxString(net.ValidatorBalance)+" per registered validator + fees")
	fmt.Println("================================================================")
	fmt.Println()

	fmt.Println("Polling balance (every 5s)...")
	for {
		pBal = pBalance(ctx, api, key.Address())
		if pBal >= requiredP {
			break
		}
		fmt.Printf("  %s  %s\n", time.Now().Format("15:04:05"), chainStatus("P", pBal, requiredP))
		time.Sleep(5 * time.Second)
	}

	fmt.Printf("\nDone. P: %s AVAX\n", avaxString(pBal))
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
// validator: all role=validator nodes of the inventory (the conversion
// registers them all; failover only moves weight), plus the default manager-L1
// signing committee that `l1 create` also funds. Approximate: a create run
// with a non-default --committee / --committee-balance shifts this, and the
// create pre-flight is the authoritative gate.
func requiredPBalance(validatorBalance uint64) uint64 {
	nodes, err := topo.LoadNear()
	if err != nil {
		fatalf("%v", err)
	}
	committee := uint64(netcfg.DefaultCommittee) * netcfg.DefaultCommitteeBalance
	return uint64(len(topo.Validators(nodes)))*validatorBalance + committee + feeBudget
}

// topup brings every staking slot's validator balance up to at least
// <target> (default 3) days of continuous-fee runway, one
// IncreaseL1ValidatorBalanceTx per short validator. Chain state (SUBNET_ID)
// comes from network.env, node IDs from staking/node-ids.env next to the
// wallet key.
func topup(keyPath, api string, args []string) {
	target := 3
	if len(args) > 0 {
		d, err := strconv.Atoi(args[0])
		if err != nil || d <= 0 {
			fatalf("topup: bad days %q (want a positive integer)", args[0])
		}
		target = d
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
	perDay := rate * 24 * 3600

	vids, names, err := stakingValidationIDs(ctx, pClient, keyPath)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("Top-up target: %d day(s) of runway at %d nAVAX/s (%s AVAX per validator-day)\n",
		target, rate, avaxString(perDay))

	var wallet pwallet.Wallet // made lazily: a fully-topped-up fleet needs none
	var total uint64
	for i, vid := range vids {
		v, _, err := pClient.GetL1Validator(ctx, vid)
		if err != nil {
			fatalf("platform.getL1Validator(%s): %v", names[i], err)
		}
		runway := float64(v.Balance) / float64(perDay)
		add := topupDeficitDays(v.Balance, rate, target)
		if add == 0 {
			fmt.Printf("  %-3s %5.1f days  ok\n", names[i], runway)
			continue
		}
		if wallet == nil {
			if wallet, err = primary.MakePWallet(ctx, api, secp256k1fx.NewKeychain(key), primary.WalletConfig{}); err != nil {
				fatalf("make P-chain wallet: %v", err)
			}
		}
		amount := uint64(add) * perDay
		if _, err := wallet.IssueIncreaseL1ValidatorBalanceTx(vid, amount); err != nil {
			fatalf("IncreaseL1ValidatorBalanceTx(%s): %v", names[i], err)
		}
		total += amount
		fmt.Printf("  %-3s %5.1f days  +%d day(s) = %s AVAX\n", names[i], runway, add, avaxString(amount))
	}
	if total == 0 {
		fmt.Printf("all validators at or above %d days\n", target)
	} else {
		fmt.Printf("total spent: %s AVAX\n", avaxString(total))
	}
}

// topupDeficitDays returns the whole days of continuous fee to add so a
// validator's balance reaches at least targetDays of runway at ratePerSec.
// The deficit is rounded DOWN to whole days, so sub-day deficits return 0
// and re-running minutes after a top-up is a no-op.
func topupDeficitDays(balance, ratePerSec uint64, targetDays int) int {
	need := uint64(targetDays) * ratePerSec * 24 * 3600
	if balance >= need {
		return 0
	}
	return int((need - balance) / (ratePerSec * 24 * 3600))
}

// stakingValidationIDs reads every registered validator's validationID
// straight from the P-chain (the same set cmd/l1 manages) and names each one
// via the committed manifest, so no conversion-order derivation can ever
// drift from chain reality.
func stakingValidationIDs(ctx context.Context, pc *platformvm.Client, keyPath string) ([]ids.ID, []string, error) {
	subnetStr := os.Getenv("SUBNET_ID")
	if subnetStr == "" {
		return nil, nil, fmt.Errorf("SUBNET_ID not set (network.env missing? run from the repo root)")
	}
	subnetID, err := ids.FromString(subnetStr)
	if err != nil {
		return nil, nil, fmt.Errorf("parse SUBNET_ID: %w", err)
	}
	vs, err := vset.Fetch(ctx, pc, subnetID, 1)
	if err != nil {
		return nil, nil, err
	}
	names := map[string]string{}
	if entries, err := vset.ReadManifest(filepath.Dir(keyPath)); err == nil {
		for _, e := range entries {
			names[e.NodeID.String()] = e.Name
		}
	}
	vids := make([]ids.ID, len(vs))
	nameList := make([]string, len(vs))
	for i, v := range vs {
		vids[i] = v.ValidationID
		nameList[i] = names[v.NodeID.String()]
		if nameList[i] == "" {
			nameList[i] = v.NodeID.String()
		}
	}
	return vids, nameList, nil
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
