package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ava-labs/avalanchego/ids"
)

// killDC1 ssh-pkills avalanchego on every DC1 host, in parallel. The
// remote shell uses `pkill -9 || true` so this is idempotent: re-running
// when DC1 is already dead is harmless.
func killDC1(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	rem := newRemote(cfg.sshUser, cfg.sshKey)

	fmt.Println("=== kill DC1 (SIGKILL, modelling catastrophic death) ===")
	return fanOut(ctx, cfg.dc1IPs, func(ctx context.Context, host string) error {
		fmt.Printf("  [dc1/%s] SIGKILL avalanchego\n", host)
		_, err := rem.run(ctx, host, killAvalanchegoHardSnippet)
		return err
	})
}

// dc2Takeover kills DC1 (idempotently), then on each DC2 host: pkills the
// running avalanchego, replaces the staking dir with the paired DC1
// staking key set, and restarts avalanchego with the same flags as
// system-start. The chain DB on DC2 is preserved -- DC2 was tracking the
// L1 as a follower, so it already has the chain history; it only needs a
// new staking identity to be recognized as a registered L1 validator.
//
// Pairing: DC2[i] inherits the keys of DC1[i] (1-based for the on-disk
// dirs).
func dc2Takeover(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	res, err := loadNetworkEnv(cfg)
	if err != nil {
		return err
	}
	rem := newRemote(cfg.sshUser, cfg.sshKey)

	// 1. Kill DC1 first (idempotent).
	if err := killDC1(ctx); err != nil {
		return err
	}

	// 2. Key-swap and restart on each DC2 host in parallel.
	fmt.Println()
	fmt.Println("=== DC2 takeover: key-swap + restart ===")
	dc2Idx := make(map[string]int, len(cfg.dc2IPs))
	for i, ip := range cfg.dc2IPs {
		dc2Idx[ip] = i + 1 // 1-based; DC2[i] inherits DC1[i] keys
	}

	if err := fanOut(ctx, cfg.dc2IPs, func(ctx context.Context, host string) error {
		idx := dc2Idx[host]
		fmt.Printf("  [dc2/%s] stopping + replacing staking keys with dc1/%d\n", host, idx)

		// 2a. Gracefully stop the existing process and clear the (auto-
		// generated) staking dir. SIGTERM (with a 30s window before
		// SIGKILL fallback) gives avalanchego time to flush pebble and
		// release its chainData flock cleanly -- the next process start
		// for this same data dir would otherwise race for the LOCK file.
		// Chain DB is preserved.
		stop := fmt.Sprintf(`
			set -e
			%[2]s
			rm -f %[1]s/staking/signer.key %[1]s/staking/staker.crt %[1]s/staking/staker.key
			mkdir -p %[1]s/staking
		`, remoteDir, stopAvalanchegoGracefulSnippet)
		if _, err := rem.run(ctx, host, stop); err != nil {
			return err
		}

		// 2b. scp DC1's staking key set into DC2's staking dir.
		src := filepath.Join(cfg.stakingDir, "dc1", fmt.Sprintf("%d", idx))
		for _, f := range []string{"signer.key", "staker.crt", "staker.key"} {
			if err := rem.scpUp(ctx, host, filepath.Join(src, f), remoteDir+"/staking/"); err != nil {
				return err
			}
		}

		// 2c. Restart avalanchego with the same flags as system-start.
		if _, err := rem.run(ctx, host, chainNodeStartScript(cfg, host, res)); err != nil {
			return err
		}
		fmt.Printf("  [dc2/%s] restarted with NodeID = %s\n", host, cfg.dc1NodeIDs[idx-1])
		return nil
	}); err != nil {
		return err
	}

	// 3. Wait for /ext/health on all 5 DC2 nodes. With sybil disabled and
	// the L1 chain DB already in place, this should resume quickly once the
	// 5 DC2 nodes peer via primary network and reach AlphaConfidence on the
	// L1.
	fmt.Println()
	fmt.Println("=== waiting for L1 health on DC2 (DC1 is dead) ===")
	if err := fanOut(ctx, cfg.dc2IPs, func(ctx context.Context, host string) error {
		return waitHealthy(ctx, host, httpPort, healthTimeout, host)
	}); err != nil {
		return err
	}

	// 4. Print the surviving RPCs.
	fmt.Println()
	fmt.Println("=== DC2 is now the L1 validator set ===")
	fmt.Printf("subnet: %s\nchain:  %s\n\n", res.SubnetID, res.ChainID)
	fmt.Println("L1 RPCs (5 DC2 hosts now running with DC1 staking keys):")
	for i, ip := range cfg.dc2IPs {
		fmt.Printf("  dc2-%d (was %s, now %s):\n    http://%s:%d/ext/bc/%s/rpc\n",
			i+1, "(auto-gen NodeID)", cfg.dc1NodeIDs[i], ip, httpPort, res.ChainID)
	}
	return nil
}

func loadNetworkEnv(cfg *config) (*l1Result, error) {
	path := filepath.Join(cfg.repoRoot, "network.env")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s (run `failover system-start` first): %w", path, err)
	}
	res := &l1Result{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "SUBNET_ID":
			res.SubnetID, err = ids.FromString(v)
			if err != nil {
				return nil, fmt.Errorf("parse SUBNET_ID: %w", err)
			}
		case "CHAIN_ID":
			res.ChainID, err = ids.FromString(v)
			if err != nil {
				return nil, fmt.Errorf("parse CHAIN_ID: %w", err)
			}
		}
	}
	if res.SubnetID == ids.Empty || res.ChainID == ids.Empty {
		return nil, fmt.Errorf("network.env missing SUBNET_ID or CHAIN_ID")
	}
	return res, nil
}
