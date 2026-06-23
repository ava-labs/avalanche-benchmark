package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// config is the runtime environment, supplied by the bash wrapper (which sources
// _common.sh + network.env and computes the P-chain bootstrap set). The pure
// planner needs none of this; only the I/O orchestration does.
type config struct {
	topo         Topology
	nodeIPs      []string // pool machines: site A (first 5 of NODE_IPS), then site B (BACKUP_SITE_NODE_IPS) if configured
	sshUser      string
	sshKey       string
	remoteDir    string // e.g. ~/avalanche-benchmark (tilde expanded remotely)
	repoDir      string // local repo root, source of upload artifacts
	chainID      string
	subnetID     string
	subnetEVMID  string
	bootstrapIPs string // static P-chain staking ips csv
	bootstrapIDs string // static P-chain node ids csv
	stateFile    string
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fatalf("missing required env %s (run via scripts/failover wrappers, not directly)", key)
	}
	return v
}

func loadConfig() *config {
	ips := strings.Split(mustEnv("NODE_IPS"), ",")
	if len(ips) < sitePoolSize {
		fatalf("NODE_IPS has %d entries, need at least %d pool machines", len(ips), sitePoolSize)
	}
	pool := ips[:sitePoolSize]

	topo := Topology{}
	if backup := os.Getenv("BACKUP_SITE_NODE_IPS"); backup != "" {
		bips := strings.Split(backup, ",")
		if len(bips) != sitePoolSize {
			fatalf("BACKUP_SITE_NODE_IPS has %d entries, need exactly %d backup-site machines", len(bips), sitePoolSize)
		}
		topo.TwoSite = true
		pool = append(pool, bips...)
	}

	return &config{
		topo:         topo,
		nodeIPs:      pool,
		sshUser:      mustEnv("SSH_USER"),
		sshKey:       mustEnv("SSH_KEY_PATH"),
		remoteDir:    envOr("REMOTE_DIR", "~/avalanche-benchmark"),
		repoDir:      mustEnv("REPO_DIR"),
		chainID:      mustEnv("CHAIN_ID"),
		subnetID:     mustEnv("SUBNET_ID"),
		subnetEVMID:  mustEnv("SUBNET_EVM_ID"),
		bootstrapIPs: mustEnv("PCHAIN_BOOTSTRAP_IPS"),
		bootstrapIDs: mustEnv("PCHAIN_BOOTSTRAP_IDS"),
		stateFile:    mustEnv("FAILOVER_STATE_FILE"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// sshOpts mirror _common.sh exactly — these avoid the YubiKey-agent hang on the
// ephemeral bench hosts. Keep them byte-identical with _common.sh.
//
// ConnectTimeout + ServerAlive* are load-bearing: without them a single wedged
// host (sshd up at TCP but starved so it never completes the banner, or a box
// that thrashes mid-command) hangs ssh — and thus the whole reconcile/restore —
// FOREVER with no error. ConnectTimeout bounds the connect/banner phase;
// ServerAliveInterval*CountMax (~60s) bails if an established connection goes
// dead mid-command. A failed ssh is fatal (see ssh()), so the operator gets a
// clear "ssh <host> failed" instead of an indefinite hang (measured 2026-06-19:
// a starved m1 froze `restore` on the first killNode pkill).
func (c *config) sshArgs(host string, extra ...string) []string {
	args := []string{
		"-i", c.sshKey,
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		fmt.Sprintf("%s@%s", c.sshUser, host),
	}
	return append(args, extra...)
}

func (c *config) scpArgs(extra ...string) []string {
	args := []string{
		"-i", c.sshKey,
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-q",
	}
	return append(args, extra...)
}

// ssh runs a remote command and returns trimmed stdout. SSH failures are fatal:
// "the simulated up/down went wrong" — no best-effort skipping.
func (c *config) ssh(host, remoteCmd string) string {
	cmd := exec.Command("ssh", c.sshArgs(host, remoteCmd)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fatalf("ssh %s failed: %v\n  cmd: %s", host, err, remoteCmd)
	}
	return strings.TrimSpace(string(out))
}

// sshStdin runs a remote command, piping the given script to its stdin.
func (c *config) sshStdin(host, remoteCmd, stdin string) {
	cmd := exec.Command("ssh", c.sshArgs(host, remoteCmd)...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		fatalf("ssh %s failed: %v\n  cmd: %s", host, err, remoteCmd)
	}
}

func (c *config) scp(localPath, host, remotePath string, recursive bool) {
	args := c.scpArgs()
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, localPath, fmt.Sprintf("%s@%s:%s", c.sshUser, host, remotePath))
	cmd := exec.Command("scp", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatalf("scp %s -> %s:%s failed: %v", localPath, host, remotePath, err)
	}
}

// snapshotPull streams a (lightly-compressed) tar of the already-stopped source's
// chain data dir into a local file on the control box. The source MUST be stopped
// first (killNode) so the on-disk pebble/EVM state is a consistent point-in-time
// image — copying a live DB yields a torn, unopenable snapshot. The output is
// streamed to a file, never buffered in memory (the DB is many GB). gzip -1 keeps
// the cross-region pull fast without pegging CPU on the (idle, stopped) source.
func (c *config) snapshotPull(host, localTar string) bool {
	f, err := os.Create(localTar)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: create %s: %v\n", localTar, err)
		return false
	}
	defer f.Close()
	cmd := exec.Command("ssh", c.sshArgs(host, fmt.Sprintf("cd %s && tar -cf - data/validator | gzip -1", c.remoteDir))...)
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot pull from %s failed: %v\n", host, err)
		return false
	}
	return true
}

// snapshotPush streams a local snapshot tar to a target and extracts it, recreating
// data/validator from the source's committed state. The target MUST be stopped and
// its data/validator removed first (loadSnapshot does this) so nothing stale merges
// with the extracted image.
func (c *config) snapshotPush(host, localTar string) {
	f, err := os.Open(localTar)
	if err != nil {
		fatalf("snapshot: open %s: %v", localTar, err)
	}
	defer f.Close()
	cmd := exec.Command("ssh", c.sshArgs(host, fmt.Sprintf("cd %s && gzip -dc | tar -xf -", c.remoteDir))...)
	cmd.Stdin = f
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatalf("snapshot push to %s failed: %v", host, err)
	}
}

// observe reads liveness (pgrep) and the active key marker (cat) in one round trip.
func (c *config) observe(host string) Observed {
	out := c.ssh(host, fmt.Sprintf(
		"pgrep -x avalanchego >/dev/null && echo A || echo D; cat %s/staking/active/key_index 2>/dev/null || echo 0",
		c.remoteDir))
	lines := strings.Split(out, "\n")
	ob := Observed{}
	if len(lines) > 0 {
		ob.Alive = strings.TrimSpace(lines[0]) == "A"
	}
	if len(lines) > 1 {
		ob.ActualKey, _ = strconv.Atoi(strings.TrimSpace(lines[1]))
	}
	return ob
}

// pluginPat matches the subnet-evm plugin processes by their on-disk path. The
// "[p]lugins" bracket makes the pattern not match the pkill/pgrep command's own
// shell (whose argv literally contains "[p]lugins", which the regex "plugins"
// does not), avoiding self-termination of the SSH session.
const pluginPat = "avalanche-benchmark/[p]lugins/"

// killNode kills avalanchego AND its orphaned subnet-evm plugin children, then
// waits for both to disappear. avalanchego runs the VM as a go-plugin child
// process; SIGKILLing the parent orphans the child, which keeps the plugin binary
// open (ETXTBSY) and blocks re-upload — and these orphans accumulate across
// failover cycles. So every stop must reap the plugin too.
func (c *config) killNode(host string) {
	c.ssh(host, "pkill -KILL -x avalanchego || true; pkill -KILL -f '"+pluginPat+"' || true")
	for i := 0; i < 40; i++ {
		out := c.ssh(host,
			"{ pgrep -x avalanchego >/dev/null || pgrep -f '"+pluginPat+"' >/dev/null; } && echo A || echo D")
		if out == "D" {
			return
		}
		c.ssh(host, "pkill -KILL -x avalanchego || true; pkill -KILL -f '"+pluginPat+"' || true")
		time.Sleep(250 * time.Millisecond)
	}
	fatalf("avalanchego/plugin on %s did not exit after pkill", host)
}

// stop kills the node and waits for it to actually exit.
func (c *config) stop(host string) {
	c.killNode(host)
}

// swap wipes staking/active and copies the committed key set for the given index
// in, then rewrites the key_index marker. Wipe-before-write: a crash mid-swap
// leaves the key missing (re-run re-copies), never duplicated.
func (c *config) swap(host string, keyIdx int) {
	c.ssh(host, fmt.Sprintf(
		"cd %s && rm -rf staking/active && mkdir -p staking/active && "+
			"cp staking/l1/%d/staker.crt staking/l1/%d/staker.key staking/l1/%d/signer.key staking/active/ && "+
			"echo %d > staking/active/key_index",
		c.remoteDir, keyIdx, keyIdx, keyIdx, keyIdx))
}

// startScript renders the identity-agnostic launch script. Identity lives in the
// files under staking/active (swapped in pass 1), so this command is byte-identical
// regardless of which validator the machine hosts. data/ is preserved (never wiped
// here) so a hot spare rejoins in seconds.
func (c *config) startScript(nodeIP string) string {
	return fmt.Sprintf(`#!/bin/bash
set -e
cd %[1]s

mkdir -p "data/validator/configs/chains/%[2]s" "data/validator/configs/subnets" "data/validator/db" "data/validator/logs"
cp chain-config.json "data/validator/configs/chains/%[2]s/config.json"
cp subnet-config.json "data/validator/configs/subnets/%[3]s.json"

setsid ./bin/avalanchego \
    --http-port=9652 \
    --staking-port=9653 \
    --http-host=0.0.0.0 \
    --public-ip=%[4]s \
    --db-dir=data/validator/db \
    --log-dir=data/validator/logs \
    --data-dir=data/validator \
    --network-id=local \
    --staking-tls-cert-file=staking/active/staker.crt \
    --staking-tls-key-file=staking/active/staker.key \
    --staking-signer-key-file=staking/active/signer.key \
    --plugin-dir=$(pwd)/plugins \
    --config-file=node-config.json \
    --chain-config-dir=data/validator/configs/chains \
    --subnet-config-dir=data/validator/configs/subnets \
    --track-subnets="%[3]s" \
    --bootstrap-ips=%[5]s \
    --bootstrap-ids=%[6]s \
    >data/validator/logs/avalanchego.out 2>&1 < /dev/null &
`, c.remoteDir, c.chainID, c.subnetID, nodeIP, c.bootstrapIPs, c.bootstrapIDs)
}

func (c *config) start(host, nodeIP string) {
	script := c.startScript(nodeIP)
	remoteScript := c.remoteDir + "/start-l1-validator.sh"
	c.sshStdin(host,
		fmt.Sprintf("cat > %s && chmod +x %s && %s", remoteScript, remoteScript, remoteScript),
		script)
}

// freshClean kills the process and wipes data/ + staking/active so the next
// observe reads dead + key 0. Used only by `reconcile fresh`.
func (c *config) freshClean(host string) {
	c.killNode(host)
	c.ssh(host, fmt.Sprintf(
		"cd %s 2>/dev/null && rm -rf data staking/active || true; "+
			"mkdir -p %s/bin %s/plugins %s/staking/l1",
		c.remoteDir, c.remoteDir, c.remoteDir, c.remoteDir))
}

// wipeL1Data stops the node and deletes its entire data/validator directory so it
// rejoins with no local chain state and is forced to state-sync the L1 fresh onto
// the live branch. Used by `restore` for the recovering site only.
//
// The whole dir is removed on purpose. There is no "L1-only" database to delete:
// avalanchego keeps every chain (P-chain, C/X, and the L1 subnet) in one shared
// --db-dir (data/validator/db, a single prefixdb with no per-chain subdir), and
// subnet-evm keeps its EVM state under data/validator/chainData/<chainID>. Removing
// only db/ is NOT enough — the EVM state resurrects the stale post-failover frontier
// and the node then reports a height the live validators never had, so it never
// converges (measured 2026-06-12; see docs/two-site-failover.md). Clearing all of
// data/validator is the only reliable clean slate.
//
// This is safe: nothing unique lives in data/validator. The node identity
// (staking/active) and committed key sets (staking/l1) are outside it and untouched,
// so the P-chain validator registration (authoritative on the dev-machine P-chain) is
// unaffected. On restart the node re-bootstraps the primary network from the dev
// machine and state-syncs the L1 to tip (state-sync-enabled in chain-config.json);
// startScript re-creates the dirs and re-copies configs.
func (c *config) wipeL1Data(host string) {
	c.killNode(host)
	c.ssh(host, fmt.Sprintf("cd %s && rm -rf data/validator || true", c.remoteDir))
}

// provisioned reports whether the machine already has binary, plugin, configs and
// every committed key set the topology can assign to it.
func (c *config) provisioned(host string) bool {
	var keyChecks strings.Builder
	for _, k := range c.topo.AllKeys() {
		fmt.Fprintf(&keyChecks, "test -d staking/l1/%d && ", k)
	}
	out := c.ssh(host, fmt.Sprintf(
		"cd %s 2>/dev/null && test -f bin/avalanchego && test -f plugins/%s && "+
			"test -f node-config.json && test -f chain-config.json && test -f subnet-config.json && "+
			"%secho OK || echo MISSING",
		c.remoteDir, c.subnetEVMID, keyChecks.String()))
	return out == "OK"
}

// nodeIndex returns the pool index of host, or -1 if it is not a known pool machine.
func (c *config) nodeIndex(host string) int {
	for i, h := range c.nodeIPs {
		if h == host {
			return i
		}
	}
	return -1
}

// isArchiveNode reports whether machine i is a pinned dedicated-RPC node — the LAST
// TWO slots of each site (m5/m6, and b5/b6 in two-site mode). RPC nodes run as ARCHIVE
// (chain-config-rpc.json: pruning-enabled=false, state-sync-enabled=false) so they
// hold full historical state and can serve arbitrary-height eth_ queries. Every
// other machine — the 3 validators and the spare — runs the default chain-config.json
// (state-sync + pruning) for fast, light sync. Index-based (slot within site >= the
// first RPC slot), so it holds in both single- and two-site mode. Safe for i<0 (no match).
func (c *config) isArchiveNode(i int) bool {
	if i < 0 {
		return false
	}
	return i%sitePoolSize >= sitePoolSize-2
}

// deployChainConfig writes host's ROLE-appropriate chain-config to ~/chain-config.json (which
// the start script copies into place): the archive profile (pruning + state-sync disabled) for
// the pinned RPC nodes, the light profile (state-sync + pruning) for validators and the spare.
// Both land as chain-config.json so startScript is unchanged. Re-applied on restore so a node's
// pruning/state-sync mode is reset to match its role-default DB source — undoing any archive
// config a failover left behind when it cloned an archive RPC's DB onto a validator.
func (c *config) deployChainConfig(host string) {
	chainCfg := "chain-config.json"
	if c.isArchiveNode(c.nodeIndex(host)) {
		chainCfg = "chain-config-rpc.json"
	}
	c.scp(c.repoDir+"/"+chainCfg, host, c.remoteDir+"/chain-config.json", false)
}

// upload pushes all artifacts a pool machine needs. Pass 0 of reconcile.
func (c *config) upload(host string) {
	c.ssh(host, fmt.Sprintf("mkdir -p %s/bin %s/plugins %s/staking/l1", c.remoteDir, c.remoteDir, c.remoteDir))
	c.scp(c.repoDir+"/bin/avalanchego", host, c.remoteDir+"/bin/", false)
	c.scp(c.repoDir+"/bin/"+c.subnetEVMID, host, c.remoteDir+"/plugins/", false)
	c.scp(c.repoDir+"/node-config.json", host, c.remoteDir+"/", false)
	c.deployChainConfig(host)
	c.scp(c.repoDir+"/subnet-config.json", host, c.remoteDir+"/", false)
	for _, k := range c.topo.AllKeys() {
		c.scp(fmt.Sprintf("%s/staking/l1/%d", c.repoDir, k), host, c.remoteDir+"/staking/l1/", true)
	}
}
