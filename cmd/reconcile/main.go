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
  site-failover <a|b>  fail the validator set over to the given site (hard cutover, two-site mode)
  restore <a|b>        graceful rolling migration of the validator set to a site — one
                       validator at a time, chain stays >=2/3 throughout, no fork (two-site
                       mode); seeds targets from a live DB snapshot by default
                       (RESTORE_MODE=state-sync forces from-scratch). Typically used to
                       restore the original site after a site-failover
  apply              pure reconcile against the existing intentions (no intent change)
  status             read-only health report (actual node state, no changes)
  verify             read-only proof the live network is ONE branch (no fork) + quorum healthy`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cfg := loadConfig()
	topo := cfg.topo

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
		if topo.TwoSite {
			fmt.Println("== reconcile fresh: reseeded intentions to {m1:6, m2:7, m3:8, m4:9(spare), m5:10(rpc), b1-b4:14-17(sync), b5:18(rpc)} ==")
		} else {
			fmt.Println("== reconcile fresh: reseeded intentions to {m1:6, m2:7, m3:8, m4:9(spare), m5:10(rpc)} ==")
		}

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

func printIntents(topo Topology, intents []MachineIntent) {
	for i, in := range intents {
		state := "up"
		if in.Cordoned {
			state = "cordoned"
		}
		fmt.Printf("  %s: key=%d %-9s %s\n", topo.MachineName(i), in.Key, roleLabel(in.Key), state)
	}
}

// reconcile converges reality to intents in three passes: ensure-provisioned,
// stop-swap, start. See docs/failover-recovery-simulation.md.
func reconcile(cfg *config, intents []MachineIntent, fresh bool) {
	topo := cfg.topo
	hosts := cfg.nodeIPs

	// Pass 0 — ensure provisioned.
	fmt.Println("[0/3] ensure provisioned...")
	for i, host := range hosts {
		if fresh {
			fmt.Printf("  %s (%s): fresh clean + upload\n", topo.MachineName(i), host)
			cfg.freshClean(host)
			cfg.upload(host)
		} else if !cfg.provisioned(host) {
			fmt.Printf("  %s (%s): missing artifacts, uploading\n", topo.MachineName(i), host)
			cfg.upload(host)
		} else {
			fmt.Printf("  %s (%s): provisioned\n", topo.MachineName(i), host)
		}
	}

	// Observe reality, then plan.
	fmt.Println("observe...")
	obs := make([]Observed, len(hosts))
	for i, host := range hosts {
		obs[i] = cfg.observe(host)
		fmt.Printf("  %s: alive=%v key=%d\n", topo.MachineName(i), obs[i].Alive, obs[i].ActualKey)
	}
	actions := Plan(intents, obs)

	// Pass 1 — stop-swap (every stop+swap before any start).
	fmt.Println("[1/3] stop-swap...")
	for _, a := range actions {
		host := hosts[a.Machine-1]
		if a.Stop {
			fmt.Printf("  %s: stop\n", topo.MachineName(a.Machine-1))
			cfg.stop(host)
		}
		if a.SwapKey != 0 {
			fmt.Printf("  %s: swap active key -> %d\n", topo.MachineName(a.Machine-1), a.SwapKey)
			cfg.swap(host, a.SwapKey)
		}
	}

	// Pass 2 — start.
	fmt.Println("[2/3] start...")
	for _, a := range actions {
		if a.Start {
			fmt.Printf("  %s: start\n", topo.MachineName(a.Machine-1))
			cfg.start(hosts[a.Machine-1], hosts[a.Machine-1])
		}
	}

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
