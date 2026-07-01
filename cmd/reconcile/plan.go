package main

import (
	"sort"
	"strconv"
)

// Committed staking-key layout (staking/l1/<idx>). Indices 1..5 are the P-chain
// primaries (not pool machines); the L1 pool keys begin at l1KeyBase:
//
//   - the first NVal keys (l1KeyBase .. l1KeyBase+NVal-1) are the REGISTERED
//     validator set — the only staked identities. They are registered on the
//     P-chain at create-l1, are permanent + IP-agnostic, and MOVE between sites
//     on an explicit site-failover (a single-machine cordon never promotes a
//     backup-site machine — consensus stays single-site by design);
//   - after them, one unique non-validating "home" identity per pool slot
//     (l1KeyBase+NVal + globalSlotIndex), so every live machine has a distinct
//     NodeID even when it is not currently hosting a validator key. A spare or
//     pinned-RPC machine simply wears its home identity permanently.
//
// The counts (NVal validators, NSpare hot spares, NRPC pinned dedicated RPCs, per
// site) come from the per-role IP env (see loadPool); the layout, key numbers,
// roles and restore steps are all derived from them. A 2nd+ RPC is optional ingress
// redundancy; one RPC per site is supported now that every role state-syncs.
const l1KeyBase = 6

const (
	siteA = 0
	siteB = 1
)

// Topology describes the machine pool: one site, or two (primary A + backup B).
// Each site has the same fixed-by-config shape: NVal validators, NSpare hot
// spares, NRPC pinned dedicated RPCs, laid out per site as
// [v0..v(NVal-1), spare0..., rpc0...].
type Topology struct {
	TwoSite bool
	NVal    int // validators per site (>=3)
	NSpare  int // hot spares per site (>=0)
	NRPC    int // pinned dedicated RPCs per site (>=1)
}

// sitePool is the number of slots in one site.
func (t Topology) sitePool() int { return t.NVal + t.NSpare + t.NRPC }

func (t Topology) Size() int {
	if t.TwoSite {
		return 2 * t.sitePool()
	}
	return t.sitePool()
}

// Site reports which site machine i (0-based) belongs to.
func (t Topology) Site(i int) int {
	if t.TwoSite && i >= t.sitePool() {
		return siteB
	}
	return siteA
}

// slotInSite is machine i's 0-based position within its own site.
func (t Topology) slotInSite(i int) int { return i % t.sitePool() }

// role predicates by slot position within a site: [validators | spares | rpcs].
func (t Topology) isValidatorSlot(i int) bool { return t.slotInSite(i) < t.NVal }
func (t Topology) isSpareSlot(i int) bool {
	s := t.slotInSite(i)
	return s >= t.NVal && s < t.NVal+t.NSpare
}
func (t Topology) isRPCSlot(i int) bool { return t.slotInSite(i) >= t.NVal+t.NSpare }

// MachineName renders the operator-facing name: m1.. (site A), b1.. (site B).
func (t Topology) MachineName(i int) string {
	if t.Site(i) == siteB {
		return "b" + strconv.Itoa(i-t.sitePool()+1)
	}
	return "m" + strconv.Itoa(i+1)
}

// homeBase is the first committed key index used for per-slot home identities
// (right after the NVal registered validator keys).
func (t Topology) homeBase() int { return l1KeyBase + t.NVal }

// HomeKey is the unique non-validating identity machine i wears when cordoned,
// free, or (for a spare/RPC) permanently. One per slot, so several live
// non-validating trackers never share a NodeID.
func (t Topology) HomeKey(i int) int { return t.homeBase() + i }

// validatorKeys are the NVal registered validator identities (l1KeyBase..).
func (t Topology) validatorKeys() []int {
	keys := make([]int, t.NVal)
	for i := range keys {
		keys[i] = l1KeyBase + i
	}
	return keys
}

// AllKeys lists every committed key index a pool machine must be provisioned
// with: the NVal validator keys plus one home identity per slot.
func (t Topology) AllKeys() []int {
	keys := make([]int, 0, t.NVal+t.Size())
	for _, k := range t.validatorKeys() {
		keys = append(keys, k)
	}
	for i := 0; i < t.Size(); i++ {
		keys = append(keys, t.HomeKey(i))
	}
	return keys
}

// SiteFromName parses an operator site argument ("a" or "b").
func (t Topology) SiteFromName(s string) (int, bool) {
	switch s {
	case "a", "A":
		return siteA, true
	case "b", "B":
		if !t.TwoSite {
			return 0, false
		}
		return siteB, true
	}
	return 0, false
}

// isValidatorKey reports whether k is one of the NVal registered validator keys.
func (t Topology) isValidatorKey(k int) bool { return k >= l1KeyBase && k < l1KeyBase+t.NVal }

// isRPCKey reports whether committed key k is the home identity of a pinned-RPC
// slot. An RPC machine is sticky on this key and is never a promotion target.
func (t Topology) isRPCKey(k int) bool {
	slot := k - t.homeBase()
	return slot >= 0 && slot < t.Size() && t.isRPCSlot(slot)
}

// MachineIntent is the persisted desired state for one pool machine: whether the
// operator has cordoned it, and which staking identity it should host.
type MachineIntent struct {
	Cordoned bool `json:"cordoned"`
	Key      int  `json:"key"`
}

// seedIntents is the default mapping a fresh deploy resets to: site A's validator
// slots host the NVal registered validator keys (l1KeyBase..); every other slot
// (site A spares/RPCs, and all of site B in two-site mode) wears its own home
// identity as an uncordoned non-validating tracker. All uncordoned.
func seedIntents(topo Topology) []MachineIntent {
	intents := make([]MachineIntent, topo.Size())
	for i := range intents {
		key := topo.HomeKey(i)
		if topo.Site(i) == siteA && topo.isValidatorSlot(i) {
			key = l1KeyBase + topo.slotInSite(i) // active validator key on the primary site
		}
		intents[i] = MachineIntent{Cordoned: false, Key: key}
	}
	return intents
}

// ComputeMapping recomputes the sticky key assignment after a cordon change.
// It is pure: given the (already-toggled) cordon flags and the previous key per
// machine, it returns the new key per machine.
//
// Policy (move only what must move):
//   - A pinned RPC machine (wears key 10 or 18) keeps it forever — never
//     validates, never a promotion target. Checked first so the pin is sticky
//     even across a cordon/uncordon of that machine.
//   - A cordoned machine gives up its key and wears its home identity.
//   - An uncordoned machine holding a validator key keeps it (sticky).
//   - A validator key left uncovered (because its holder was cordoned, or it was
//     uncovered already) is "orphaned" and reassigned to a free uncordoned
//     machine — but ONLY within preferredSite, lowest machine number first.
//     Orphans never cross sites implicitly; only an explicit site-failover
//     (which cordons one whole site and prefers the other) moves the set.
//   - With no free machine in preferredSite, the orphaned key stays uncovered
//     (quorum may drop).
func ComputeMapping(topo Topology, cordoned []bool, prevKey []int, preferredSite int) []int {
	n := len(cordoned)
	newKey := make([]int, n)
	covered := map[int]bool{}
	var free []int // 0-based machine indices in preferredSite wearing a home key, uncordoned

	for i := 0; i < n; i++ {
		switch {
		case topo.isRPCKey(prevKey[i]):
			newKey[i] = prevKey[i] // pinned RPC: sticky identity, never promoted
		case cordoned[i]:
			newKey[i] = topo.HomeKey(i)
		case topo.isValidatorKey(prevKey[i]):
			newKey[i] = prevKey[i] // sticky: keep our live validator key
			covered[prevKey[i]] = true
		default: // uncordoned, wears a home key -> candidate for an orphan
			newKey[i] = topo.HomeKey(i)
			if topo.Site(i) == preferredSite {
				free = append(free, i)
			}
		}
	}

	var orphaned []int
	for _, k := range topo.validatorKeys() {
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
// machines under the given intents (the "N/NVal" the operator cares about).
func LiveValidators(topo Topology, intents []MachineIntent) int {
	seen := map[int]bool{}
	for _, in := range intents {
		if !in.Cordoned && topo.isValidatorKey(in.Key) {
			seen[in.Key] = true
		}
	}
	return len(seen)
}
