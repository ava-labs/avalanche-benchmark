package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	remoteDir       = "/data/avalanche-benchmark"
	controlDataRoot = "/data/avalanche-benchmark"
	httpPort        = 9650
	stakingPort     = 9651
	control1HTTP    = 9650
	control1Staking = 9651
	control2HTTP    = 9652
	control2Staking = 9653

	healthTimeout = 5 * time.Minute
)

// killAvalanchegoHardSnippet sends SIGKILL and waits for the process
// to be gone. Use this when modelling catastrophic node death (kill-dc1)
// or when we're about to wipe the data dir on the next line anyway
// (system-start reset). After SIGKILL the kernel still has to finish
// releasing file descriptors, including pebble's flock on chainData --
// that's why we poll pgrep until empty (~30s bound) and then add 1s of
// slack. Match "bin/avalanchego" specifically so we don't catch unrelated
// processes.
const killAvalanchegoHardSnippet = `
	pkill -9 -f 'bin/avalanchego' 2>/dev/null || true
	for _i in $(seq 1 30); do
		pgrep -f 'bin/avalanchego' >/dev/null || break
		sleep 1
	done
	sleep 1
`

// stopAvalanchegoGracefulSnippet sends SIGTERM and waits up to 2 minutes
// for avalanchego to flush pebble, release locks, and close peers
// cleanly. Use this for deliberate transitions where the node is healthy
// and we want a clean handoff (dc2-takeover key-swap).
//
// We do NOT escalate to SIGKILL if SIGTERM hangs. DC2 hosts hold L1 chain
// state we want preserved across the takeover; SIGKILL on a healthy
// follower can leave pebble in a half-flushed state and lose the
// chainData flock release timing window. If graceful shutdown stalls
// past the deadline, fail loudly and let the operator look. SIGKILL on
// DC2 is allowed only in the cleanup/wipe path (system-start reset),
// which uses killAvalanchegoHardSnippet instead.
const stopAvalanchegoGracefulSnippet = `
	pkill -TERM -f 'bin/avalanchego' 2>/dev/null || true
	for _i in $(seq 1 240); do
		pgrep -f 'bin/avalanchego' >/dev/null || break
		sleep 0.5
	done
	if pgrep -f 'bin/avalanchego' >/dev/null; then
		echo "ERROR: avalanchego did not exit within 120s of SIGTERM" >&2
		echo "       refusing to SIGKILL: would risk pebble corruption on DC2 state" >&2
		exit 1
	fi
	sleep 1
`

func systemStart(ctx context.Context, arch archSpec) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.arch = arch
	fmt.Printf("=== arch: %s (%d DC1 validator(s) + %d DC2 validator(s)) ===\n",
		arch, arch.dc1, arch.dc2)

	// 0. Sanity: the binaries we need exist locally.
	for _, p := range []string{
		filepath.Join(cfg.binDir, "avalanchego"),
		filepath.Join(cfg.binDir, cfg.subnetEvmID),
	} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("missing %s -- run `make` first", p)
		}
	}

	// 1. Wipe local control state and start the 2 primary network processes.
	if err := wipeControlState(); err != nil {
		return fmt.Errorf("wipe control state: %w", err)
	}
	fmt.Println("=== phase 1: start 2 primary-network avalanchego processes on control ===")
	if err := startControlPrimary(ctx, cfg, 1, control1HTTP, control1Staking, "", ""); err != nil {
		return err
	}
	if err := waitHealthy(ctx, "127.0.0.1", control1HTTP, healthTimeout, "control-1"); err != nil {
		return err
	}
	fmt.Println("  control-1 healthy")
	if err := startControlPrimary(ctx, cfg, 2, control2HTTP, control2Staking,
		fmt.Sprintf("%s:%d", cfg.controlIP, control1Staking), cfg.control1NodeID); err != nil {
		return err
	}
	if err := waitHealthy(ctx, "127.0.0.1", control2HTTP, healthTimeout, "control-2"); err != nil {
		return err
	}
	fmt.Println("  control-2 healthy")

	// 2. Run create-l1 against control-1's primary RPC.
	fmt.Println()
	fmt.Println("=== phase 2: create the L1 on P-chain ===")
	res, err := createL1(ctx, cfg, fmt.Sprintf("http://127.0.0.1:%d", control1HTTP))
	if err != nil {
		return err
	}
	fmt.Println("  subnet:", res.SubnetID)
	fmt.Println("  chain: ", res.ChainID)

	// Persist for downstream commands (kill-dc1 / dc2-takeover).
	if err := writeNetworkEnv(cfg, res); err != nil {
		return err
	}

	// 3. Boot all 10 chain nodes in parallel: wipe, scp, start.
	fmt.Println()
	fmt.Println("=== phase 3: boot 10 chain nodes ===")
	rem := newRemote(cfg.sshUser, cfg.sshKey)
	if err := bootChainNodes(ctx, cfg, rem, res); err != nil {
		return err
	}

	// 4. Wait for L1 RPC to be ready on all 10 chain nodes (health on the
	// Avalanche endpoint covers primary-network bootstrap; we also need the
	// L1 chain itself to be serving).
	fmt.Println()
	fmt.Println("=== phase 4: wait for L1 RPC on all 10 chain nodes ===")
	if err := waitChainHealthy(ctx, cfg); err != nil {
		return err
	}

	// 5. Print RPCs.
	fmt.Println()
	fmt.Println("=== ready ===")
	printRPCs(cfg, res)
	return nil
}

func wipeControlState() error {
	// Kill any avalanchego processes left behind by a prior run, then wipe
	// the data dirs. Always-reset is the policy (running with stale DC1
	// state would force a hardfork on next start).
	if out, err := exec.Command("pkill", "-9", "-f", "avalanchego").CombinedOutput(); err != nil {
		// pkill exits 1 if there's nothing to kill; that's fine.
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			return fmt.Errorf("pkill avalanchego: %w (%s)", err, string(out))
		}
	}
	time.Sleep(1 * time.Second)
	for _, sub := range []string{"control-1", "control-2"} {
		if err := os.RemoveAll(filepath.Join(controlDataRoot, sub)); err != nil {
			return err
		}
	}
	return nil
}

// startControlPrimary spawns a primary-network avalanchego process locally.
// Index 1 or 2 selects which staking key set to use.
func startControlPrimary(ctx context.Context, cfg *config, idx, httpP, stakeP int, bootstrapIP, bootstrapID string) error {
	dataDir := filepath.Join(controlDataRoot, fmt.Sprintf("control-%d", idx))
	stakingTarget := filepath.Join(dataDir, "staking")
	if err := os.MkdirAll(stakingTarget, 0o700); err != nil {
		return err
	}
	stakingSrc := filepath.Join(cfg.stakingDir, "control", fmt.Sprintf("%d", idx))
	for _, f := range []string{"signer.key", "staker.crt", "staker.key"} {
		if err := copyFile(filepath.Join(stakingSrc, f), filepath.Join(stakingTarget, f)); err != nil {
			return fmt.Errorf("copy staking %s: %w", f, err)
		}
	}
	pluginsTarget := filepath.Join(dataDir, "plugins")
	if err := os.MkdirAll(pluginsTarget, 0o755); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(cfg.binDir, cfg.subnetEvmID), filepath.Join(pluginsTarget, cfg.subnetEvmID)); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(pluginsTarget, cfg.subnetEvmID), 0o755); err != nil {
		return err
	}
	logFile, err := os.Create(filepath.Join(dataDir, "stdout.log"))
	if err != nil {
		return err
	}

	args := []string{
		fmt.Sprintf("--http-port=%d", httpP),
		fmt.Sprintf("--staking-port=%d", stakeP),
		"--http-host=0.0.0.0",
		fmt.Sprintf("--public-ip=%s", cfg.controlIP),
		"--network-id=local",
		"--sybil-protection-enabled=false",
		fmt.Sprintf("--plugin-dir=%s", pluginsTarget),
		fmt.Sprintf("--data-dir=%s", dataDir),
		fmt.Sprintf("--bootstrap-ips=%s", bootstrapIP),
		fmt.Sprintf("--bootstrap-ids=%s", bootstrapID),
	}
	cmd := exec.Command(filepath.Join(cfg.binDir, "avalanchego"), args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start control-%d: %w", idx, err)
	}
	fmt.Printf("  spawned control-%d (pid=%d, http=%d, staking=%d, log=%s)\n",
		idx, cmd.Process.Pid, httpP, stakeP, logFile.Name())
	return nil
}

func bootChainNodes(ctx context.Context, cfg *config, rem *remote, res *l1Result) error {
	// One pass per host: wipe, mkdir tree, scp binary+plugin+chain-config
	// (plus a validator key set if the arch places one on this host),
	// start avalanchego.
	allHosts := append(append([]string{}, cfg.dc1IPs...), cfg.dc2IPs...)
	keyAssign := keyAssignments(cfg)

	return fanOut(ctx, allHosts, func(ctx context.Context, host string) error {
		keyIdx, isValidator := keyAssign[host]
		role := roleLabel(cfg, host, keyIdx, isValidator)

		fmt.Printf("  [%s/%s] resetting...\n", role, host)
		setup := fmt.Sprintf(`
			set -e
			%[4]s
			rm -rf %[1]s
			mkdir -p %[1]s/bin %[1]s/plugins %[1]s/staking %[1]s/configs/chains/%[2]s %[1]s/db %[1]s/logs
			chown -R %[3]s:%[3]s %[1]s
		`, remoteDir, res.ChainID, cfg.sshUser, killAvalanchegoHardSnippet)
		if _, err := rem.run(ctx, host, setup); err != nil {
			return err
		}

		// scp binary + plugin + chain-config (everywhere).
		if err := rem.scpUp(ctx, host, filepath.Join(cfg.binDir, "avalanchego"), remoteDir+"/bin/"); err != nil {
			return err
		}
		if err := rem.scpUp(ctx, host, filepath.Join(cfg.binDir, cfg.subnetEvmID), remoteDir+"/plugins/"); err != nil {
			return err
		}
		if err := rem.scpUp(ctx, host, filepath.Join(cfg.configDir, "chain-config.json"),
			fmt.Sprintf("%s/configs/chains/%s/config.json", remoteDir, res.ChainID)); err != nil {
			return err
		}
		// Validator hosts get the staking key set assigned by arch.
		if isValidator {
			src := filepath.Join(cfg.stakingDir, "dc1", fmt.Sprintf("%d", keyIdx))
			for _, f := range []string{"signer.key", "staker.crt", "staker.key"} {
				if err := rem.scpUp(ctx, host, filepath.Join(src, f), remoteDir+"/staking/"); err != nil {
					return err
				}
			}
		}

		if _, err := rem.run(ctx, host, chainNodeStartScript(cfg, host, res)); err != nil {
			return err
		}
		fmt.Printf("  [%s/%s] avalanchego started\n", role, host)
		return nil
	})
}

// chainNodeStartScript returns the shell script that boots a single
// chain-node avalanchego with --track-subnets. Reused by system-start
// (after fresh scp) and by dc2-takeover (after key-swap).
func chainNodeStartScript(cfg *config, host string, res *l1Result) string {
	bootstrapIPs := fmt.Sprintf("%s:%d,%s:%d", cfg.controlIP, control1Staking, cfg.controlIP, control2Staking)
	bootstrapIDs := fmt.Sprintf("%s,%s", cfg.control1NodeID, cfg.control2NodeID)
	return fmt.Sprintf(`
		set -e
		cd %[1]s
		chmod +x bin/avalanchego plugins/%[2]s
		nohup ./bin/avalanchego \
			--http-port=%[3]d \
			--staking-port=%[4]d \
			--http-host=0.0.0.0 \
			--public-ip=%[5]s \
			--network-id=local \
			--sybil-protection-enabled=false \
			--plugin-dir=%[1]s/plugins \
			--data-dir=%[1]s \
			--chain-config-dir=%[1]s/configs/chains \
			--track-subnets=%[6]s \
			--bootstrap-ips=%[7]s \
			--bootstrap-ids=%[8]s \
			>logs/stdout.log 2>&1 &
		disown
		sleep 1
	`, remoteDir, cfg.subnetEvmID, httpPort, stakingPort, host,
		res.SubnetID, bootstrapIPs, bootstrapIDs)
}

func waitChainHealthy(ctx context.Context, cfg *config) error {
	all := append(append([]string{}, cfg.dc1IPs...), cfg.dc2IPs...)
	return fanOut(ctx, all, func(ctx context.Context, host string) error {
		return waitHealthy(ctx, host, httpPort, healthTimeout, host)
	})
}

func writeNetworkEnv(cfg *config, res *l1Result) error {
	path := filepath.Join(cfg.repoRoot, "network.env")
	body := fmt.Sprintf("SUBNET_ID=%s\nCHAIN_ID=%s\n", res.SubnetID, res.ChainID)
	return os.WriteFile(path, []byte(body), 0o644)
}

func printRPCs(cfg *config, res *l1Result) {
	keyAssign := keyAssignments(cfg)
	tag := func(ip string) string {
		if k, ok := keyAssign[ip]; ok {
			return fmt.Sprintf("validator v%d", k)
		}
		return "follower"
	}

	fmt.Println()
	fmt.Println("Primary-network RPCs (control):")
	fmt.Printf("  control-1: http://%s:%d\n", cfg.controlIP, control1HTTP)
	fmt.Printf("  control-2: http://%s:%d\n", cfg.controlIP, control2HTTP)
	fmt.Println()
	fmt.Printf("L1 RPCs (DC1, arch=%s):\n", cfg.arch)
	for i, ip := range cfg.dc1IPs {
		fmt.Printf("  dc1-%d (%s): http://%s:%d/ext/bc/%s/rpc\n", i+1, tag(ip), ip, httpPort, res.ChainID)
	}
	fmt.Println()
	fmt.Printf("L1 RPCs (DC2, arch=%s):\n", cfg.arch)
	for i, ip := range cfg.dc2IPs {
		fmt.Printf("  dc2-%d (%s): http://%s:%d/ext/bc/%s/rpc\n", i+1, tag(ip), ip, httpPort, res.ChainID)
	}
	fmt.Println()
	fmt.Printf("subnet ID: %s\n", res.SubnetID)
	fmt.Printf("chain ID:  %s\n", res.ChainID)
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, 0o644)
}
