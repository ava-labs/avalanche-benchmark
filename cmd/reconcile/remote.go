package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/netcfg"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/vset"
)

// config is the runtime environment: the nodes.ini inventory plus .env /
// network.env settings. The binary self-loads everything (see loadEnvFiles),
// so it runs directly with no wrapper.
type config struct {
	nodes        []topo.Node // the nodes.ini inventory, in file order
	instances    []instance  // one per node, parallel to nodes; carries ports + on-box paths
	sshUser      string
	sshKey       string
	remoteDir    string // e.g. ~/avalanche-benchmark (tilde expanded remotely)
	repoDir      string // local repo root, source of upload artifacts
	chainID      string
	subnetID     string
	subnetEVMID  string
	upstreamIPs  string            // public anchor-network peer(s) the RPC tier's P-chain follows (ip:port csv)
	upstreamIDs  string            // their NodeIDs (TLS-pinned; a hijacked IP cannot impersonate)
	nodeIDByName map[string]string // node name -> NodeID, from staking/node-ids.env
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fatalf("missing required env %s (set it in .env / network.env)", key)
	}
	return v
}

// loadEnvFiles makes the binary self-contained: it sources .env + network.env
// from the repo root (REPO_DIR, defaulting to the working dir) and fills the
// same defaults the old _common.sh shell loader supplied, so `benchmark-fleet`
// runs directly with no wrapper. Values already in the real environment win
// (godotenv never overrides), so an ad-hoc `FOO=bar benchmark-fleet ...` still
// overrides a file. The inventory (nodes.ini), SSH_USER and the chain IDs have
// no defaults - they must come from the files (topo.Load / mustEnv report a
// clear error if absent).
func loadEnvFiles() {
	repo := os.Getenv("REPO_DIR")
	if repo == "" {
		if wd, err := os.Getwd(); err == nil {
			repo = wd
		}
		os.Setenv("REPO_DIR", repo)
	}
	// Loaded separately so a missing network.env (pre-02) still lets .env load.
	_ = godotenv.Load(filepath.Join(repo, ".env"))
	_ = godotenv.Load(filepath.Join(repo, "network.env"))

	setDefault("REMOTE_DIR", "~/avalanche-benchmark")
	setDefault("SSH_KEY_PATH", "/home/ubuntu/.ssh/ilya-solohin-failover-bench-2026-05-04")
	setDefault("SUBNET_EVM_ID", "srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy")
	// Public peer the RPC tier follows (kept in sync with _common.sh; rotates
	// on an AVALANCHEGO_COMMIT bump - see bootstrappers.json). Per-network
	// default; netcfg resolves NETWORK from the network.env loaded above.
	setDefault("FUJI_UPSTREAM_IPS", netcfg.Get().UpstreamIPs)
	setDefault("FUJI_UPSTREAM_IDS", netcfg.Get().UpstreamIDs)
}

func setDefault(key, val string) {
	if os.Getenv(key) == "" {
		os.Setenv(key, val)
	}
}

// loadPool reads the nodes.ini inventory and derives per-node instances.
// Split out from loadConfig so `endpoints` can run with just the inventory.
func loadPool() ([]topo.Node, []instance) {
	nodes, err := topo.Load(filepath.Join(os.Getenv("REPO_DIR"), topo.File))
	if err != nil {
		fatalf("%v", err)
	}
	return nodes, buildInstances(nodes)
}

func loadConfig() *config {
	nodes, insts := loadPool()
	c := &config{
		nodes:       nodes,
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
	}
	stakingDir := filepath.Join(c.repoDir, "staking")
	if err := vset.CheckNamedKeyDirs(stakingDir); err != nil {
		fatalf("%v", err)
	}
	entries, err := vset.ReadManifest(stakingDir)
	if err != nil {
		fatalf("%v (run ./setup/00_gen_secrets.sh first)", err)
	}
	c.nodeIDByName = make(map[string]string, len(entries))
	for _, e := range entries {
		c.nodeIDByName[e.Name] = e.NodeID.String()
	}
	return c
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
// "the simulated up/down went wrong": no best-effort skipping. Flows that must
// survive a dead host (the provision sweep) use sshTry instead.
func (c *config) ssh(host, remoteCmd string) string {
	out, err := c.sshTry(host, remoteCmd)
	if err != nil {
		fatalf("ssh %s failed: %v\n  cmd: %s", host, err, remoteCmd)
	}
	return out
}

// sshTry is ssh without the fatalf: an unreachable host is an error the caller
// handles (recorded as down/not-alive) instead of aborting the whole reconcile.
func (c *config) sshTry(host, remoteCmd string) (string, error) {
	cmd := exec.Command("ssh", c.sshArgs(host, remoteCmd)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
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

// pluginPat matches the subnet-evm plugin processes by their on-disk path. The
// "[p]lugins" bracket makes the pattern not match the pkill/pgrep command's own
// shell (whose argv literally contains "[p]lugins", which the regex "plugins"
// does not), avoiding self-termination of the SSH session.
const pluginPat = "avalanche-benchmark/[p]lugins/"

// killNode kills the avalanchego for node i AND its orphaned subnet-evm plugin
// child, then waits for it to disappear. avalanchego runs the VM as a go-plugin child
// process; SIGKILLing the parent orphans the child, which keeps the plugin binary open
// (ETXTBSY) and blocks re-upload, and these orphans accumulate across failover cycles.
// So every stop must reap the plugin too.
//
// On a normal (one-node) box this is the exact-name reap it always was: kill every
// avalanchego + every plugin on the host. On a CO-HOSTED box that broad reap would
// take down the housemate processes, so we instead target THIS node by its unique
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
	fatalf("avalanchego/plugin for %s on %s did not exit after pkill", c.nodes[i].Name, in.host)
}

// killCmds returns the (kill, liveness-probe) shell commands for an instance, choosing
// the broad host-wide reap for a single-node box and a PID-scoped reap for a
// co-hosted one. Split out so killNode stays a simple kill/wait loop.
func (c *config) killCmds(in instance) (kill, alive string) {
	if !in.shared {
		kill = "pkill -KILL -x avalanchego || true; pkill -KILL -f '" + pluginPat + "' || true"
		alive = "{ pgrep -x avalanchego >/dev/null || pgrep -f '" + pluginPat + "' >/dev/null; } && echo A || echo D"
		return kill, alive
	}
	// Co-hosted: find this node's avalanchego by its unique http-port, kill it and
	// its direct children (the go-plugin process), leaving housemates untouched.
	kill = fmt.Sprintf(
		"for pid in $(pgrep -f -- '%s'); do kids=$(pgrep -P \"$pid\" 2>/dev/null || true); kill -KILL \"$pid\" $kids 2>/dev/null || true; done",
		in.procPat)
	alive = fmt.Sprintf("pgrep -f -- '%s' >/dev/null && echo A || echo D", in.procPat)
	return kill, alive
}

// swap wipes the node's active staking dir and installs its permanent
// committed identity from staking/l1/<name>. Wipe-before-write: a crash
// mid-swap leaves the key missing (re-run re-copies), never duplicated.
// Validators MUST carry their BLS signer.key (its absence fails the cp
// loudly); rpc identities never have one - avalanchego self-generates a
// throwaway BLS key on start, which is never registered anywhere.
func (c *config) swap(i int) {
	n, in := c.nodes[i], c.instances[i]
	src := "staking/l1/" + n.Name
	files := src + "/staker.crt " + src + "/staker.key"
	if n.IsValidator() {
		files += " " + src + "/signer.key"
	}
	c.ssh(in.host, fmt.Sprintf(
		"cd %s && rm -rf %s && mkdir -p %s && cp %s %s/",
		c.remoteDir, in.activeDir, in.activeDir, files, in.activeDir))
}

// nodeIDFor returns a node's NodeID from the generated manifest.
func (c *config) nodeIDFor(name string) string {
	id := c.nodeIDByName[name]
	if id == "" {
		fatalf("missing %s in %s/staking/node-ids.env (run ./setup/00_gen_secrets.sh)", name, c.repoDir)
	}
	return id
}

// pchainBeacons returns the --bootstrap-ips/--bootstrap-ids for node i.
// The whole fleet runs --p-chain-follow-only, re-syncing the P-chain forever
// from exactly these peers (two-hop chaining, see FUJI_PLAN.md):
//   - role=rpc nodes follow the pinned public anchor-network peer: the
//     fleet's ONE allowed external TCP (FUJI_UPSTREAM_IPS/IDS; re-check
//     genesis/bootstrappers.json on every avalanchego bump, the hardcoded IPs
//     rotate between releases);
//   - validators follow ALL of the fleet's rpc nodes (dc= is display-only,
//     nothing functional keys off it). Identities are permanent, so this
//     list is static per deploy. NOTE the >=75% beacon-weight startup latch:
//     a (re)starting validator needs ceil(75%) of these rpc beacons
//     connected - with 4 rpc nodes, any 3.
func (c *config) pchainBeacons(i int) (ips, nodeIDs string) {
	if c.nodes[i].Role == topo.RoleRPC {
		return c.upstreamIPs, c.upstreamIDs
	}
	var ipL, idL []string
	for j, in := range c.instances {
		if c.nodes[j].Role == topo.RoleRPC {
			ipL = append(ipL, fmt.Sprintf("%s:%d", in.host, in.stakingPort))
			idL = append(idL, c.nodeIDFor(c.nodes[j].Name))
		}
	}
	return strings.Join(ipL, ","), strings.Join(idL, ",")
}

// siblingSeeds returns --state-sync-ips/--state-sync-ids for node i: every
// OTHER node under its permanent identity. Signed-IP gossip never relays the
// fleet's private IPs, so the L1 consensus mesh must be seeded explicitly:
// via state-sync-ids, NOT bootstrap-ids, so fresh siblings never become
// P-chain frontier beacons (the frontier-cap gotcha; proven recipe in the
// 2026-07-03 e2e). Identities never move, so the list is static per deploy.
func (c *config) siblingSeeds(i int) (ips, nodeIDs string) {
	var ipL, idL []string
	for j, in := range c.instances {
		if j == i {
			continue
		}
		ipL = append(ipL, fmt.Sprintf("%s:%d", in.host, in.stakingPort))
		idL = append(idL, c.nodeIDFor(c.nodes[j].Name))
	}
	return strings.Join(ipL, ","), strings.Join(idL, ",")
}

// startScript renders the launch script for node i. Identity lives in the
// files under the node's data/<name>/staking/active dir (swapped in pass 1);
// the only role-dependent parts are the P-chain beacon and sibling-seed lists
// above. The data root is preserved (never wiped here) so a restarted node
// rejoins in seconds. All ports and paths come from the instance, so nodes
// sharing a box land on their own ports and dirs.
//
// Primary-network flags (see FUJI_PLAN.md): the primary network is the anchor
// network itself (--network-id=fuji|mainnet, built-in genesis, per netcfg);
// --partial-sync-primary-network syncs
// ONLY the P-chain (skips Fuji X/C); --p-chain-follow-only keeps the P-chain
// permanently bootstrapping off the beacons: REQUIRED on the inside tiers (a
// stock partial-sync node behind non-validator peers freezes after the
// bootstrap-to-consensus handoff) and what the RPC tier was e2e-proven with;
// --network-allow-private-ips lets the fleet dial its DC-internal addresses.
func (c *config) startScript(i int) string {
	in := c.instances[i]
	beaconIPs, beaconIDs := c.pchainBeacons(i)
	seedIPs, seedIDs := c.siblingSeeds(i)
	// Validators load their committed BLS key; rpc identities never have one,
	// not even on disk (avalanchego FATALs on a missing signer-key file, it
	// does NOT auto-generate), so they run an in-memory ephemeral signer that
	// is never registered anywhere.
	signerFlag := "--staking-signer-key-file=" + in.activeDir + "/signer.key"
	if c.nodes[i].Role == topo.RoleRPC {
		signerFlag = "--staking-ephemeral-signer-enabled=true"
	}
	return fmt.Sprintf(`#!/bin/bash
set -e
cd %[1]s

mkdir -p "%[7]s/configs/chains/%[2]s" "%[7]s/configs/subnets" "%[7]s/db" "%[7]s/logs"
cp chain-config.json "%[7]s/configs/chains/%[2]s/config.json"
cp subnet-config.json "%[7]s/configs/subnets/%[3]s.json"

# Belt and braces for the stdout capture: coreth once INFO-logged every
# bombarded tx into avalanchego.out (~5.7 GB/h per RPC node, disk full in
# ~15h, avalanchego FATALs at 3%% free). chain-config log-level=warn kills
# the spam at the source; this watchdog caps the file at 2 GiB regardless
# (the file is open O_APPEND, so truncate reclaims space with no restart).
# The pkill keeps restarts from stacking watchdogs; the trailing ';' in the
# pattern anchors it so co-hosted nodes' dirs don't cross-match.
pkill -f "outwatch=%[7]s;" || true
setsid bash -c 'outwatch=%[7]s; while sleep 60; do
    [ "$(stat -c%%s "$outwatch/logs/avalanchego.out" 2>/dev/null || echo 0)" -gt 2147483648 ] &&
        truncate -s 0 "$outwatch/logs/avalanchego.out"
done' >/dev/null 2>&1 < /dev/null &

# Memory guard: a lagging node's subnet-evm plugin pins undecided processing
# blocks without bound (seen: 49.9 GB RSS on a 61 GiB box; the kernel OOM then
# wedged the whole machine). GOMEMLIMIT is inherited by the plugin child and
# makes the Go runtime GC hard at 75%% of the box's RAM instead of growing
# forever. Nodes are raw setsid processes (no systemd unit), so there is no
# MemoryMax hard stop; instead the raised oom_score_adj (also inherited) makes
# the kernel kill the node tree first, not sshd, if it still exceeds physical
# RAM. NOTE: co-hosted nodes each get 75%%, acceptable on test-only boxes.
export GOMEMLIMIT=$(awk '/MemTotal/{printf "%%dB", $2*1024*3/4}' /proc/meminfo)
echo 500 > /proc/self/oom_score_adj || true

setsid ./bin/avalanchego \
    --http-port=%[8]d \
    --staking-port=%[9]d \
    --http-host=0.0.0.0 \
    --public-ip=%[4]s \
    --db-dir=%[7]s/db \
    --db-type=pebbledb \
    --log-dir=%[7]s/logs \
    --data-dir=%[7]s \
    --network-id=%[13]s \
    --partial-sync-primary-network=true \
    --p-chain-follow-only=true \
    --network-allow-private-ips=true \
    --staking-tls-cert-file=%[10]s/staker.crt \
    --staking-tls-key-file=%[10]s/staker.key \
    %[14]s \
    --plugin-dir=$(pwd)/plugins \
    --config-file=node-config.json \
    --chain-config-dir=%[7]s/configs/chains \
    --subnet-config-dir=%[7]s/configs/subnets \
    --track-subnets="%[3]s" \
    --bootstrap-ips=%[5]s \
    --bootstrap-ids=%[6]s \
    --state-sync-ips=%[11]s \
    --state-sync-ids=%[12]s \
    >%[7]s/logs/avalanchego.out 2>&1 < /dev/null &
`, c.remoteDir, c.chainID, c.subnetID, in.host, beaconIPs, beaconIDs,
		in.dataDir, in.httpPort, in.stakingPort, in.activeDir,
		seedIPs, seedIDs, netcfg.Get().Name, signerFlag)
}

func (c *config) start(i int) {
	in := c.instances[i]
	script := c.startScript(i)
	remoteScript := c.remoteDir + "/" + in.startScript
	c.sshStdin(in.host,
		fmt.Sprintf("cat > %s && chmod +x %s && %s", remoteScript, remoteScript, remoteScript),
		script)
}

// freshClean kills the process for node i and resets it to a from-genesis
// L1 while PRESERVING the already-synced anchor P-chain. Used by `fleet
// fresh` and `fleet up`. It wipes ONLY the L1 EVM state
// (data/<name>/chainData) and the active staking dir (re-installed by swap),
// NOT the whole data root: keeping data/<name>/db keeps the P-chain, so a
// fresh raise no longer re-replays the anchor chain (minutes, bursty) before
// the fleet can serve.
//
// On restart the dedup-fixed proposervm (containerman17/fde >= 4274f639) sees
// its outer blocks are all above the now-empty inner frontier, rolls the L1
// back to genesis, and state-syncs to tip. This REQUIRES that fixed binary: on
// the pre-fix binary a chainData-only wipe bricks chain creation ("inner block
// unavailable for deduplicated block"). C-chain is unaffected - its coreth ethdb
// lives inside db/ and its chainData dir is empty, so this never resets it.
// Only this node's dirs are touched, so co-hosted housemates are intact.
func (c *config) freshClean(i int) {
	in := c.instances[i]
	c.killNode(i)
	c.ssh(in.host, fmt.Sprintf(
		"cd %s 2>/dev/null && rm -rf %s/chainData %s || true; "+
			"mkdir -p %s/bin %s/plugins %s/staking/l1",
		c.remoteDir, in.dataDir, in.activeDir, c.remoteDir, c.remoteDir, c.remoteDir))
	c.clearBootstrapBacklog(i)
}

// clearBootstrapBacklog drops the L1 chain's fetched-but-not-executed bootstrap
// blocks from the node's shared db/ (bin/bsclear: one pebble DeleteRange over
// the chain's interval_bs prefix; the P-chain and every other chain live under
// disjoint sha256 prefixes and are untouched, staking keys are not in the db).
// Bootstrap blocks survive a chainData wipe - they live in db/, not chainData/ -
// which used to resurrect half a bootstrap after a rebuild AND force a
// multi-minute UNLOGGED Bootstrapper.Clear grind before "starting state sync"
// could appear (the 2026-07-11 silent-stall window). MUST run while the node
// is down (both callers kill it first). A box without the tool or without a db
// yet (first provision) skips with a note.
func (c *config) clearBootstrapBacklog(i int) {
	in := c.instances[i]
	dbDir := fmt.Sprintf("%s/db/%s/pebble", in.dataDir, netcfg.Get().Name)
	c.ssh(in.host, fmt.Sprintf(
		"cd %s 2>/dev/null || exit 0; if [ -x bin/bsclear ] && [ -d %s ]; then ./bin/bsclear %s %s; else echo 'bsclear: skipped (no tool or no db yet)'; fi",
		c.remoteDir, dbDir, dbDir, c.chainID))
}

// rebuildWedged is waitServing's in-place repair for a node that will never
// reach the tip on its own: a fork wedge (self-finalized sibling block, height
// frozen forever) or a genuinely stalled bootstrap (no progress for its stall
// budget). Exactly the live repair recipe: kill it, wipe ONLY the L1 EVM state
// (data root chainData; NEVER the shared db/ holding the P-chain, NEVER the
// active staking dir) plus the chain's bootstrap backlog inside db/ (so the
// restart state-syncs clean instead of silently grinding Bootstrapper.Clear),
// restart. The node rolls the L1 back to genesis and state-syncs onto the live
// branch; identity and P-chain are untouched, so no key swap or
// re-provisioning is needed. Fatal on ssh failure: this targets one specific
// host and must fail loudly if that host is unreachable.
func (c *config) rebuildWedged(i int) {
	in := c.instances[i]
	c.killNode(i)
	c.ssh(in.host, fmt.Sprintf("cd %s && rm -rf %s/chainData", c.remoteDir, in.dataDir))
	c.clearBootstrapBacklog(i)
	c.start(i)
}

// provisioned reports whether the box already has binary, plugin, shared configs
// AND the committed key set for every node it hosts. An ssh-unreachable host
// returns the error instead of aborting: the sweep records it as a dead host
// and reconciles the rest.
func (c *config) provisioned(host string) (bool, error) {
	var checks strings.Builder
	for _, i := range c.instancesOnHost(host) {
		fmt.Fprintf(&checks, "test -d staking/l1/%s && ", c.nodes[i].Name)
	}
	out, err := c.sshTry(host, fmt.Sprintf(
		"cd %s 2>/dev/null && test -f bin/avalanchego && test -f bin/bsclear && test -f plugins/%s && "+
			"test -f node-config.json && test -f subnet-config.json && test -f chain-config.json && "+
			"%secho OK || echo MISSING",
		c.remoteDir, c.subnetEVMID, checks.String()))
	return out == "OK", err
}

// upload pushes all artifacts a box needs. Pass 0 of reconcile. The binary,
// plugin and shared configs go once per physical box; each node the box hosts
// gets its OWN committed key set (staking/l1/<name>) - a box never holds
// another node's keys, and rpc key sets carry no BLS signer key at all.
func (c *config) upload(host string) {
	c.ssh(host, fmt.Sprintf("mkdir -p %s/bin %s/plugins %s/staking/l1", c.remoteDir, c.remoteDir, c.remoteDir))
	c.rsyncUpload(c.repoDir+"/bin/avalanchego", host, c.remoteDir+"/bin/")
	c.rsyncUpload(c.repoDir+"/bin/bsclear", host, c.remoteDir+"/bin/")
	c.rsyncUpload(c.repoDir+"/bin/"+c.subnetEVMID, host, c.remoteDir+"/plugins/")
	c.scp(c.repoDir+"/node-config.json", host, c.remoteDir+"/", false)
	c.scp(c.repoDir+"/subnet-config.json", host, c.remoteDir+"/", false)
	c.scp(c.repoDir+"/chain-config.json", host, c.remoteDir+"/", false)
	for _, i := range c.instancesOnHost(host) {
		name := c.nodes[i].Name
		local := filepath.Join(c.repoDir, "staking", "l1", name)
		if _, err := os.Stat(local); err != nil {
			fatalf("missing key set %s for node %s (run ./setup/00_gen_secrets.sh)", local, name)
		}
		c.scp(local, host, c.remoteDir+"/staking/l1/", true)
	}
}
