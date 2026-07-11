package main

import (
	"fmt"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

// The pool layout, per-slot permanent keys and staking-slot order live in
// internal/topo (shared with create-l1 and fuji-wallet so the
// slot -> key -> validationID mapping can never drift). Identities never move
// between machines: failover is a weight change on the ValidatorManager
// contract, not a key swap.
type Topology = topo.Topology

const (
	siteA = topo.SiteA
	siteB = topo.SiteB
)

// MachineIntent is the persisted desired state for one pool slot: whether the
// operator has cordoned the machine (process down), and the desired consensus
// weight of its permanent identity. Weight is 0 for RPC slots (never
// registered) and >=1 for staking slots (a registered L1 validator's weight
// can never be set to 0: weight 0 means removal, and we never remove).
type MachineIntent struct {
	Cordoned bool   `json:"cordoned"`
	Weight   uint64 `json:"weight"`
}

// seedIntents is the state a fresh deploy resets to, matching the conversion
// weights create-l1 registered: every site A staking slot (validators and
// spare) carries the consensus at ValidatorWeight; site B staking slots idle
// at SpareWeight; RPC slots are unregistered. All uncordoned.
func seedIntents(t Topology) []MachineIntent {
	intents := make([]MachineIntent, t.Size())
	for i := range intents {
		intents[i] = MachineIntent{Cordoned: false, Weight: seedWeight(t, i)}
	}
	return intents
}

func seedWeight(t Topology, i int) uint64 {
	switch {
	case !t.IsStakingSlot(i):
		return 0
	case t.Site(i) == siteA:
		return valmgr.ValidatorWeight
	default:
		return valmgr.SpareWeight
	}
}

// isActiveWeight reports whether a desired weight makes the slot an acting
// validator: at least 1% of the fleet's total desired weight. The validator
// tier (1000x a spare) clears it; spare and dead tiers do not.
func isActiveWeight(w, total uint64) bool {
	return total > 0 && w*100 >= total
}

// weightRole names the tier a desired weight sits in, for status display.
// Unregistered RPC slots carry weight 0; a mid-ratchet on-chain value shows raw.
func weightRole(w uint64) string {
	switch w {
	case 0:
		return "rpc"
	case valmgr.ValidatorWeight:
		return "validator"
	case valmgr.SpareWeight:
		return "spare"
	case valmgr.DeadWeight:
		return "dead"
	default:
		return fmt.Sprintf("w=%d", w)
	}
}

// stakeCell renders the status stake column: the ACTUAL contract tier first,
// with a pending marker when the desired tier differs (a weight change still
// in flight). haveActual=false means the ValidatorManager contract was
// unreadable: fall back to the desired tier. RPC slots (weight 0) are never
// registered, so they have no on-chain weight to show.
func stakeCell(desired, actual uint64, haveActual bool) string {
	if desired == 0 {
		return "rpc"
	}
	if !haveActual || actual == desired {
		return weightRole(desired)
	}
	return weightRole(actual) + " -> " + weightRole(desired) + " pending"
}

func totalWeight(intents []MachineIntent) uint64 {
	var sum uint64
	for _, in := range intents {
		sum += in.Weight
	}
	return sum
}

// Observed is reconcile's fresh read of one machine's reality.
type Observed struct {
	Alive     bool // pgrep avalanchego (instance-scoped when co-located)
	ActualKey int  // staking/active/key_index, 0 if missing/unknown
}

// Action is what reconcile will do to one machine. Execution order across all
// machines is: every Stop+SwapKey first (pass 1), then every Start (pass 2).
// SwapKey now only ever installs the slot's PERMANENT key (first provision or
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
