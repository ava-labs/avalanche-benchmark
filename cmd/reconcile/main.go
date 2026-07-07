// Command benchmark-fleet drives the two-datacenter Avalanche benchmark L1.
//
// Every machine wears ONE permanent staking identity (internal/topo); the
// validator+spare slots of both sites are all registered as L1 validators at
// conversion and NEVER re-registered. Two verbs operate the fleet:
//
//	up/down:   the box lifecycle, stake follows. `down` hard-kills avalanchego
//	           (simulated failure, data left on disk) and drops the identity's
//	           on-chain weight to dead; `up` rebuilds the box from genesis
//	           (wipes L1 chainData, keeps the Fuji P-chain), starts it, and
//	           brings its weight back at spare. Promotion to full validator
//	           stays an explicit `weight validator`.
//	weight:    <validator|spare|dead> moves a registered identity's consensus
//	           weight between the three tiers through the ValidatorManager
//	           contract on Fuji's C-chain (weights.go). This is the seesaw; it
//	           never starts or stops a process, so it can also express the odd
//	           states (a down box still holding validator weight to stall the
//	           chain on purpose). `status` shows both columns per datacenter.
//
// The binary self-loads .env + network.env from the repo root, so it is the
// whole interface: run it directly (or via ./fleet). No wrapper scripts.
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "benchmark-fleet: "+format+"\n", args...)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: benchmark-fleet <command>
  up   <m...>                 (re)build the given machines from genesis, start them,
                              and bring their stake back at spare weight
                              (wipes L1 chain data, keeps the Fuji P-chain)
  down <m...>                 simulate hardware failure: hard-kill the given machines
                              (data left on disk) and drop their stake to dead weight
  weight <validator|spare|dead> <m...>
                              set the given machines' on-chain consensus weight tier
                              (validator=1000000, spare=1000, dead=1) and converge it
  status [--watch]            read-only report: per-datacenter stake tier + reachability
  fresh                       WIPE + redeploy the whole fleet from genesis, reseed intents
                              (site A active), force re-upload binaries, converge weights
  endpoints                   print per-slot "name<TAB>site<TAB>role<TAB>host<TAB>httpPort"
                              (used by 04_monitoring.sh / 05_benchmark.sh)`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	loadEnvFiles()
	cmd := os.Args[1]

	// `endpoints` is a pure read of the IP env (no SSH/chain config), so it runs
	// before the full loadConfig — 04/05 call it with only the IP env available.
	if cmd == "endpoints" {
		topo, _, insts := loadPool()
		printEndpoints(topo, insts)
		return
	}

	cfg := loadConfig()
	topo := cfg.topo

	switch cmd {
	case "status":
		status(cfg, hasFlag(os.Args[2:], "--watch"))

	case "up", "down":
		cfg.warnColocation()
		ms := parseMachines(os.Args[2:], topo)
		intents := mustLoadIntents(cfg)
		down := cmd == "down"
		tier := valmgr.SpareWeight
		if down {
			tier = valmgr.DeadWeight
		}
		staked := false
		for _, m := range ms {
			next, err := setCordon(intents, m, down)
			if err != nil {
				fatalf("%v", err)
			}
			// Stake follows the box: down -> dead, up -> spare (promote back to
			// validator explicitly). RPC slots carry no weight.
			if topo.IsStakingSlot(m - 1) {
				if next, err = setWeight(next, m, tier, topo); err != nil {
					fatalf("%v", err)
				}
				staked = true
			}
			intents = next
		}
		mustSaveIntents(cfg, intents)
		if down {
			// Simulated hardware failure: hard SIGKILL, no wipe — the box "dies"
			// with its data intact.
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
		if staked {
			printIntents(topo, intents)
			reconcileWeights(cfg, intents)
		}

	case "weight":
		cfg.warnColocation()
		w, ms := parseWeight(os.Args[2:], topo)
		intents := mustLoadIntents(cfg)
		for _, m := range ms {
			next, err := setWeight(intents, m, w, topo)
			if err != nil {
				fatalf("%v", err)
			}
			intents = next
		}
		mustSaveIntents(cfg, intents)
		printIntents(topo, intents)
		reconcileWeights(cfg, intents)

	case "fresh":
		cfg.warnColocation()
		intents := seedIntents(topo)
		mustSaveIntents(cfg, intents)
		fmt.Println("== benchmark-fleet fresh: reseeded intents (site A active) ==")
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

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// parseMachines parses one or more 1-based machine numbers (deduped, order
// preserved). Flags are skipped; each number must be in range 1..Size.
func parseMachines(args []string, topo Topology) []int {
	var out []int
	seen := map[int]bool{}
	for _, a := range args {
		if len(a) > 0 && a[0] == '-' {
			continue // skip flags
		}
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

// parseWeight parses `<tier> <m...>`: the first arg is the weight tier, the
// rest are machine numbers. Returns the mapped weight and the machines.
func parseWeight(args []string, topo Topology) (uint64, []int) {
	if len(args) < 2 {
		fatalf("usage: weight <validator|spare|dead> <m...>")
	}
	var w uint64
	switch args[0] {
	case "validator":
		w = valmgr.ValidatorWeight
	case "spare":
		w = valmgr.SpareWeight
	case "dead":
		w = valmgr.DeadWeight
	default:
		fatalf("tier must be validator|spare|dead, got %q", args[0])
	}
	return w, parseMachines(args[1:], topo)
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
		uploaded := map[string]bool{}
		for i, host := range hosts {
			if uploaded[host] {
				continue
			}
			uploaded[host] = true
			fmt.Printf("  %s (%s): upload artifacts\n", topo.MachineName(i), host)
			cfg.upload(host)
		}
	} else {
		checked := map[string]bool{}
		for i, host := range hosts {
			if intents[i].Cordoned || checked[host] {
				continue
			}
			checked[host] = true
			if !cfg.provisioned(host) {
				fmt.Printf("  %s (%s): missing artifacts, uploading\n", topo.MachineName(i), host)
				cfg.upload(host)
			} else {
				fmt.Printf("  %s (%s): provisioned\n", topo.MachineName(i), host)
			}
		}
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
