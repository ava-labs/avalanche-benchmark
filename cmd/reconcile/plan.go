package main

import "sort"

// Key identities in play. The 3 validator keys are registered on the P-chain at
// create-l1 and are permanent + IP-agnostic; key 9 is the fixed non-validating
// ("nv") key that the spare and any cordoned machine wear.
const (
	valKeyLo = 6
	valKeyHi = 8
	nvKey    = 9
	poolSize = 4
)

func validatorKeys() []int { return []int{6, 7, 8} }

func isValidatorKey(k int) bool { return k >= valKeyLo && k <= valKeyHi }

// MachineIntent is the persisted desired state for one pool machine: whether the
// operator has cordoned it, and which staking identity it should host.
type MachineIntent struct {
	Cordoned bool `json:"cordoned"`
	Key      int  `json:"key"`
}

// seedIntents is the default mapping a fresh deploy resets to:
// m1=v1(6), m2=v2(7), m3=v3(8), m4=spare(9), all uncordoned.
func seedIntents() []MachineIntent {
	return []MachineIntent{
		{Cordoned: false, Key: 6},
		{Cordoned: false, Key: 7},
		{Cordoned: false, Key: 8},
		{Cordoned: false, Key: 9},
	}
}

// ComputeMapping recomputes the sticky key assignment after a cordon toggle.
// It is pure: given the (already-toggled) cordon flags and the previous key per
// machine, it returns the new key per machine.
//
// Policy (move only what must move):
//   - A cordoned machine gives up its key and wears nv.
//   - An uncordoned machine holding a validator key keeps it (sticky).
//   - A validator key left uncovered (because its holder was cordoned, or it was
//     uncovered already) is "orphaned" and reassigned to a free uncordoned
//     machine (one currently wearing nv), lowest machine number first.
//   - With no free machine, the orphaned key stays uncovered (quorum may drop).
func ComputeMapping(cordoned []bool, prevKey []int) []int {
	n := len(cordoned)
	newKey := make([]int, n)
	covered := map[int]bool{}
	var free []int // 0-based machine indices wearing nv and uncordoned

	for i := 0; i < n; i++ {
		switch {
		case cordoned[i]:
			newKey[i] = nvKey
		case isValidatorKey(prevKey[i]):
			newKey[i] = prevKey[i] // sticky: keep our live validator key
			covered[prevKey[i]] = true
		default: // uncordoned, holds nv -> free spare, candidate for an orphan
			newKey[i] = nvKey
			free = append(free, i)
		}
	}

	var orphaned []int
	for _, k := range validatorKeys() {
		if !covered[k] {
			orphaned = append(orphaned, k)
		}
	}
	sort.Ints(orphaned) // free is already ascending by machine index

	for j := 0; j < len(orphaned) && j < len(free); j++ {
		newKey[free[j]] = orphaned[j]
	}
	return newKey
}

// Observed is reconcile's fresh read of one machine's reality.
type Observed struct {
	Alive     bool // pgrep -x avalanchego
	ActualKey int  // staking/active/key_index, 0 if missing/unknown
}

// Action is what reconcile will do to one machine. Execution order across all
// machines is: every Stop+SwapKey first (pass 1), then every Start (pass 2), so
// no permutation ever has two live machines holding the same validator key.
type Action struct {
	Machine int
	Stop    bool // pkill the running process
	SwapKey int  // 0 = no swap; else wipe staking/active and copy this committed key
	Start   bool // launch avalanchego pointing at staking/active
}

// Plan computes per-machine actions from intent + observation. Pure function.
func Plan(intents []MachineIntent, obs []Observed) []Action {
	acts := make([]Action, len(intents))
	for i := range intents {
		in := intents[i]
		ob := obs[i]

		needSwap := ob.ActualKey != in.Key
		// Stop a live machine that must end down (cordoned) or be re-keyed; you
		// never write a key to a live process, so a swap implies a prior stop.
		stop := ob.Alive && (in.Cordoned || needSwap)

		swap := 0
		if needSwap {
			swap = in.Key
		}

		// After pass 1 the machine is dead if it was already dead or we stopped
		// it. An uncordoned dead machine must come back up.
		willBeDead := !ob.Alive || stop
		start := !in.Cordoned && willBeDead

		acts[i] = Action{Machine: i + 1, Stop: stop, SwapKey: swap, Start: start}
	}
	return acts
}

// LiveValidators counts the distinct validator identities hosted by uncordoned
// machines under the given intents (the "N/3" the operator cares about).
func LiveValidators(intents []MachineIntent) int {
	seen := map[int]bool{}
	for _, in := range intents {
		if !in.Cordoned && isValidatorKey(in.Key) {
			seen[in.Key] = true
		}
	}
	return len(seen)
}
