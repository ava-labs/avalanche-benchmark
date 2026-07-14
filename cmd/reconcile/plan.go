package main

import (
	"fmt"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
)

// The pool layout, per-slot permanent keys and staking-slot order live in
// internal/topo (shared with cmd/l1 and fuji-wallet so the slot -> key
// mapping can never drift). Identities never move between machines: failover
// is a weight change on the P-chain (driven by cmd/l1), never a key swap.
type Topology = topo.Topology

const (
	siteA = topo.SiteA
	siteB = topo.SiteB
)

// The three weight tiers a registered validator sits in, DISPLAY-ONLY here:
// on-chain weights live on the P-chain and move only through cmd/l1
// (set-weight / apply). Kept in sync with cmd/l1's create defaults.
const (
	validatorWeight uint64 = 100_000
	spareWeight     uint64 = 1_000
	deadWeight      uint64 = 1
)

// MachineIntent is the persisted desired state for one pool slot: whether the
// operator has cordoned the machine (process down). Weight is NOT fleet state
// anymore: the on-chain weights are authoritative on the P-chain and move
// only through cmd/l1.
type MachineIntent struct {
	Cordoned bool `json:"cordoned"`
}

// seedIntents is the state a fresh deploy resets to: everything uncordoned.
func seedIntents(t Topology) []MachineIntent {
	return make([]MachineIntent, t.Size())
}

// isActiveWeight reports whether an on-chain weight makes the slot an acting
// validator: at least 1% of the fleet's total weight. The validator tier
// (100x a spare) clears it; spare and dead tiers do not.
func isActiveWeight(w, total uint64) bool {
	return total > 0 && w*100 >= total
}

// weightRole names the tier a weight sits in, for status display.
// Unregistered RPC slots carry weight 0; a mid-seesaw value shows raw.
func weightRole(w uint64) string {
	switch w {
	case 0:
		return "rpc"
	case validatorWeight:
		return "validator"
	case spareWeight:
		return "spare"
	case deadWeight:
		return "dead"
	default:
		return fmt.Sprintf("w=%d", w)
	}
}

// stakeCell renders the status stake column from the ACTUAL on-chain weight.
// haveActual=false means the P-chain was unreadable. RPC slots are never
// registered, so they have no on-chain weight to show.
func stakeCell(actual uint64, haveActual, stakingSlot bool) string {
	if !stakingSlot {
		return "rpc"
	}
	if !haveActual {
		return "?"
	}
	return weightRole(actual)
}

// Observed is reconcile's fresh read of one machine's reality.
type Observed struct {
	Alive       bool // pgrep avalanchego (instance-scoped when co-located)
	ActualKey   int  // staking/active/key_index, 0 if missing/unknown
	Unreachable bool // ssh to the host failed: nothing can be observed or executed there
}

// Action is what reconcile will do to one machine. Execution order across all
// machines is: every Stop+SwapKey first (pass 1), then every Start (pass 2).
// SwapKey only ever installs the slot's PERMANENT key (first provision or
// healing a hand-mangled active dir): identities never migrate.
type Action struct {
	Machine int
	Stop    bool // stop the running process
	SwapKey int  // 0 = no change; else install this committed key into staking/active
	Start   bool // launch avalanchego pointing at staking/active
}

// Plan computes per-machine actions from intent + observation. Pure function.
func Plan(t Topology, intents []MachineIntent, obs []Observed) []Action {
	acts := make([]Action, len(intents))
	for i := range intents {
		in := intents[i]
		ob := obs[i]

		// An unreachable host can't run any action; plan nothing for it (it was
		// recorded as down with a warning) so one dead box never aborts the
		// stop/swap/start passes for the rest of the fleet.
		if ob.Unreachable {
			acts[i] = Action{Machine: i + 1}
			continue
		}

		needSwap := ob.ActualKey != t.KeyOf(i)
		// Stop a live machine that must end down (cordoned) or has the wrong
		// identity installed; you never write a key under a live process.
		stop := ob.Alive && (in.Cordoned || needSwap)

		swap := 0
		if needSwap {
			swap = t.KeyOf(i)
		}

		willBeDead := !ob.Alive || stop
		start := !in.Cordoned && willBeDead

		acts[i] = Action{Machine: i + 1, Stop: stop, SwapKey: swap, Start: start}
	}
	return acts
}
