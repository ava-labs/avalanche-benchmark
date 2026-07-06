// Command reconcile converges a fixed pool of machines — per site 3+
// validators, hot spares and pinned dedicated-RPC nodes, optionally doubled
// across two sites — to the desired state recorded in the intentions JSON.
//
// Every machine wears ONE permanent staking identity (internal/topo); the
// validator+spare slots of both sites are all registered as L1 validators at
// conversion. Reconciliation therefore has two halves:
//
//	processes — stop/start avalanchego per cordon intent (SSH, this package);
//	weights   — converge each registered identity's consensus weight to the
//	            intent through the ValidatorManager contract on the Fuji
//	            C-chain (weights.go). Failover IS a weight change; keys never
//	            move between machines (key swaps produced forks).
//
// The decision logic is pure (plan.go); remote.go does the SSH/scp I/O. Run
// via the bash wrappers, which supply config through the environment.
package main

import (
	"fmt"
	"os"
	"strconv"
)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "reconcile: "+format+"\n", args...)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: reconcile <command>
  fresh              clear cordons, wipe data, reseed intents (site A active), force re-upload,
                     start all, converge weights
  down <m>           cordon machine m; if it held active weight, promote a same-site standby
  up <m>             uncordon machine m (rejoins as a standby; weights are sticky)
  clean <m>          wipe machine m's chain data (keep credentials), then reconcile it back up
  site-failover <a|b>  hard DC failover: nuke the other site, seesaw all active weight to the
                       given site via the ValidatorManager (two-site mode)
  restore <a|b>        graceful weight migration to a site: bring both sites up, wait until the
                       target site serves at tip, then seesaw the weights (two-site mode)
  apply              pure reconcile against the existing intentions (processes + weights)
  status             read-only health report (node state + on-chain weights, no changes)
  verify             read-only proof the live network is ONE branch (no fork) + quorum healthy
  endpoints          print per-slot "name<TAB>site<TAB>role<TAB>host<TAB>httpPort" (co-location-aware;
                     used by 04_monitoring.sh / 05_benchmark.sh to target the right ports)`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	// `endpoints` is a pure read of the IP env (no SSH/chain config needed), so it
	// runs before the full loadConfig — 04/05 call it with only the IP env exported.
	if os.Args[1] == "endpoints" {
		topo, _, insts := loadPool()
		printEndpoints(topo, insts)
		return
	}

	cfg := loadConfig()
	topo := cfg.topo
	// Operator topology warnings only on state-CHANGING commands — never on the
	// read-only status/verify, which are often run under `watch` where the notes
	// would bury the node list.
	switch os.Args[1] {
	case "status", "verify":
	default:
		cfg.warnColocation()
	}

	if os.Args[1] == "status" {
		status(cfg)
		return
	}

	if os.Args[1] == "verify" {
		if !verifyAgreement(cfg) {
			os.Exit(1)
		}
		return
	}

	if os.Args[1] == "clean" {
		m := parseMachine(os.Args, topo)
		fmt.Printf("== reconcile clean %d (%s): wipe chain data, keep credentials ==\n", m, topo.MachineName(m-1))
		cfg.wipeL1Data(m - 1) // stop + wipe ONLY this instance's data dir (co-location safe)
		intents, err := loadIntents(cfg.stateFile, topo)
		if err != nil {
			fatalf("%v", err)
		}
		reconcile(cfg, intents, false) // bring it back up clean against current intent
		return
	}

	fresh := false
	waitSite := -1 // when >=0, wait for this site to serve at tip before the weight seesaw
	var intents []MachineIntent

	switch os.Args[1] {
	case "fresh":
		fresh = true
		intents = seedIntents(topo)
		if err := saveIntents(cfg.stateFile, intents); err != nil {
			fatalf("%v", err)
		}
		fmt.Println("== reconcile fresh: reseeded intentions (site A active) ==")

	case "down", "up":
		m := parseMachine(os.Args, topo)
		prev, err := loadIntents(cfg.stateFile, topo)
		if err != nil {
			fatalf("%v", err)
		}
		intents, err = retarget(prev, m, os.Args[1] == "down", topo)
		if err != nil {
			fatalf("%v", err)
		}
		if err := saveIntents(cfg.stateFile, intents); err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("== reconcile %s %d (%s) ==\n", os.Args[1], m, topo.MachineName(m-1))

	case "site-failover":
		if len(os.Args) < 3 {
			usage()
		}
		target, ok := topo.SiteFromName(os.Args[2])
		if !ok {
			fatalf("site must be 'a' or 'b' (two-site mode requires the BACKUP_* IP lists), got %q", os.Args[2])
		}
		prev, err := loadIntents(cfg.stateFile, topo)
		if err != nil {
			fatalf("%v", err)
		}
		intents, err = retargetSite(prev, topo, target)
		if err != nil {
			fatalf("%v", err)
		}
		if err := saveIntents(cfg.stateFile, intents); err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("== reconcile site-failover %s ==\n", os.Args[2])
		// Model a real disaster: NUKE the down site — hard-kill every node on it at
		// once — so its tip is frozen at the instant of failure. The surviving site's
		// standby validators are live consensus participants at the same tip, so no
		// state surgery is needed: the weight seesaw below IS the recovery.
		downSite := otherSite(target)
		fmt.Printf("== site-failover: nuking site %s (parallel SIGKILL — simulated outage) ==\n", siteName(downSite))
		cfg.nukeSite(downSite)

	case "restore":
		if len(os.Args) < 3 {
			usage()
		}
		target, ok := topo.SiteFromName(os.Args[2])
		if !ok {
			fatalf("site must be 'a' or 'b' (two-site mode requires the BACKUP_* IP lists), got %q", os.Args[2])
		}
		prev, err := loadIntents(cfg.stateFile, topo)
		if err != nil {
			fatalf("%v", err)
		}
		intents, err = retargetRestore(prev, topo, target)
		if err != nil {
			fatalf("%v", err)
		}
		if err := saveIntents(cfg.stateFile, intents); err != nil {
			fatalf("%v", err)
		}
		waitSite = target
		fmt.Printf("== reconcile restore %s (graceful weight migration) ==\n", os.Args[2])

	case "apply":
		var err error
		intents, err = loadIntents(cfg.stateFile, topo)
		if err != nil {
			fatalf("%v", err)
		}
		fmt.Println("== reconcile apply ==")

	default:
		usage()
	}

	printIntents(topo, intents)
	reconcile(cfg, intents, fresh)
	if waitSite >= 0 {
		cfg.waitForSiteReady(intents, waitSite)
	}
	reconcileWeights(cfg, intents)
	// Re-point bombard AFTER the weights actually moved: ingress must follow
	// the consensus, not the intent.
	cfg.writeActiveRPCs(intents)
}

func parseMachine(args []string, topo Topology) int {
	if len(args) < 3 {
		usage()
	}
	m, err := strconv.Atoi(args[2])
	if err != nil || m < 1 || m > topo.Size() {
		fatalf("machine must be 1..%d, got %q", topo.Size(), args[2])
	}
	return m
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
	total := totalWeight(intents)
	for i, in := range intents {
		state := "up"
		if in.Cordoned {
			state = "cordoned"
		}
		fmt.Printf("  %s: key=%d weight=%-10d %-9s %s\n",
			topo.MachineName(i), topo.KeyOf(i), in.Weight, roleLabel(topo, i, in, total), state)
	}
}

// reconcile converges process reality to intents in three passes: ensure-
// provisioned, stop-swap, start. The weight half runs after (reconcileWeights).
func reconcile(cfg *config, intents []MachineIntent, fresh bool) {
	topo := cfg.topo
	hosts := cfg.nodeIPs

	// Pass 0 — ensure provisioned. Work per PHYSICAL box, not per logical slot: the
	// binary/plugin/keys are pushed once per box even when it hosts several co-located
	// instances. In fresh mode every instance is killed+wiped BEFORE any upload, so a
	// re-upload never hits ETXTBSY against a still-running co-located plugin.
	fmt.Println("[0/3] ensure provisioned...")
	if fresh {
		for i := range hosts {
			fmt.Printf("  %s (%s): fresh clean\n", topo.MachineName(i), hosts[i])
			cfg.freshClean(i)
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
			if checked[host] {
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

	fmt.Println("\nProcesses reconciled. Watch live node state in another window with:")
	fmt.Println("  watch -n 2 ./scripts/failover/status.sh")
}

// status runs ONLY the read-only health snapshot against the current intentions —
// no provisioning, no stop/start, no weight changes. Lets a client see real state.
func status(cfg *config) {
	intents, err := loadIntents(cfg.stateFile, cfg.topo)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Println("== reconcile status ==")
	printIntents(cfg.topo, intents)
	fmt.Println("health (read-only snapshot)...")
	results := cfg.checkHealth(intents)
	reportHealth(cfg, intents, results)
	weightsReport(cfg, intents)
}
