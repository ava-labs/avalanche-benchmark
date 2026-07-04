// Command reconcile converges a fixed pool of machines — 5 in single-site mode
// (3 validators + 1 hot spare + 1 pinned dedicated-RPC node), or 5+5 in
// two-site mode (a backup data center of zero-weight syncing trackers + its own
// pinned RPC) — to the desired failover state recorded in the intentions JSON.
// It is the single engine behind 03 (fresh deploy), and the up/down/failover
// wrappers — see docs/failover-recovery-simulation.md and
// docs/two-site-failover.md.
//
// The decision logic is a pure function (plan.go); this file does the SSH/scp
// I/O. Run it via the bash wrappers, which supply config through the environment.
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
  fresh              clear cordons, wipe data, reseed mapping, force re-upload, start all
  down <m>           cordon machine m, recompute mapping, reconcile
  up <m>             uncordon machine m, recompute mapping, reconcile
  clean <m>          wipe machine m's chain data (keep credentials), then reconcile it back up
  site-failover <a|b>  fail the validator set over to the given site (hard cutover, two-site mode)
  restore <a|b>        graceful rolling migration of the validator set to a site — one
                       validator at a time, chain stays >=2/3 throughout, no fork (two-site
                       mode); seeds targets from a live DB snapshot by default
                       (RESTORE_MODE=state-sync forces from-scratch; RESTORE_SKIP_SEED=1
                       skips the snapshot+wipe to resume an already-synced target).
                       Typically used to restore the original site after a site-failover
  apply              pure reconcile against the existing intentions (no intent change)
  status             read-only health report (actual node state, no changes)
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
	// runs before the full loadConfig — 04/05 call it with only NODE_IPS exported.
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

	if os.Args[1] == "restore" {
		if len(os.Args) < 3 {
			usage()
		}
		target, ok := topo.SiteFromName(os.Args[2])
		if !ok {
			fatalf("site must be 'a' or 'b' (two-site mode requires BACKUP_SITE_NODE_IPS), got %q", os.Args[2])
		}
		rollingRestore(cfg, target)
		return
	}

	fresh := false
	var intents []MachineIntent

	switch os.Args[1] {
	case "fresh":
		fresh = true
		intents = seedIntents(topo)
		if err := saveIntents(cfg.stateFile, intents); err != nil {
			fatalf("%v", err)
		}
		// The actual mapping (validator keys on site A's validator slots, pinned
		// home identities everywhere else) is printed by printIntents below.
		fmt.Println("== reconcile fresh: reseeded intentions to the default mapping ==")

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
			fatalf("site must be 'a' or 'b' (two-site mode requires BACKUP_SITE_NODE_IPS), got %q", os.Args[2])
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
		// Model a real disaster: NUKE the down site first — hard-kill every node on it
		// at once — so its tip is frozen at the instant of failure before anything else
		// happens. The surviving site is a live tracker; it now reconciles ITSELF on
		// whatever blocks it already holds (no data is ever pulled from the dead site),
		// which both fixes the true RPO boundary and lets waitForSiteSettled converge on a
		// static tip instead of chasing the old site's still-advancing one.
		downSite := otherSite(target)
		fmt.Printf("== site-failover: nuking site %s (parallel SIGKILL — simulated outage) ==\n", siteName(downSite))
		cfg.nukeSite(downSite)
		// Make the surviving site internally consistent (promote validators already at the
		// tip as-is; resync any laggard from the site's OWN archive RPC) so a quorum forms
		// without the inconsistent-height deadlock. No-op when the site is a synced hot standby.
		cfg.reconcileBackupHeights(intents, target)

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
		fmt.Printf("%s\t%s\t%s\t%s\t%d\n", topo.MachineName(i), site, topo.slotRoleName(i), in.host, in.httpPort)
	}
}

func printIntents(topo Topology, intents []MachineIntent) {
	for i, in := range intents {
		state := "up"
		if in.Cordoned {
			state = "cordoned"
		}
		fmt.Printf("  %s: key=%d %-9s %s\n", topo.MachineName(i), in.Key, topo.roleLabel(in.Key), state)
	}
}

// reconcile converges reality to intents in three passes: ensure-provisioned,
// stop-swap, start. See docs/failover-recovery-simulation.md.
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
	actions := Plan(intents, obs)

	// Pass 1 — stop-swap (every stop+swap before any start).
	fmt.Println("[1/3] stop-swap...")
	for _, a := range actions {
		if a.Stop {
			fmt.Printf("  %s: stop\n", topo.MachineName(a.Machine-1))
			cfg.stop(a.Machine - 1)
		}
		if a.SwapKey != 0 {
			fmt.Printf("  %s: swap active key -> %d\n", topo.MachineName(a.Machine-1), a.SwapKey)
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

	// Publish the active site's RPCs so bombard routes ingress to the data center
	// that now holds the validators (and switches as failover/restore moves them).
	cfg.writeActiveRPCs(intents)

	// No blocking health wait — watch live state with status.sh in another window.
	fmt.Println("\nApplied. Watch live node state in another window with:")
	fmt.Println("  watch -n 2 ./scripts/failover/status.sh")
}

// status runs ONLY the read-only health snapshot against the current intentions —
// no provisioning, no stop/swap/start. Lets a client see real state without SSH.
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
}
