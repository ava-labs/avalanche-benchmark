// Command benchmark-fleet drives the two-datacenter Avalanche benchmark L1.
//
// Every machine wears ONE permanent staking identity (internal/topo); the
// validator+spare slots of both sites are all registered as L1 validators at
// conversion and NEVER re-registered. Two independent axes operate the fleet:
//
//	up/down:   kill or (re)start avalanchego on a box. `down` hard-kills
//	           (simulated failure, data left on disk); `up` rebuilds the box
//	           from genesis (wipes L1 chainData, keeps the Fuji P-chain).
//	           Neither touches on-chain weight: killing or reviving a box must
//	           never depend on Fuji quorum.
//	weight:    <tier> <m...> [<tier> <m...>]... moves registered identities'
//	           consensus weight between the three tiers through the
//	           ValidatorManager contract on Fuji's C-chain (weights.go). One
//	           invocation is ONE seesaw with raises ordered before lowers, so
//	           `weight validator 7 8 9 dead 1 2 3` is a whole site failover:
//	           no low-weight window, and each lower fits a single churn step
//	           because the raises landed first. Splitting it into two runs
//	           costs a geometric ratchet against a shrinking total. It never
//	           starts or stops a process. `status` shows both axes per DC.
//
// The binary self-loads .env + network.env from the repo root, so it is the
// whole interface: run it directly (or via ./fleet). No wrapper scripts.
package main

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "benchmark-fleet: "+format+"\n", args...)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: benchmark-fleet <command>
  up   <m...>       rebuild the given machines from genesis and start them
                    (wipes their L1 chain data; on-chain weight untouched)
  down <m...>       simulate hardware failure: hard-kill the given machines
                    (data left on disk; on-chain weight untouched)
  weight <tier> <m...> [<tier> <m...>]...
                    set on-chain weight; tier = validator|spare|dead (1000000|1000|1)
                    put all changes of one failover in a single command: new
                    validators are raised before old ones are lowered,
                    e.g.: weight validator 7 8 9 dead 1 2 3
  status [--watch]  read-only report: stake tier and reachability per datacenter
  fresh             WIPE every machine and redeploy the whole fleet from genesis
                    (site A active; destroys all chain data)
  destroy           kill every node and remove the deploy dir on every machine;
                    keeps network.env (the chain identity costs AVAX to recreate)
  endpoints         print one line per machine: name, site, role, host, HTTP port
                    (tab-separated; used by monitoring.sh / bombard.sh)`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	loadEnvFiles()
	cmd := os.Args[1]

	// `endpoints` is a pure read of the IP env (no SSH/chain config), so it runs
	// before the full loadConfig: monitoring.sh/bombard.sh call it with only the
	// IP env available.
	if cmd == "endpoints" {
		rejectArgs(os.Args[2:])
		topo, _, insts := loadPool()
		printEndpoints(topo, insts)
		return
	}

	cfg := loadConfig()
	topo := cfg.topo

	switch cmd {
	case "status":
		watch := false
		for _, a := range os.Args[2:] {
			if a != "--watch" {
				fatalf("status: unknown argument %q (only --watch)", a)
			}
			watch = true
		}
		status(cfg, watch)

	case "up", "down":
		cfg.warnColocation()
		ms := parseMachines(os.Args[2:], topo)
		intents := mustLoadIntents(cfg)
		down := cmd == "down"
		for _, m := range ms {
			next, err := setCordon(intents, m, down)
			if err != nil {
				fatalf("%v", err)
			}
			intents = next
		}
		mustSaveIntents(cfg, intents)
		if down {
			// Simulated hardware failure: hard SIGKILL, no wipe — the box "dies"
			// with its data intact. Weight is untouched (killing must never
			// depend on Fuji quorum); flip stake with `weight`.
			for _, m := range ms {
				fmt.Printf("== down %s (%s): SIGKILL (simulated failure) ==\n", topo.MachineName(m-1), cfg.nodeIPs[m-1])
				cfg.killNode(m - 1)
			}
		} else {
			// Recovery: rebuild each target from genesis, then start.
			fresh := map[int]bool{}
			for _, m := range ms {
				fresh[m-1] = true
			}
			reconcile(cfg, intents, fresh, false)
		}

	case "weight":
		cfg.warnColocation()
		assign := parseWeightArgs(os.Args[2:], topo)
		intents := mustLoadIntents(cfg)
		for m, w := range assign {
			next, err := setWeight(intents, m, w, topo)
			if err != nil {
				fatalf("%v", err)
			}
			intents = next
		}
		mustSaveIntents(cfg, intents)
		printIntents(topo, intents)
		reconcileWeights(cfg, intents)

	case "destroy":
		rejectArgs(os.Args[2:])
		destroy(cfg)

	case "fresh":
		rejectArgs(os.Args[2:])
		cfg.warnColocation()
		intents := seedIntents(topo)
		mustSaveIntents(cfg, intents)
		fmt.Println("== benchmark-fleet fresh: full redeploy from genesis (site A active) ==")
		printIntents(topo, intents)
		all := map[int]bool{}
		for i := range intents {
			all[i] = true
		}
		reconcile(cfg, intents, all, true)
		reconcileWeights(cfg, intents)

	default:
		usage()
	}
}

func mustLoadIntents(cfg *config) []MachineIntent {
	intents, err := loadIntents(cfg.stateFile, cfg.topo)
	if err != nil {
		fatalf("%v", err)
	}
	return intents
}

func mustSaveIntents(cfg *config, intents []MachineIntent) {
	if err := saveIntents(cfg.stateFile, intents); err != nil {
		fatalf("%v", err)
	}
}

// rejectArgs errors on any argument to a verb that takes none. Silently
// ignoring extras is how `destroy --dry-run` wipes a fleet.
func rejectArgs(args []string) {
	if len(args) > 0 {
		fatalf("unexpected argument %q", args[0])
	}
}

// parseMachines parses one or more 1-based machine numbers (deduped, order
// preserved). Each number must be in range 1..Size; there are no flags, and
// anything flag-shaped is an error rather than a silent skip.
func parseMachines(args []string, topo Topology) []int {
	var out []int
	seen := map[int]bool{}
	for _, a := range args {
		m, err := strconv.Atoi(a)
		if err != nil || m < 1 || m > topo.Size() {
			fatalf("machine must be 1..%d, got %q", topo.Size(), a)
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		fatalf("need at least one machine number (1..%d)", topo.Size())
	}
	return out
}

// parseWeightArgs parses `<tier> <m...> [<tier> <m...>]...` into a
// machine -> weight assignment, so ONE invocation (and thus one converge, with
// raises ordered before lowers) can express a whole seesaw:
// `weight validator 7 8 9 dead 1 2 3`.
func parseWeightArgs(args []string, topo Topology) map[int]uint64 {
	tiers := map[string]uint64{
		"validator": valmgr.ValidatorWeight,
		"spare":     valmgr.SpareWeight,
		"dead":      valmgr.DeadWeight,
	}
	assign := map[int]uint64{}
	var w uint64
	haveTier := false
	for _, a := range args {
		if tw, ok := tiers[a]; ok {
			w = tw
			haveTier = true
			continue
		}
		m, err := strconv.Atoi(a)
		if err != nil || m < 1 || m > topo.Size() {
			fatalf("expected a tier (validator|spare|dead) or a machine 1..%d, got %q", topo.Size(), a)
		}
		if !haveTier {
			fatalf("machine %d before any tier; usage: weight <tier> <m...> [<tier> <m...>]...", m)
		}
		if _, dup := assign[m]; dup {
			fatalf("machine %d listed twice", m)
		}
		assign[m] = w
	}
	if len(assign) == 0 {
		fatalf("usage: weight <validator|spare|dead> <m...> [<tier> <m...>]...")
	}
	return assign
}

// printEndpoints writes one tab-separated line per pool slot —
// name, site (a|b), role (v1/v2/v3/spare/rpc), host, httpPort — so bash callers
// (monitoring, benchmark) target each node's ACTUAL port instead of assuming 9652,
// which is wrong for a co-located 2nd+ instance on a box.
func printEndpoints(topo Topology, insts []instance) {
	for i, in := range insts {
		site := "a"
		if topo.Site(i) == siteB {
			site = "b"
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%d\n", topo.MachineName(i), site, topo.SlotRoleName(i), in.host, in.httpPort)
	}
}

func printIntents(topo Topology, intents []MachineIntent) {
	for i, in := range intents {
		state := "up"
		if in.Cordoned {
			state = "down"
		}
		fmt.Printf("  %s: weight=%-6d %-9s %s\n",
			topo.MachineName(i), in.Weight, weightRole(in.Weight), state)
	}
}

// reconcile converges process reality to intents. freshSet names instances to
// rebuild from genesis first (kill + wipe L1 chainData, keep the P-chain).
// forceUpload re-ships binaries to every box regardless of what's already there
// (whole-fleet fresh); otherwise a box is uploaded only if it is missing
// artifacts. Passes: ensure-provisioned, stop-swap, start.
func reconcile(cfg *config, intents []MachineIntent, freshSet map[int]bool, forceUpload bool) {
	topo := cfg.topo
	hosts := cfg.nodeIPs

	fmt.Println("[0/3] ensure provisioned...")
	if forceUpload {
		// Whole-fleet fresh: kill+wipe every target BEFORE any upload, so a
		// re-upload never hits ETXTBSY against a still-running co-located plugin.
		for i := range hosts {
			if freshSet[i] {
				fmt.Printf("  %s (%s): fresh clean\n", topo.MachineName(i), hosts[i])
				cfg.freshClean(i)
			}
		}
		// Uploads are per-host independent (all state is remote), so fan out;
		// any failure fatalfs and aborts the whole run, same as sequentially.
		uploaded := map[string]bool{}
		var wg sync.WaitGroup
		for i, host := range hosts {
			if uploaded[host] {
				continue
			}
			uploaded[host] = true
			fmt.Printf("  %s (%s): upload artifacts\n", topo.MachineName(i), host)
			wg.Add(1)
			go func(h string) { defer wg.Done(); cfg.upload(h) }(host)
		}
		wg.Wait()
	} else {
		checked := map[string]bool{}
		var wg sync.WaitGroup
		for i, host := range hosts {
			if intents[i].Cordoned || checked[host] {
				continue
			}
			checked[host] = true
			wg.Add(1)
			go func(i int, host string) {
				defer wg.Done()
				if !cfg.provisioned(host) {
					fmt.Printf("  %s (%s): missing artifacts, uploading\n", topo.MachineName(i), host)
					cfg.upload(host)
				} else {
					fmt.Printf("  %s (%s): provisioned\n", topo.MachineName(i), host)
				}
			}(i, host)
		}
		wg.Wait()
		for i := range hosts {
			if freshSet[i] {
				fmt.Printf("  %s (%s): rebuild from genesis\n", topo.MachineName(i), hosts[i])
				cfg.freshClean(i)
			}
		}
	}

	// Observe reality, then plan.
	fmt.Println("observe...")
	obs := make([]Observed, len(hosts))
	for i := range hosts {
		obs[i] = cfg.observe(i)
		fmt.Printf("  %s: alive=%v key=%d\n", topo.MachineName(i), obs[i].Alive, obs[i].ActualKey)
	}
	actions := Plan(topo, intents, obs)

	// Pass 1 — stop-swap (every stop+swap before any start). Swaps only ever
	// install a slot's permanent key (first provision / healing).
	fmt.Println("[1/3] stop-swap...")
	for _, a := range actions {
		if a.Stop {
			fmt.Printf("  %s: stop\n", topo.MachineName(a.Machine-1))
			cfg.stop(a.Machine - 1)
		}
		if a.SwapKey != 0 {
			fmt.Printf("  %s: install permanent key %d\n", topo.MachineName(a.Machine-1), a.SwapKey)
			cfg.swap(a.Machine-1, a.SwapKey)
		}
	}

	// Pass 2 — start.
	fmt.Println("[2/3] start...")
	for _, a := range actions {
		if a.Start {
			fmt.Printf("  %s: start\n", topo.MachineName(a.Machine-1))
			cfg.start(a.Machine - 1)
		}
	}

	fmt.Println("\nProcesses reconciled. Watch live node state with:  ./fleet status --watch")
}

// destroy tears the fleet down: kill avalanchego + plugin children and remove
// the whole deploy dir on every box, then drop the local process-state file.
// network.env is deliberately KEPT: it records the chain identity (subnet,
// chain, manager, registered validators), which persists on Fuji and costs
// AVAX to recreate. Deleting it is a manual decision, never a side effect.
func destroy(cfg *config) {
	done := map[string]bool{}
	for i, host := range cfg.nodeIPs {
		if done[host] {
			continue
		}
		done[host] = true
		fmt.Printf("== destroy %s (%s): kill all + rm %s ==\n", cfg.topo.MachineName(i), host, cfg.remoteDir)
		cfg.ssh(host, "pkill -KILL -x avalanchego || true; pkill -KILL -f '"+pluginPat+"' || true; rm -rf "+cfg.remoteDir)
	}
	if err := os.Remove(cfg.stateFile); err != nil && !os.IsNotExist(err) {
		fatalf("remove %s: %v", cfg.stateFile, err)
	}
	fmt.Println("\nFleet destroyed. network.env kept; redeploy with ./fleet fresh.")
}

// status runs ONLY the read-only report against the current intents — no
// provisioning, stop/start, or weight changes. With watch it repeats until
// interrupted.
func status(cfg *config, watch bool) {
	for {
		if watch {
			fmt.Print("\033[H\033[2J") // clear screen
		}
		intents := mustLoadIntents(cfg)
		fmt.Println("== benchmark-fleet status ==")
		results := cfg.checkHealth(intents)
		reportHealth(cfg, intents, results)
		weightsReport(cfg, intents)
		if !watch {
			return
		}
		time.Sleep(2 * time.Second)
	}
}
