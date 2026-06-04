// Command reconcile converges a fixed pool of 5 machines (3 validators + 1 hot
// spare + 1 pinned dedicated-RPC node) to the desired failover state recorded in
// the intentions JSON. It is the single engine behind 03
// (fresh deploy), and the up/down/failover wrappers — see
// docs/failover-recovery-simulation.md.
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
  fresh        clear cordons, wipe data, reseed mapping, force re-upload, start all
  down <m>     cordon machine m, recompute mapping, reconcile
  up <m>       uncordon machine m, recompute mapping, reconcile
  apply        pure reconcile against the existing intentions (no intent change)
  status       read-only health report (actual node state, no changes)`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cfg := loadConfig()

	if os.Args[1] == "status" {
		status(cfg)
		return
	}

	fresh := false
	var intents []MachineIntent

	switch os.Args[1] {
	case "fresh":
		fresh = true
		intents = seedIntents()
		if err := saveIntents(cfg.stateFile, intents); err != nil {
			fatalf("%v", err)
		}
		fmt.Println("== reconcile fresh: reseeded intentions to {m1:6, m2:7, m3:8, m4:9(spare), m5:10(rpc)} ==")

	case "down", "up":
		m := parseMachine(os.Args)
		prev, err := loadIntents(cfg.stateFile)
		if err != nil {
			fatalf("%v", err)
		}
		intents, err = retarget(prev, m, os.Args[1] == "down")
		if err != nil {
			fatalf("%v", err)
		}
		if err := saveIntents(cfg.stateFile, intents); err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("== reconcile %s %d ==\n", os.Args[1], m)

	case "apply":
		var err error
		intents, err = loadIntents(cfg.stateFile)
		if err != nil {
			fatalf("%v", err)
		}
		fmt.Println("== reconcile apply ==")

	default:
		usage()
	}

	printIntents(intents)
	reconcile(cfg, intents, fresh)
}

func parseMachine(args []string) int {
	if len(args) < 3 {
		usage()
	}
	m, err := strconv.Atoi(args[2])
	if err != nil || m < 1 || m > poolSize {
		fatalf("machine must be 1..%d, got %q", poolSize, args[2])
	}
	return m
}

func printIntents(intents []MachineIntent) {
	for i, in := range intents {
		state := "up"
		if in.Cordoned {
			state = "cordoned"
		}
		fmt.Printf("  m%d: key=%d %-9s %s\n", i+1, in.Key, roleLabel(in.Key), state)
	}
}

// reconcile converges reality to intents in three passes: ensure-provisioned,
// stop-swap, start. See docs/failover-recovery-simulation.md.
func reconcile(cfg *config, intents []MachineIntent, fresh bool) {
	hosts := cfg.nodeIPs

	// Pass 0 — ensure provisioned.
	fmt.Println("[0/3] ensure provisioned...")
	for i, host := range hosts {
		if fresh {
			fmt.Printf("  m%d (%s): fresh clean + upload\n", i+1, host)
			cfg.freshClean(host)
			cfg.upload(host)
		} else if !cfg.provisioned(host) {
			fmt.Printf("  m%d (%s): missing artifacts, uploading\n", i+1, host)
			cfg.upload(host)
		} else {
			fmt.Printf("  m%d (%s): provisioned\n", i+1, host)
		}
	}

	// Observe reality, then plan.
	fmt.Println("observe...")
	obs := make([]Observed, len(hosts))
	for i, host := range hosts {
		obs[i] = cfg.observe(host)
		fmt.Printf("  m%d: alive=%v key=%d\n", i+1, obs[i].Alive, obs[i].ActualKey)
	}
	actions := Plan(intents, obs)

	// Pass 1 — stop-swap (every stop+swap before any start).
	fmt.Println("[1/3] stop-swap...")
	for _, a := range actions {
		host := hosts[a.Machine-1]
		if a.Stop {
			fmt.Printf("  m%d: stop\n", a.Machine)
			cfg.stop(host)
		}
		if a.SwapKey != 0 {
			fmt.Printf("  m%d: swap active key -> %d\n", a.Machine, a.SwapKey)
			cfg.swap(host, a.SwapKey)
		}
	}

	// Pass 2 — start.
	fmt.Println("[2/3] start...")
	for _, a := range actions {
		if a.Start {
			fmt.Printf("  m%d: start\n", a.Machine)
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
	intents, err := loadIntents(cfg.stateFile)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Println("== reconcile status ==")
	printIntents(intents)
	fmt.Println("health (read-only snapshot)...")
	results := cfg.checkHealth(intents)
	reportHealth(cfg, intents, results)
}
