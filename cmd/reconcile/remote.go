package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// config is the runtime environment, supplied by the bash wrapper (which sources
// _common.sh + network.env and exports the Fuji upstream peer). The pure
// planner needs none of this; only the I/O orchestration does.
type config struct {
	topo        Topology
	nodeIPs     []string   // pool machines: site A (NODE_IPS), then site B (BACKUP_SITE_NODE_IPS) if configured
	instances   []instance // one per pool slot, parallel to nodeIPs; carries ports + dirs (co-location aware)
	sshUser     string
	sshKey      string
	remoteDir   string // e.g. ~/avalanche-benchmark (tilde expanded remotely)
	repoDir     string // local repo root, source of upload artifacts
	chainID     string
	subnetID    string
	subnetEVMID string
	upstreamIPs string         // public Fuji peer(s) the RPC tier's P-chain follows (ip:port csv)
	upstreamIDs string         // their NodeIDs (TLS-pinned; a hijacked IP cannot impersonate)
	nodeIDByKey map[int]string // committed key index -> NodeID, from staking/node-ids.env
	stateFile   string
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fatalf("missing required env %s (run via scripts/failover wrappers, not directly)", key)
	}
	return v
}

// splitIPs parses a comma-separated IP list, trimming blanks and dropping empties.
func splitIPs(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// loadPool derives the topology, ordered pool, and per-slot instances from the
// per-role IP env. Each site is defined by three lists whose LENGTHS set the
// counts and whose VALUES set placement (a repeated IP co-locates another process
// on that box):
//
//	VALIDATOR_IPS / SPARE_IPS / RPC_IPS                 (site A)
//	BACKUP_VALIDATOR_IPS / BACKUP_SPARE_IPS / BACKUP_RPC_IPS (site B, optional)
//
// Per site the pool is laid out [validators..., spares..., rpcs...]. Validators
// must be >=3 (hard error) and RPCs >=1. (A single RPC is fine now that every role
// state-syncs and restore seeds all roles from one pruned snapshot: no more
// snapshot-the-redundant-RPC trick that needed a second ingress node.) The two sites
// must share the same shape, since the registered validator set moves between them on failover.
//
// Back-compat: if VALIDATOR_IPS is unset, fall back to the legacy positional
// NODE_IPS / BACKUP_SITE_NODE_IPS as the fixed 3 validators + 1 spare + 2 RPCs.
// Split out from loadConfig so `endpoints` can run with just the IP env.
func loadPool() (Topology, []string, []instance) {
	if os.Getenv("VALIDATOR_IPS") == "" {
		return loadPoolPositional()
	}

	valA := splitIPs(mustEnv("VALIDATOR_IPS"))
	spareA := splitIPs(os.Getenv("SPARE_IPS"))
	rpcA := splitIPs(os.Getenv("RPC_IPS"))
	if len(valA) < 3 {
		fatalf("VALIDATOR_IPS has %d IP(s); a live L1 needs at least 3 validators", len(valA))
	}
	if len(rpcA) < 1 {
		fatalf("RPC_IPS has %d IP(s); need at least 1 RPC node per site to serve ingress.", len(rpcA))
	}

	topo := Topology{NVal: len(valA), NSpare: len(spareA), NRPC: len(rpcA)}
	pool := append(append(append([]string{}, valA...), spareA...), rpcA...)

	if os.Getenv("BACKUP_VALIDATOR_IPS") != "" {
		valB := splitIPs(mustEnv("BACKUP_VALIDATOR_IPS"))
		spareB := splitIPs(os.Getenv("BACKUP_SPARE_IPS"))
		rpcB := splitIPs(os.Getenv("BACKUP_RPC_IPS"))
		if len(valB) != topo.NVal || len(spareB) != topo.NSpare || len(rpcB) != topo.NRPC {
			fatalf("backup site shape (%dv/%ds/%dr) must match primary (%dv/%ds/%dr): the validator set moves between sites on failover",
				len(valB), len(spareB), len(rpcB), topo.NVal, topo.NSpare, topo.NRPC)
		}
		topo.TwoSite = true
		pool = append(pool, valB...)
		pool = append(pool, spareB...)
		pool = append(pool, rpcB...)
	}
	return topo, pool, buildInstances(pool)
}

// loadPoolPositional is the legacy fixed 3/1/2 layout from NODE_IPS /
// BACKUP_SITE_NODE_IPS, preserved so existing deploys behave identically.
func loadPoolPositional() (Topology, []string, []instance) {
	const legacyPool = 6 // 3 validators + 1 spare + 2 RPCs
	topo := Topology{NVal: 3, NSpare: 1, NRPC: 2}
	ips := strings.Split(mustEnv("NODE_IPS"), ",")
	if len(ips) < legacyPool {
		fatalf("NODE_IPS has %d entries, need at least %d pool machines", len(ips), legacyPool)
	}
	pool := ips[:legacyPool]
	if backup := os.Getenv("BACKUP_SITE_NODE_IPS"); backup != "" {
		bips := strings.Split(backup, ",")
		if len(bips) != legacyPool {
			fatalf("BACKUP_SITE_NODE_IPS has %d entries, need exactly %d backup-site machines", len(bips), legacyPool)
		}
		topo.TwoSite = true
		pool = append(pool, bips...)
	}
	return topo, pool, buildInstances(pool)
}

func loadConfig() *config {
	topo, pool, insts := loadPool()
	c := &config{
		topo:        topo,
		nodeIPs:     pool,
		instances:   insts,
		sshUser:     mustEnv("SSH_USER"),
		sshKey:      mustEnv("SSH_KEY_PATH"),
		remoteDir:   envOr("REMOTE_DIR", "~/avalanche-benchmark"),
		repoDir:     mustEnv("REPO_DIR"),
		chainID:     mustEnv("CHAIN_ID"),
		subnetID:    mustEnv("SUBNET_ID"),
		subnetEVMID: mustEnv("SUBNET_EVM_ID"),
		upstreamIPs: mustEnv("FUJI_UPSTREAM_IPS"),
		upstreamIDs: mustEnv("FUJI_UPSTREAM_IDS"),
		stateFile:   mustEnv("FAILOVER_STATE_FILE"),
	}
	c.nodeIDByKey = loadNodeIDs(c.repoDir + "/staking/node-ids.env")
	return c
}

// loadNodeIDs parses the generated manifest (staking/node-ids.env, written by
// 00_gen_secrets.sh) into committed-key-index -> NodeID. The NodeIDs feed the
// per-role --bootstrap-ids / --state-sync-ids lists in startScript.
func loadNodeIDs(path string) map[int]string {
	vars, err := godotenv.Read(path)
	if err != nil {
		fatalf("read %s (run ./00_gen_secrets.sh first): %v", path, err)
	}
	out := make(map[int]string, len(vars))
	for k, v := range vars {
		var idx int
		if _, err := fmt.Sscanf(k, "L1_%d_NODE_ID", &idx); err == nil && v != "" {
			out[idx] = v
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// sshOpts mirror _common.sh exactly: these avoid the YubiKey-agent hang on the
// ephemeral bench hosts. Keep them byte-identical with _common.sh.
//
// ConnectTimeout + ServerAlive* are load-bearing: without them a single wedged
// host (sshd up at TCP but starved so it never completes the banner, or a box
// that thrashes mid-command) hangs ssh (and thus the whole reconcile/restore)
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
// "the simulated up/down went wrong": no best-effort skipping.
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

// rsyncUpload pushes one file with rsync's delta+compress (near-instant when the
// remote copy is unchanged). -e carries the SAME key/opts as scp (scpArgs returns
// just `-i key -o ... -q`, all valid ssh flags; rsync word-splits it on spaces, the
// same no-spaces assumption the existing scp/ssh helpers make). -p preserves the
// source mode (executable bit on the binary); -t preserves mtime so an unchanged
// file is skipped outright next time. rsync ships on the Ubuntu AMI; if it is
// missing the transfer fails loudly via fatalf, same as scp().
func (c *config) rsyncUpload(localPath, host, remotePath string) {
	args := []string{
		"-ztp", "-e", "ssh " + strings.Join(c.scpArgs(), " "),
		localPath, fmt.Sprintf("%s@%s:%s", c.sshUser, host, remotePath),
	}
	cmd := exec.Command("rsync", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatalf("rsync %s -> %s:%s failed: %v", localPath, host, remotePath, err)
	}
}

// snapshotPull streams a (lightly-compressed) tar of the already-stopped source's
// chain data dir into a local file on the control box. The source MUST be stopped
// first (killNode) so the on-disk pebble/EVM state is a consistent point-in-time
// image: copying a live DB yields a torn, unopenable snapshot. The output is
// streamed to a file, never buffered in memory (the DB is many GB). zstd -T0 uses
// all cores on the (idle, stopped) source: much faster than single-threaded gzip
// and a better ratio, so fewer bytes cross the slow cross-region link. (If a node
// lacks zstd the pull fails cleanly and restore falls back to state-sync.)
func (c *config) snapshotPull(srcIdx int, localTar string) bool {
	in := c.instances[srcIdx]
	f, err := os.Create(localTar)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: create %s: %v\n", localTar, err)
		return false
	}
	defer f.Close()
	// Tar the CONTENTS of the data dir (relative "./db", "./logs", …) rather than the
	// dir itself, so a snapshot from one instance can be extracted into a target whose
	// data dir has a different name (e.g. data/validator -> data/validator-1 when the
	// recovering node is co-located). For a normal (non-co-located) restore this is the
	// same bytes as before, just relative paths.
	cmd := exec.Command("ssh", c.sshArgs(in.host, fmt.Sprintf("cd %s/%s && tar -cf - . | zstd -3 -T0", c.remoteDir, in.dataDir))...)
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot pull from %s failed: %v\n", in.host, err)
		return false
	}
	return true
}

// snapshotPush streams a local snapshot tar to a target and extracts it, recreating
// data/validator from the source's committed state. The target MUST be stopped and
// its data/validator removed first (loadSnapshot does this) so nothing stale merges
// with the extracted image.
func (c *config) snapshotPush(dstIdx int, localTar string) {
	in := c.instances[dstIdx]
	f, err := os.Open(localTar)
	if err != nil {
		fatalf("snapshot: open %s: %v", localTar, err)
	}
	defer f.Close()
	// Extract the relative-path image (see snapshotPull) INTO this instance's own data
	// dir, whatever it is named: decoupling source and destination dir names.
	cmd := exec.Command("ssh", c.sshArgs(in.host, fmt.Sprintf("mkdir -p %s/%s && cd %s/%s && zstd -dc | tar -xf -", c.remoteDir, in.dataDir, c.remoteDir, in.dataDir))...)
	cmd.Stdin = f
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatalf("snapshot push to %s failed: %v", in.host, err)
	}
}

// observe reads liveness (pgrep) and the active key marker (cat) for pool slot i in
// one round trip. On a normal (one-process) box liveness is the exact-name `pgrep -x
// avalanchego` it always was; on a co-located box it matches THIS instance by its
// unique HTTP port so a housemate process isn't mistaken for this one.
func (c *config) observe(i int) Observed {
	in := c.instances[i]
	live := "pgrep -x avalanchego >/dev/null"
	if in.shared {
		live = fmt.Sprintf("pgrep -f -- '%s' >/dev/null", in.procPat)
	}
	out := c.ssh(in.host, fmt.Sprintf(
		"%s && echo A || echo D; cat %s/%s/key_index 2>/dev/null || echo 0",
		live, c.remoteDir, in.activeDir))
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

// killNode kills the avalanchego for pool slot i AND its orphaned subnet-evm plugin
// child, then waits for it to disappear. avalanchego runs the VM as a go-plugin child
// process; SIGKILLing the parent orphans the child, which keeps the plugin binary open
// (ETXTBSY) and blocks re-upload, and these orphans accumulate across failover cycles.
// So every stop must reap the plugin too.
//
// On a normal (one-process) box this is the exact-name reap it always was: kill every
// avalanchego + every plugin on the host. On a CO-LOCATED box that broad reap would
// take down the housemate processes, so we instead target THIS instance by its unique
// HTTP port and reap only its own plugin child (pgrep -P on that pid).
func (c *config) killNode(i int) {
	in := c.instances[i]
	kill, alive := c.killCmds(in)
	c.ssh(in.host, kill)
	for k := 0; k < 40; k++ {
		if c.ssh(in.host, alive) == "D" {
			return
		}
		c.ssh(in.host, kill)
		time.Sleep(250 * time.Millisecond)
	}
	fatalf("avalanchego/plugin for %s on %s did not exit after pkill", c.topo.MachineName(i), in.host)
}

// killCmds returns the (kill, liveness-probe) shell commands for an instance, choosing
// the broad host-wide reap for a single-process box and a PID-scoped reap for a
// co-located one. Split out so killNode stays a simple kill/wait loop.
func (c *config) killCmds(in instance) (kill, alive string) {
	if !in.shared {
		kill = "pkill -KILL -x avalanchego || true; pkill -KILL -f '" + pluginPat + "' || true"
		alive = "{ pgrep -x avalanchego >/dev/null || pgrep -f '" + pluginPat + "' >/dev/null; } && echo A || echo D"
		return kill, alive
	}
	// Co-located: find this instance's avalanchego by its unique http-port, kill it and
	// its direct children (the go-plugin process), leaving housemates untouched.
	kill = fmt.Sprintf(
		"for pid in $(pgrep -f -- '%s'); do kids=$(pgrep -P \"$pid\" 2>/dev/null || true); kill -KILL \"$pid\" $kids 2>/dev/null || true; done",
		in.procPat)
	alive = fmt.Sprintf("pgrep -f -- '%s' >/dev/null && echo A || echo D", in.procPat)
	return kill, alive
}

// stop ends the node for a PLANNED operation (key swap, `down`): gracefully, so its
// EVM state snapshot is flushed cleanly. See gracefulStop.
func (c *config) stop(i int) {
	c.gracefulStop(i)
}

// gracefulStopPolls bounds how long gracefulStop waits for a clean SIGTERM exit before
// falling back to SIGKILL. avalanchego flushes pebbledb + its EVM state snapshot on
// SIGTERM; under heavy load that can take a while, so the window is generous. The
// fallback guarantees the stop still completes.
const gracefulStopPolls = 120 // * 500ms = 60s

// gracefulStop stops the node CLEANLY: SIGTERM so avalanchego flushes its EVM state
// snapshot and reaps its own plugin child, then waits for a clean exit; falls back to
// the hard SIGKILL reap (killNode) only if it doesn't exit within the grace window.
// Used for every PLANNED stop (key swaps via stop(), snapshot sources, `down`). A
// SIGKILL there leaves a "missing or corrupted snapshot" that the node regenerates and
// (under load, after a reorg) can wedge on restart (observed: non-validators AND a
// validator frozen post-failover/restore, recoverable only by a full `clean`). Graceful
// shutdown also reaps the plugin child, avoiding the SIGKILL orphan/ETXTBSY problem.
// site-failover keeps the hard killNode on purpose: it simulates a real outage.
func (c *config) gracefulStop(i int) {
	in := c.instances[i]
	c.ssh(in.host, c.termCmd(in)) // SIGTERM: flush state + reap plugin
	_, alive := c.killCmds(in)
	for k := 0; k < gracefulStopPolls; k++ {
		if c.ssh(in.host, alive) == "D" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("  %s: graceful stop didn't exit in %ds: SIGKILL fallback\n", c.topo.MachineName(i), gracefulStopPolls/2)
	c.killNode(i)
}

// termCmd returns the SIGTERM command for an instance (host-wide for a single-process
// box, PID-scoped by http-port for a co-located one). SIGTERM triggers avalanchego's
// graceful shutdown, which flushes state and reaps its own plugin child, so we signal
// only the parent.
func (c *config) termCmd(in instance) string {
	if !in.shared {
		return "pkill -TERM -x avalanchego || true"
	}
	return fmt.Sprintf("for pid in $(pgrep -f -- '%s'); do kill -TERM \"$pid\" 2>/dev/null || true; done", in.procPat)
}

// swap wipes staking/active and copies the committed key set for the given index
// in, then rewrites the key_index marker. Wipe-before-write: a crash mid-swap
// leaves the key missing (re-run re-copies), never duplicated.
func (c *config) swap(i, keyIdx int) {
	in := c.instances[i]
	c.ssh(in.host, fmt.Sprintf(
		"cd %s && rm -rf %s && mkdir -p %s && "+
			"cp staking/l1/%d/staker.crt staking/l1/%d/staker.key staking/l1/%d/signer.key %s/ && "+
			"echo %d > %s/key_index",
		c.remoteDir, in.activeDir, in.activeDir, keyIdx, keyIdx, keyIdx, in.activeDir, keyIdx, in.activeDir))
}

// nodeIDForKey returns committed key k's NodeID from the generated manifest.
func (c *config) nodeIDForKey(k int) string {
	id := c.nodeIDByKey[k]
	if id == "" {
		fatalf("missing L1_%d_NODE_ID in %s/staking/node-ids.env (run ./00_gen_secrets.sh)", k, c.repoDir)
	}
	return id
}

// pchainBeacons returns the --bootstrap-ips/--bootstrap-ids for pool slot i.
// The whole fleet runs --p-chain-follow-only, re-syncing the P-chain forever
// from exactly these peers (two-hop chaining, see FUJI_PLAN.md):
//   - RPC slots follow the pinned public Fuji peer: the fleet's ONE allowed
//     external TCP (FUJI_UPSTREAM_IPS/IDS; re-check genesis/bootstrappers.json
//     on every avalanchego bump, the hardcoded IPs rotate between releases);
//   - every other slot follows its OWN SITE's RPC slots (validators only ever
//     reach RPC machines in the same DC). RPC identities are pinned home keys
//     that never swap, so this list is static per deploy. NOTE the >=75%
//     beacon-weight startup latch: with 2 RPCs per site a validator needs BOTH
//     connected at (re)start.
func (c *config) pchainBeacons(i int) (ips, nodeIDs string) {
	if c.topo.isRPCSlot(i) {
		return c.upstreamIPs, c.upstreamIDs
	}
	var ipL, idL []string
	for j, in := range c.instances {
		if c.topo.isRPCSlot(j) && c.topo.Site(j) == c.topo.Site(i) {
			ipL = append(ipL, fmt.Sprintf("%s:%d", in.host, in.stakingPort))
			idL = append(idL, c.nodeIDForKey(c.topo.HomeKey(j)))
		}
	}
	return strings.Join(ipL, ","), strings.Join(idL, ",")
}

// siblingSeeds returns --state-sync-ips/--state-sync-ids for pool slot i: every
// OTHER pool instance under its CURRENTLY intended identity. Signed-IP gossip
// never relays the fleet's private IPs, so the L1 consensus mesh must be seeded
// explicitly: via state-sync-ids, NOT bootstrap-ids, so fresh siblings never
// become P-chain frontier beacons (the frontier-cap gotcha; proven recipe in
// the 2026-07-03 e2e). Identities move on failover, so the seeds are read from
// the intentions file at render time: every (re)started node dials a fresh,
// correct list, and a stale entry on a not-restarted sibling is just a failed
// dial until the swapped node dials in itself.
func (c *config) siblingSeeds(i int) (ips, nodeIDs string) {
	intents, err := loadIntents(c.stateFile, c.topo)
	if err != nil {
		fatalf("render start script: %v", err)
	}
	var ipL, idL []string
	for j, in := range c.instances {
		if j == i {
			continue
		}
		ipL = append(ipL, fmt.Sprintf("%s:%d", in.host, in.stakingPort))
		idL = append(idL, c.nodeIDForKey(intents[j].Key))
	}
	return strings.Join(ipL, ","), strings.Join(idL, ",")
}

// startScript renders the launch script for pool slot i. Identity lives in the
// files under the instance's staking/active dir (swapped in pass 1); the only
// role-dependent parts are the P-chain beacon and sibling-seed lists above. The
// data dir is preserved (never wiped here) so a hot spare rejoins in seconds. All
// ports and paths come from the instance, so a co-located 2nd process lands on its
// own ports and dirs; for a normal node these are the original 9652/9653 +
// data/validator values.
//
// Fuji flags (see FUJI_PLAN.md): the primary network is Fuji itself
// (--network-id=fuji, built-in genesis); --partial-sync-primary-network syncs
// ONLY the P-chain (skips Fuji X/C); --p-chain-follow-only keeps the P-chain
// permanently bootstrapping off the beacons: REQUIRED on the inside tiers (a
// stock partial-sync node behind non-validator peers freezes after the
// bootstrap-to-consensus handoff) and what the RPC tier was e2e-proven with;
// --network-allow-private-ips lets the fleet dial its DC-internal addresses.
func (c *config) startScript(i int) string {
	in := c.instances[i]
	beaconIPs, beaconIDs := c.pchainBeacons(i)
	seedIPs, seedIDs := c.siblingSeeds(i)
	return fmt.Sprintf(`#!/bin/bash
set -e
cd %[1]s

mkdir -p "%[7]s/configs/chains/%[2]s" "%[7]s/configs/subnets" "%[7]s/db" "%[7]s/logs"
cp %[8]s "%[7]s/configs/chains/%[2]s/config.json"
cp subnet-config.json "%[7]s/configs/subnets/%[3]s.json"

setsid ./bin/avalanchego \
    --http-port=%[9]d \
    --staking-port=%[10]d \
    --http-host=0.0.0.0 \
    --public-ip=%[4]s \
    --db-dir=%[7]s/db \
    --log-dir=%[7]s/logs \
    --data-dir=%[7]s \
    --network-id=fuji \
    --partial-sync-primary-network=true \
    --p-chain-follow-only=true \
    --network-allow-private-ips=true \
    --staking-tls-cert-file=%[11]s/staker.crt \
    --staking-tls-key-file=%[11]s/staker.key \
    --staking-signer-key-file=%[11]s/signer.key \
    --plugin-dir=$(pwd)/plugins \
    --config-file=node-config.json \
    --chain-config-dir=%[7]s/configs/chains \
    --subnet-config-dir=%[7]s/configs/subnets \
    --track-subnets="%[3]s" \
    --bootstrap-ips=%[5]s \
    --bootstrap-ids=%[6]s \
    --state-sync-ips=%[12]s \
    --state-sync-ids=%[13]s \
    >%[7]s/logs/avalanchego.out 2>&1 < /dev/null &
`, c.remoteDir, c.chainID, c.subnetID, in.host, beaconIPs, beaconIDs,
		in.dataDir, in.chainCfg, in.httpPort, in.stakingPort, in.activeDir,
		seedIPs, seedIDs)
}

func (c *config) start(i int) {
	in := c.instances[i]
	script := c.startScript(i)
	remoteScript := c.remoteDir + "/" + in.startScript
	c.sshStdin(in.host,
		fmt.Sprintf("cat > %s && chmod +x %s && %s", remoteScript, remoteScript, remoteScript),
		script)
}

// freshClean kills the process for pool slot i and wipes ITS data + staking/active
// dirs so the next observe reads dead + key 0. Used only by `reconcile fresh`. Only
// this instance's dirs are removed (not the whole data/ tree), so co-located
// housemates on the same box are left intact; the shared base dirs are ensured to
// exist for the subsequent upload.
func (c *config) freshClean(i int) {
	in := c.instances[i]
	c.killNode(i)
	c.ssh(in.host, fmt.Sprintf(
		"cd %s 2>/dev/null && rm -rf %s %s || true; "+
			"mkdir -p %s/bin %s/plugins %s/staking/l1",
		c.remoteDir, in.dataDir, in.activeDir, c.remoteDir, c.remoteDir, c.remoteDir))
}

// wipeL1Data stops the node and deletes its entire data/validator directory so it
// rejoins with no local chain state and is forced to state-sync the L1 fresh onto
// the live branch. Used by `restore` for the recovering site only.
//
// The whole dir is removed on purpose. There is no "L1-only" database to delete:
// avalanchego keeps every chain (P-chain, C/X, and the L1 subnet) in one shared
// --db-dir (data/validator/db, a single prefixdb with no per-chain subdir), and
// subnet-evm keeps its EVM state under data/validator/chainData/<chainID>. Removing
// only db/ is NOT enough: the EVM state resurrects the stale post-failover frontier
// and the node then reports a height the live validators never had, so it never
// converges (measured 2026-06-12; see docs/two-site-failover.md). Clearing all of
// data/validator is the only reliable clean slate.
//
// This is safe: nothing unique lives in data/validator. The node identity
// (staking/active) and generated key sets (staking/l1) are outside it and untouched,
// so the P-chain validator registration (authoritative on Fuji's public P-chain) is
// unaffected. On restart the node re-syncs the Fuji P-chain through its beacons
// (RPC tier, follow-only) and state-syncs the L1 to tip (state-sync-enabled in
// chain-config.json); startScript re-creates the dirs and re-copies configs.
func (c *config) wipeL1Data(i int) {
	in := c.instances[i]
	c.killNode(i)
	c.ssh(in.host, fmt.Sprintf("cd %s && rm -rf %s || true", c.remoteDir, in.dataDir))
}

// provisioned reports whether the box already has binary, plugin, shared configs,
// every committed key set the topology can assign to it, AND the role chain-config for
// every instance it hosts (so adding a co-located 2nd instance to a previously
// single-process box re-triggers an upload for that box's new chain-config-N.json).
func (c *config) provisioned(host string) bool {
	var checks strings.Builder
	for _, k := range c.topo.AllKeys() {
		fmt.Fprintf(&checks, "test -d staking/l1/%d && ", k)
	}
	for _, i := range c.instancesOnHost(host) {
		fmt.Fprintf(&checks, "test -f %s && ", c.instances[i].chainCfg)
	}
	out := c.ssh(host, fmt.Sprintf(
		"cd %s 2>/dev/null && test -f bin/avalanchego && test -f plugins/%s && "+
			"test -f node-config.json && test -f subnet-config.json && "+
			"%secho OK || echo MISSING",
		c.remoteDir, c.subnetEVMID, checks.String()))
	return out == "OK"
}

// isRPCNode reports whether machine i is a pinned dedicated-RPC node: the RPC slots of each
// site (m5/m6, and b5/b6 in two-site mode). RPC nodes run chain-config-rpc.json, kept SEPARATE
// from the validator/spare chain-config.json so RPC-only settings can be tuned independently.
// Both files now carry the SAME sync rules (state-sync + pruning), so an RPC self-heals via
// state-sync and seeds from the same pruned snapshot as every other role. (Previously the RPC
// config was an archive profile: pruning + state-sync disabled: which held full history but
// had no self-heal path.) Index-based, holds in single- and two-site mode. Safe for i<0.
func (c *config) isRPCNode(i int) bool {
	if i < 0 {
		return false
	}
	return c.topo.isRPCSlot(i)
}

// backupSiteMinDelayMS is the block-production cadence the BACKUP site (B) runs at whenever it
// holds the validator set: i.e. after a failover. min-delay-target only affects a node while it
// is PRODUCING (proposing); a tracker applying another site's blocks ignores it. So site B still
// tracks the primary at full 25ms cadence during normal operation, and only its OWN production
// (post-failover) is slowed to ~10 blk/s: below the ~14 blk/s replay ceiling, so a recovering
// primary's tip stays catchable WITHOUT any mid-restore rolling restart. The throttle is simply
// baked into site B's config; the primary (A) keeps chain-config.json's 25ms.
const backupSiteMinDelayMS = 100

// deployChainConfig writes pool slot i's ROLE-appropriate chain-config to its instance config
// file (which the start script copies into place): chain-config-rpc.json for the pinned RPC
// nodes, chain-config.json for validators and the spare. The two files are kept SEPARATE so RPC
// settings can be tuned independently, but they now carry the SAME sync rules (state-sync +
// pruning): so every role shares one snapshot shape and self-heals via state-sync if it falls
// more than state-sync-min-blocks behind. (Previously the RPC file was an archive profile with
// pruning + state-sync disabled for full history; RPCs no longer serve arbitrary-height eth_
// queries, but they now recover by a plain resync instead of an archive->archive clone.) For a
// normal node the file is chain-config.json (startScript unchanged); a co-located instance gets
// chain-config-N.json so housemates with different roles don't clobber each other. On the BACKUP
// site it also lowers min-delay-target to backupSiteMinDelayMS so that site produces slowly when
// it takes over. Re-applied on restore so a node's pruning/state-sync mode is reset to match its DB.
func (c *config) deployChainConfig(i int) {
	in := c.instances[i]
	src := "chain-config.json"
	if c.isRPCNode(i) {
		src = "chain-config-rpc.json"
	}
	c.scp(c.repoDir+"/"+src, in.host, c.remoteDir+"/"+in.chainCfg, false)
	if c.topo.Site(i) == siteB {
		c.setMinDelay(i, backupSiteMinDelayMS)
	}
}

// setMinDelay rewrites min-delay-target (ms) in pool slot i's chain-config file. Takes effect on
// the node's next (re)start: the VM reads min-delay-target once at init (subnet-evm vm.go).
func (c *config) setMinDelay(i, ms int) {
	in := c.instances[i]
	c.ssh(in.host, fmt.Sprintf(`cd %s && sed -i 's/"min-delay-target": *[0-9]*/"min-delay-target": %d/' %s`, c.remoteDir, ms, in.chainCfg))
}

// upload pushes all artifacts a box needs. Pass 0 of reconcile. The binary, plugin,
// shared configs and committed key sets are pushed ONCE per physical box; the
// role chain-config is written per instance the box hosts (one for a normal box, one
// per co-located process otherwise).
func (c *config) upload(host string) {
	c.ssh(host, fmt.Sprintf("mkdir -p %s/bin %s/plugins %s/staking/l1", c.remoteDir, c.remoteDir, c.remoteDir))
	c.rsyncUpload(c.repoDir+"/bin/avalanchego", host, c.remoteDir+"/bin/")
	c.rsyncUpload(c.repoDir+"/bin/"+c.subnetEVMID, host, c.remoteDir+"/plugins/")
	c.scp(c.repoDir+"/node-config.json", host, c.remoteDir+"/", false)
	c.scp(c.repoDir+"/subnet-config.json", host, c.remoteDir+"/", false)
	for _, k := range c.topo.AllKeys() {
		c.scp(fmt.Sprintf("%s/staking/l1/%d", c.repoDir, k), host, c.remoteDir+"/staking/l1/", true)
	}
	for _, i := range c.instancesOnHost(host) {
		c.deployChainConfig(i)
	}
}
