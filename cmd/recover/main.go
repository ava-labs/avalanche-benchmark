// Command recover is a throwaway P-chain reclaim tool for the mainnet L1
// teardown: it reads each L1 validator's owners and issues DisableL1ValidatorTx
// (which refunds the remaining continuous-fee balance to the
// remainingBalanceOwner with no last-validator restriction). Delete after use.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/formatting/address"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fujikey"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/netcfg"
)

func fatalf(f string, a ...any) { fmt.Fprintf(os.Stderr, "recover: "+f+"\n", a...); os.Exit(1) }

func main() {
	// Match the kit's env lookup so NETWORK/SUBNET_ID/API_TOKEN/PCHAIN_API apply.
	_ = godotenv.Load(".env")
	_ = godotenv.Load("network.env")

	if len(os.Args) < 2 {
		fatalf("usage: recover <inspect|disable <validationID>>")
	}
	ctx := context.Background()
	cfg := netcfg.Get()
	subnetID, err := ids.FromString(os.Getenv("SUBNET_ID"))
	if err != nil {
		fatalf("parse SUBNET_ID: %v", err)
	}
	pc := platformvm.NewClient(cfg.API)

	switch os.Args[1] {
	case "inspect":
		inspect(ctx, cfg, pc, subnetID)
	case "disable":
		if len(os.Args) < 3 {
			fatalf("disable: need a validationID")
		}
		vid, err := ids.FromString(os.Args[2])
		if err != nil {
			fatalf("parse validationID: %v", err)
		}
		disable(ctx, cfg, pc, vid)
	default:
		fatalf("unknown command %q", os.Args[1])
	}
}

func fmtOwner(hrp string, o *secp256k1fx.OutputOwners) string {
	addrs := make([]string, len(o.Addrs))
	for i, a := range o.Addrs {
		s, _ := address.Format("P", hrp, a.Bytes())
		addrs[i] = s
	}
	return fmt.Sprintf("threshold=%d locktime=%d addrs=%v", o.Threshold, o.Locktime, addrs)
}

func walletAddr(cfg netcfg.Config) (ids.ShortID, string) {
	key, err := fujikey.Load(filepath.Join("staking", "fuji-wallet.key"))
	if err != nil {
		fatalf("load wallet key: %v", err)
	}
	a := key.Address()
	s, _ := address.Format("P", cfg.HRP, a.Bytes())
	return a, s
}

func balance(ctx context.Context, pc *platformvm.Client, a ids.ShortID) uint64 {
	resp, err := pc.GetBalance(ctx, []ids.ShortID{a})
	if err != nil {
		fatalf("getBalance: %v", err)
	}
	return uint64(resp.Balance)
}

func inspect(ctx context.Context, cfg netcfg.Config, pc *platformvm.Client, subnetID ids.ID) {
	wa, ws := walletAddr(cfg)
	fmt.Printf("network=%s api=%s\n", cfg.Name, cfg.API)
	fmt.Printf("wallet P-addr=%s balance=%d nAVAX\n", ws, balance(ctx, pc, wa))
	vs, err := pc.GetCurrentValidators(ctx, subnetID, nil)
	if err != nil {
		fatalf("getCurrentValidators: %v", err)
	}
	fmt.Printf("subnet %s: %d validators\n", subnetID, len(vs))
	for _, v := range vs {
		if v.ValidationID == nil {
			fmt.Printf("  %s: NO validationID\n", v.NodeID)
			continue
		}
		l1v, _, err := pc.GetL1Validator(ctx, *v.ValidationID)
		if err != nil {
			fatalf("getL1Validator(%s): %v", *v.ValidationID, err)
		}
		fmt.Printf("  nodeID=%s vid=%s weight=%d balance=%d nAVAX minNonce=%d\n",
			v.NodeID, *v.ValidationID, l1v.Weight, l1v.Balance, l1v.MinNonce)
		fmt.Printf("      remainingBalanceOwner: %s\n", fmtOwner(cfg.HRP, l1v.RemainingBalanceOwner))
		fmt.Printf("      deactivationOwner:     %s\n", fmtOwner(cfg.HRP, l1v.DeactivationOwner))
	}
}

// disableRetries x disableDelay bounds the retry on a public-API stale read.
// Back-to-back reclaims hit "failed to read consumed UTXO ... not found": a
// load-balanced backend that has not yet seen the UTXO the previous tx just
// consumed. Re-making the wallet re-reads the chain's UTXO state, so a short
// bounded retry lets the backend catch up instead of silently skipping a
// validator.
const (
	disableRetries = 4
	disableDelay   = 4 * time.Second
)

func disable(ctx context.Context, cfg netcfg.Config, pc *platformvm.Client, vid ids.ID) {
	key, err := fujikey.Load(filepath.Join("staking", "fuji-wallet.key"))
	if err != nil {
		fatalf("load wallet key: %v", err)
	}
	var lastErr error
	for attempt := 1; attempt <= disableRetries; attempt++ {
		if attempt > 1 {
			fmt.Printf("  retry %d/%d after transient (%v)\n", attempt, disableRetries, lastErr)
			time.Sleep(disableDelay)
		}
		// Preload the validationID so the wallet backend knows its deactivation
		// owner for the DisableAuth, and re-read UTXO state on every attempt.
		w, err := primary.MakePWallet(ctx, cfg.API, secp256k1fx.NewKeychain(key),
			primary.WalletConfig{ValidationIDs: []ids.ID{vid}})
		if err != nil {
			lastErr = err
			if isStaleRead(err) {
				continue
			}
			fatalf("make wallet: %v", err)
		}
		tx, err := w.IssueDisableL1ValidatorTx(vid)
		if err != nil {
			lastErr = err
			if isStaleRead(err) {
				continue
			}
			fatalf("issue DisableL1ValidatorTx(%s): %v", vid, err)
		}
		fmt.Printf("disabled vid=%s txID=%s\n", vid, tx.ID())
		return
	}
	fatalf("issue DisableL1ValidatorTx(%s): gave up after %d attempts: %v", vid, disableRetries, lastErr)
}

// isStaleRead matches the load-balanced public-API stale read seen on
// back-to-back reclaims: a backend serving state that predates the UTXO the
// previous tx just consumed.
func isStaleRead(err error) bool {
	return err != nil && strings.Contains(err.Error(), "failed to read consumed UTXO")
}
