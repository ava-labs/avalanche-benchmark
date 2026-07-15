// Command benchmark-fleet drives the HARDWARE of the benchmark L1 fleet:
// provisioning, process up/down, health. It never touches on-chain state;
// validator weights are the cmd/l1 binary's job (`bin/l1 set-weight` /
// `bin/l1 apply`), and this tool only READS them back for the status display
// and the Prometheus exporter.
//
// The fleet inventory is nodes.ini (internal/topo): every node has a NAME,
// which is the CLI handle here, the staking key dir (staking/l1/<name>) and
// the node's data root on its box (data/<name>). Every node wears ONE
// permanent staking identity; the role=validator nodes are all registered as
// L1 validators at `l1 create` and NEVER re-registered. up/down kill or
// (re)start avalanchego on a box: `down` hard-kills (simulated failure, data
// left on disk); `up` rebuilds the node from genesis (wipes L1 chainData,
// keeps the anchor P-chain). Neither touches on-chain weight: killing or
// reviving a box must never depend on anchor-network quorum.
//
// The binary self-loads nodes.ini + .env + network.env from the repo root, so
// it is the whole interface: run it directly (or via ./fleet). No wrapper
// scripts.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "benchmark-fleet: "+format+"\n", args...)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: benchmark-fleet <command>
  up   <node...>    rebuild the given nodes from genesis, start them and wait
                    until they are SERVING at the fleet tip (wipes their L1 chain
                    data; on-chain weight untouched); nodes already serving at
                    tip are skipped, nodes catching up or bootstrapping are
                    waited on while they make progress; a node that stalls
                    (frozen height = fork wedge, or no bootstrap movement) is
                    rebuilt automatically (chainData + bootstrap-backlog wipe,
                    state re-sync)
  down <node...>    simulate hardware failure: hard-kill the given nodes
                    (data left on disk; on-chain weight untouched)
  status            read-only report: on-chain stake tier and reachability
                    (weights MOVE via bin/l1 set-weight / apply)
  fresh             WIPE every node and redeploy the whole fleet from genesis
                    (destroys all chain data; on-chain weights untouched)
  destroy           kill every node and remove the deploy dir on every machine;
                    keeps network.env (the chain identity costs AVAX to recreate)
  endpoints         print one line per node: name, dc, role, host, HTTP port
                    (tab-separated; used by run/02_monitoring.sh / run/03_bombard.sh)
  exporter          serve the fleet_actual_weight Prometheus gauge on :9091
                    (started by run/02_monitoring.sh)

  <node...> are nodes.ini names (a1, rpc_b2, ...); dc=<tag> selects every node
  wearing that dc tag.`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	loadEnvFiles()
	cmd := os.Args[1]

	// `endpoints` is a pure read of nodes.ini (no SSH/chain config), so it runs
	// before the full loadConfig: run/02_monitoring.sh and run/03_bombard.sh
	// call it with only the inventory available.
	if cmd == "endpoints" {
		rejectArgs(os.Args[2:])
		nodes, insts := loadPool()
		printEndpoints(nodes, insts)
		return
	}

	cfg := loadConfig()

	switch cmd {
	case "status":
		rejectArgs(os.Args[2:])
		status(cfg)

	case "down":
		cfg.warnColocation()
		ms := parseNodes(os.Args[2:], cfg.nodes)
		// Simulated hardware failure: hard SIGKILL, no wipe - the box "dies"
		// with its data intact. Weight is untouched (killing must never
		// depend on anchor-network quorum); flip stake with bin/l1.
		for _, m := range ms {
			fmt.Printf("== down %s (%s): SIGKILL (simulated failure) ==\n", cfg.nodes[m].Name, cfg.nodes[m].Host)
			cfg.killNode(m)
		}

	case "up":
		cfg.warnColocation()
		ms := parseNodes(os.Args[2:], cfg.nodes)
		// Treatment is keyed on OBSERVED health alone, per requested node:
		// a node already answering RPC AT THE FLEET TIP is healthy - leave it
		// alone instead of wiping and rebuilding it. A CATCHING UP or
		// BOOTSTRAPPING node is also left running - it is making its way
		// to the tip and a wipe would only destroy that progress (the
		// 2026-07-11 wipe-loop: `up` freshCleaned nodes mid-bootstrap) -
		// but `up` still blocks until it serves; waitServing rebuilds it
		// there only if it genuinely stalls (no bootstrap-metric or
		// height movement for its stall budget). DOWN nodes (process dead,
		// e.g. after `fleet down`, or host unreachable) get the full rebuild.
		results := cfg.checkHealth()
		var rebuild, wait []int
		for _, m := range ms {
			switch results[m].state {
			case healthServing:
				continue
			case healthCatchingUp, healthBootstrapping:
				wait = append(wait, m)
			default: // healthDown
				rebuild = append(rebuild, m)
				wait = append(wait, m)
			}
		}
		if len(wait) == 0 {
			fmt.Printf("all %d node(s) already up\n", len(ms))
			return
		}
		// Recovery: rebuild each dead target from genesis, start it, then block
		// until every waited-on node answers RPC as SERVING at the
		// fleet tip (within catchUpThreshold of the fleet max height).
		// A requested node whose host is ssh-unreachable cannot be
		// rebuilt: it is dropped from the wait (it would never serve) and
		// the command exits non-zero naming it AFTER the reachable
		// nodes have been brought up.
		var lost []string
		if len(rebuild) > 0 {
			targets := map[int]bool{}
			for _, m := range rebuild {
				targets[m] = true
			}
			unreachable := map[int]bool{}
			for _, m := range reconcile(cfg, targets, false) {
				unreachable[m] = true
			}
			var kept []int
			for _, m := range wait {
				if unreachable[m] {
					lost = append(lost, cfg.nodes[m].Name)
					continue
				}
				kept = append(kept, m)
			}
			wait = kept
		}
		if len(wait) > 0 {
			waitServing(cfg, wait)
		}
		if len(lost) > 0 {
			fatalf("up: host unreachable, not rebuilt: %s", strings.Join(lost, ", "))
		}

	case "exporter":
		rejectArgs(os.Args[2:])
		runExporter(cfg, ":9091")

	case "destroy":
		rejectArgs(os.Args[2:])
		destroy(cfg)

	case "fresh":
		rejectArgs(os.Args[2:])
		cfg.warnColocation()
		fmt.Println("== benchmark-fleet fresh: full redeploy from genesis ==")
		fmt.Println("(on-chain weights untouched; move them with bin/l1 apply / scenarios/00_healthy.sh)")
		all := map[int]bool{}
		for i := range cfg.nodes {
			all[i] = true
		}
		reconcile(cfg, all, true)

	default:
		usage()
	}
}

// rejectArgs errors on any argument to a verb that takes none. Silently
// ignoring extras is how `destroy --dry-run` wipes a fleet.
func rejectArgs(args []string) {
	if len(args) > 0 {
		fatalf("unexpected argument %q", args[0])
	}
}

// parseNodes resolves node arguments (deduped, order preserved) to inventory
// indexes. Each argument is a nodes.ini name (a1, rpc_b2, ...) or a dc=<tag>
// selector expanding to every node wearing that dc tag. Anything else is an
// error rather than a silent skip.
func parseNodes(args []string, nodes []topo.Node) []int {
	var out []int
	seen := map[int]bool{}
	add := func(i int) {
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	for _, a := range args {
		if dc, ok := strings.CutPrefix(a, "dc="); ok {
			matched := false
			for i, n := range nodes {
				if n.DC == dc {
					add(i)
					matched = true
				}
			}
			if !matched {
				fatalf("dc=%s matches no node in nodes.ini", dc)
			}
			continue
		}
		found := -1
		for i, n := range nodes {
			if n.Name == a {
				found = i
				break
			}
		}
		if found < 0 {
			fatalf("unknown node %q (nodes.ini defines: %s)", a, nodeNames(nodes))
		}
		add(found)
	}
	if len(out) == 0 {
		fatalf("need at least one node name (or dc=<tag>); nodes.ini defines: %s", nodeNames(nodes))
	}
	return out
}

func nodeNames(nodes []topo.Node) string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return strings.Join(names, " ")
}

// printEndpoints writes one tab-separated line per node - name, dc ("-" when
// untagged), role, host, httpPort - so bash callers (monitoring, benchmark)
// target each node's ACTUAL port instead of assuming one, which is wrong for
// a co-hosted 2nd+ node on a box.
func printEndpoints(nodes []topo.Node, insts []instance) {
	for i, n := range nodes {
		dc := n.DC
		if dc == "" {
			dc = "-"
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%d\n", n.Name, dc, n.Role, insts[i].host, insts[i].httpPort)
	}
}

// reconcile rebuilds the target nodes from genesis: ensure the box is
// provisioned, kill + wipe the L1 chainData (keep the P-chain db and staking
// identity), install the node's permanent key, start. It touches ONLY the
// targets. forceUpload re-ships binaries to every box regardless of what's
// already there (whole-fleet fresh); otherwise a target box is uploaded only
// if it is missing artifacts.
//
// An ssh-unreachable host is NOT fatal (whole-fleet fresh excepted, where a
// failed upload still aborts): it is warned about and gets no actions, so one
// dead box never blocks reconciling the rest. The returned list names the
// node indexes that were skipped as unreachable; callers that specifically
// targeted one of them fail loudly on it themselves.
func reconcile(cfg *config, targets map[int]bool, forceUpload bool) []int {
	nodes := cfg.nodes

	fmt.Println("[0/3] ensure provisioned...")
	deadHosts := map[string]bool{}
	if forceUpload {
		// Whole-fleet fresh: kill+wipe every target BEFORE any upload, so a
		// re-upload never hits ETXTBSY against a still-running co-hosted plugin.
		for i := range nodes {
			if targets[i] {
				fmt.Printf("  %s (%s): fresh clean\n", nodes[i].Name, nodes[i].Host)
				cfg.freshClean(i)
			}
		}
		// Uploads are per-host independent (all state is remote), so fan out;
		// any failure fatalfs and aborts the whole run, same as sequentially.
		uploaded := map[string]bool{}
		var wg sync.WaitGroup
		for i := range nodes {
			host := nodes[i].Host
			if uploaded[host] {
				continue
			}
			uploaded[host] = true
			fmt.Printf("  %s (%s): upload artifacts\n", nodes[i].Name, host)
			wg.Add(1)
			go func(h string) { defer wg.Done(); cfg.upload(h) }(host)
		}
		wg.Wait()
	} else {
		checked := map[string]bool{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := range nodes {
			host := nodes[i].Host
			if !targets[i] || checked[host] {
				continue
			}
			checked[host] = true
			wg.Add(1)
			go func(i int, host string) {
				defer wg.Done()
				ok, err := cfg.provisioned(host)
				if err != nil {
					fmt.Printf("  %s (%s): WARNING host unreachable over ssh (%v), treating as down\n", nodes[i].Name, host, err)
					mu.Lock()
					deadHosts[host] = true
					mu.Unlock()
					return
				}
				if !ok {
					fmt.Printf("  %s (%s): missing artifacts, uploading\n", nodes[i].Name, host)
					cfg.upload(host)
				} else {
					fmt.Printf("  %s (%s): provisioned\n", nodes[i].Name, host)
				}
			}(i, host)
		}
		wg.Wait()
		for i := range nodes {
			if targets[i] {
				if deadHosts[nodes[i].Host] {
					fmt.Printf("  %s (%s): unreachable, cannot rebuild\n", nodes[i].Name, nodes[i].Host)
					continue
				}
				fmt.Printf("  %s (%s): rebuild from genesis\n", nodes[i].Name, nodes[i].Host)
				cfg.freshClean(i)
			}
		}
	}

	// freshClean left every reachable target dead with an empty active staking
	// dir, so what remains is fixed: install the node's permanent key, then
	// start. Every swap lands before any start (you never write a key under a
	// live process); swaps only ever install a node's PERMANENT key -
	// identities never migrate.
	fmt.Println("[1/3] install keys...")
	for i := range nodes {
		if !targets[i] || deadHosts[nodes[i].Host] {
			continue
		}
		fmt.Printf("  %s: install permanent key staking/l1/%s\n", nodes[i].Name, nodes[i].Name)
		cfg.swap(i)
	}

	fmt.Println("[2/3] start...")
	for i := range nodes {
		if !targets[i] || deadHosts[nodes[i].Host] {
			continue
		}
		fmt.Printf("  %s: start\n", nodes[i].Name)
		cfg.start(i)
	}

	fmt.Println("\nProcesses reconciled. Check node state with:  ./fleet status")

	var unreachable []int
	for i := range nodes {
		if targets[i] && deadHosts[nodes[i].Host] {
			unreachable = append(unreachable, i)
		}
	}
	return unreachable
}

// waitServing blocks until every listed node answers its L1 RPC as SERVING
// AND is within catchUpThreshold of the fleet max height, polling every 5s.
// Each poll probes the WHOLE fleet (checkHealth) so the fleet max is fresh; if
// no other node responds, the fleet max is the node's own height and it
// passes trivially (no deadlock bringing up a lone node). Prints a line when a
// node's state changes, plus a progress line every poll while it is
// catching up or bootstrapping.
//
// There is deliberately NO flat overall deadline: the clock is per node and
// runs on NO PROGRESS (madeProgress: state changes, height movement, bootstrap
// bs_fetched/bs_accepted metric movement), so a node legitimately fetching or
// executing a long bootstrap backlog is waited on as long as it keeps moving.
// The old flat 10-minute timeout fatalfed mid-recovery and, combined with a
// rerunning scenario wrapper, wipe-looped bootstrapping nodes forever
// (2026-07-11 incident).
//
// A node with no progress for its stall budget (stallBudget: generous for
// BOOTSTRAPPING to absorb the silent Bootstrapper.Clear window) is rebuilt in
// place (rebuildWedged: stop, wipe ONLY the L1 chainData + bootstrap backlog,
// keep P-chain db + staking keys, restart) and the wait continues through its
// state re-sync. The proven fast path stays: a CATCHING UP node whose
// height FREEZES across wedgeFrozenPolls polls (sibling-race fork wedge) is
// rebuilt immediately, no budget wait. At most one rebuild per node per
// invocation; when every remaining node has exhausted budget + rebuild, the
// command exits non-zero naming them. Since `up` no longer wipes catching-up
// or bootstrapping nodes, exiting and re-running is always safe.
func waitServing(cfg *config, ms []int) {
	const poll = 5 * time.Second
	type track struct {
		state    nodeHealth // zero value healthDown: the just-(re)built state
		block    uint64
		bs       uint64 // bootstrap counter (bs_fetched+bs_accepted), valid iff bsOK
		bsOK     bool
		progress time.Time // last time this node showed forward motion
		frozenN  int       // consecutive frozen-height polls (fork-wedge detector)
		rebuilt  bool
	}
	tr := map[int]*track{}
	for _, m := range ms {
		tr[m] = &track{progress: time.Now()}
	}
	serving := map[int]bool{}
	fmt.Printf("waiting for %d node(s) to reach SERVING at fleet tip (rebuild after %s without progress while bootstrapping, %s otherwise)...\n",
		len(ms), bootstrapStallBudget, defaultStallBudget)
	for {
		results := cfg.checkHealth()
		fleetMax := fleetMaxBlock(results)
		var stuck []string
		for _, m := range ms {
			if serving[m] {
				continue
			}
			t := tr[m]
			r := results[m]
			name := cfg.nodes[m].Name
			var bs uint64
			var bsOK bool
			if r.state == healthBootstrapping {
				bs, bsOK = cfg.bootstrapCounter(m)
			}
			switch {
			case r.state == healthCatchingUp:
				fmt.Printf("  %s catching up: %d/%d\n", name, r.block, fleetMax)
			case r.state == healthBootstrapping && bsOK:
				fmt.Printf("  %s bootstrapping: %d blocks fetched+accepted\n", name, bs)
			case r.state != t.state:
				fmt.Printf("  %s: %s\n", name, r.state)
			}
			if madeProgress(t.state, r.state, t.block, r.block, t.bs, bs, t.bsOK, bsOK) {
				t.progress = time.Now()
			}
			var wedged bool
			t.frozenN, wedged = wedgeFrozen(r.state, r.block, t.block, t.frozenN)
			stalledFor := time.Since(t.progress)
			stalled := stalledFor > stallBudget(r.state)
			t.state, t.block = r.state, r.block
			if bsOK {
				t.bs, t.bsOK = bs, true
			}
			if r.state == healthServing {
				serving[m] = true
				continue
			}
			if (wedged || stalled) && !t.rebuilt {
				if wedged {
					fmt.Printf("  %s: height FROZEN at %d (%d behind fleet max %d) across %d polls: fork wedge, rebuilding\n",
						name, r.block, fleetMax-r.block, fleetMax, wedgeFrozenPolls+1)
				} else {
					fmt.Printf("  %s: NO progress for %s while %s: genuinely stuck, rebuilding\n",
						name, stalledFor.Round(time.Second), r.state)
				}
				fmt.Printf("  %s: stop + wipe L1 chainData and bootstrap backlog (P-chain db and staking keys kept)\n", name)
				cfg.rebuildWedged(m)
				fmt.Printf("  %s: restarted, waiting for state sync to the live branch\n", name)
				// Full tracker reset: the restarted process reports fresh
				// (lower) heights and zeroed bs counters, which must not be
				// compared against pre-rebuild values.
				*t = track{progress: time.Now(), rebuilt: true}
				continue
			}
			if stalled { // budget exhausted after the one allowed rebuild
				stuck = append(stuck, fmt.Sprintf("%s (%s, no progress for %s after a rebuild)",
					name, r.state, stalledFor.Round(time.Second)))
			}
		}
		if len(serving) == len(ms) {
			return
		}
		if len(stuck) == len(ms)-len(serving) {
			fatalf("up: gave up; every remaining node is stuck: %s; check logs or re-run `./fleet up` (it will not wipe their progress)",
				strings.Join(stuck, ", "))
		}
		time.Sleep(poll)
	}
}

// destroy tears the fleet down: kill avalanchego + plugin children and remove
// the whole deploy dir on every box.
// network.env is deliberately KEPT: it records the chain identity (subnet,
// chain, manager, registered validators), which persists on the anchor
// network and costs AVAX to recreate. Deleting it is a manual decision, never
// a side effect.
func destroy(cfg *config) {
	done := map[string]bool{}
	for i := range cfg.nodes {
		host := cfg.nodes[i].Host
		if done[host] {
			continue
		}
		done[host] = true
		fmt.Printf("== destroy %s (%s): kill all + rm %s ==\n", cfg.nodes[i].Name, host, cfg.remoteDir)
		cfg.ssh(host, "pkill -KILL -x avalanchego || true; pkill -KILL -f '"+pluginPat+"' || true; rm -rf "+cfg.remoteDir)
	}
	fmt.Println("\nFleet destroyed. network.env kept; redeploy with ./fleet fresh.")
}

// inFlightLine parses `ps -eo pid,etimes,args` output and returns one
// "in flight: ./fleet <args> (<elapsed>)" line for any OTHER benchmark-fleet
// command executing on this box, or "" if none. Read-only invocations
// (status, endpoints, exporter) and this process are excluded: the line
// exists so someone watching `watch ./fleet status` knows which
// state-changing phase is producing the transitions they see.
func inFlightLine(psOut string, selfPID int) string {
	var found []string
	for _, l := range strings.Split(psOut, "\n") {
		f := strings.Fields(l)
		if len(f) < 4 { // pid, etimes, binary, subcommand
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil || pid == selfPID {
			continue
		}
		if filepath.Base(f[2]) != "benchmark-fleet" {
			continue
		}
		switch f[3] {
		case "status", "endpoints", "exporter":
			continue
		}
		secs, _ := strconv.Atoi(f[1])
		found = append(found, fmt.Sprintf("./fleet %s (%s)",
			strings.Join(f[3:], " "), time.Duration(secs)*time.Second))
	}
	if len(found) == 0 {
		return ""
	}
	return "in flight: " + strings.Join(found, "; ")
}

// status runs ONLY the read-only report of observed facts - no provisioning,
// stop/start, or weight changes. It reads node APIs and the P-chain, never
// the boxes' disks, so it is layout-agnostic.
func status(cfg *config) {
	fmt.Println("== benchmark-fleet status ==")
	if out, err := exec.Command("ps", "-eo", "pid,etimes,args").Output(); err == nil {
		if l := inFlightLine(string(out), os.Getpid()); l != "" {
			fmt.Println(l)
		}
	}
	results := cfg.checkHealth()
	// Overlay HALTED onto any node whose consensus tip is wedged (a frozen
	// fleet otherwise reads SERVING N/N: nobody is "behind" the fleet max when
	// every node is stuck at the same height). Status-only.
	cfg.markHalted(results)
	// One batch of P-chain reads (read-only; weights MOVE via bin/l1).
	weights, weightsErr := fetchWeights(cfg)
	if weightsErr != nil {
		fmt.Printf("weights: %v (showing reachability only)\n", weightsErr)
	}
	reportHealth(cfg, results, weights)
}
