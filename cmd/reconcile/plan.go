package main

import (
	"sort"
	"strconv"
)

// Key identities in play. The 3 validator keys are registered on the P-chain at
// create-l1 and are permanent + IP-agnostic; key 9 is the fixed non-validating
// ("nv") key that the site-A spare and any cordoned machine wear in single-site
// mode. Key 10 is the pinned dedicated-RPC identity worn by m5 — a
// non-validating tracker that bombard targets; it is never promoted to a
// validator, so ingress survives failover.
//
// Two-site mode (BACKUP_SITE_NODE_IPS set) adds a backup data center of 6
// machines (b1-b6) that run as live zero-weight syncing trackers, plus unique
// "home" identities so every live machine has a distinct NodeID. Each site is
// 3 validators + 1 spare + TWO archive RPCs (the second RPC is the redundancy
// that lets restore snapshot one RPC while its twin keeps serving ingress):
//
//	m1-m3 park on 11-13 when displaced, m4 spare=9, m5 rpc=10, m6 rpc=19 (pinned)
//	b1-b3 sync on 14-16, b4 spare=17,               b5 rpc=18, b6 rpc=20 (pinned)
//
// Validator keys (6-8) only cross sites via an explicit site-failover — a
// single-machine cordon never promotes a backup-site machine (consensus stays
// single-site by design).
const (
	valKeyLo = 6
	valKeyHi = 8
	nvKey    = 9  // non-validating home (m4 spare; shared by free machines in single-site mode)
	rpcKey   = 10 // site-A pinned RPC #1 (m5)
	rpcKey2  = 19 // site-A pinned RPC #2 (m6)

	rpcKeyB  = 18 // site-B pinned RPC #1 (b5)
	rpcKeyB2 = 20 // site-B pinned RPC #2 (b6)

	sitePoolSize = 6 // per site: 3 validators + 1 spare + 2 archive RPCs
)

const (
	siteA = 0
	siteB = 1
)

// Topology describes the machine pool: one site of 6 (legacy single-site mode)
// or two sites of 6 (primary A + backup B).
type Topology struct {
	TwoSite bool
}

func (t Topology) Size() int {
	if t.TwoSite {
		return 2 * sitePoolSize
	}
	return sitePoolSize
}

// Site reports which site machine i (0-based) belongs to.
func (t Topology) Site(i int) int {
	if i >= sitePoolSize {
		return siteB
	}
	return siteA
}

// MachineName renders the operator-facing name: m1-m6 (site A), b1-b6 (site B).
func (t Topology) MachineName(i int) string {
	if t.Site(i) == siteB {
		return "b" + strconv.Itoa(i-sitePoolSize+1)
	}
	return "m" + strconv.Itoa(i+1)
}

// twoSiteHomes maps machine index -> the non-validating identity it wears when
// not hosting a validator key. Unique per machine: a backup site means several
// live non-validating trackers at once, which can't share a NodeID.
var twoSiteHomes = []int{11, 12, 13, nvKey, rpcKey, rpcKey2, 14, 15, 16, 17, rpcKeyB, rpcKeyB2}

// HomeKey is the identity machine i falls back to when cordoned or free. In
// single-site mode this is the shared nv key (at most one free machine is ever
// live, so sharing is safe — preserved from the validated single-site runs).
func (t Topology) HomeKey(i int) int {
	if t.TwoSite {
		return twoSiteHomes[i]
	}
	return nvKey
}

// AllKeys lists every committed key index a pool machine must be provisioned with.
func (t Topology) AllKeys() []int {
	if t.TwoSite {
		return []int{6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	}
	return []int{6, 7, 8, 9, 10, 19}
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

// isRPCKey reports whether k is a pinned dedicated-RPC identity. An rpc machine
// is sticky on this key and is never a promotion target (never joins `free`).
func isRPCKey(k int) bool { return k == rpcKey || k == rpcKeyB || k == rpcKey2 || k == rpcKeyB2 }

func validatorKeys() []int { return []int{6, 7, 8} }

func isValidatorKey(k int) bool { return k >= valKeyLo && k <= valKeyHi }

// MachineIntent is the persisted desired state for one pool machine: whether the
// operator has cordoned it, and which staking identity it should host.
type MachineIntent struct {
	Cordoned bool `json:"cordoned"`
	Key      int  `json:"key"`
}

// seedIntents is the default mapping a fresh deploy resets to:
// m1=v1(6), m2=v2(7), m3=v3(8), m4=spare(9), m5=rpc(10), m6=rpc(19), all uncordoned —
// plus, in two-site mode, b1-b3 syncing on 14-16, b4=spare(17), b5=rpc(18), b6=rpc(20).
func seedIntents(topo Topology) []MachineIntent {
	intents := []MachineIntent{
		{Cordoned: false, Key: 6},
		{Cordoned: false, Key: 7},
		{Cordoned: false, Key: 8},
		{Cordoned: false, Key: 9},
		{Cordoned: false, Key: 10},
		{Cordoned: false, Key: rpcKey2},
	}
	if topo.TwoSite {
		intents = append(intents,
			MachineIntent{Cordoned: false, Key: 14},
			MachineIntent{Cordoned: false, Key: 15},
			MachineIntent{Cordoned: false, Key: 16},
			MachineIntent{Cordoned: false, Key: 17},
			MachineIntent{Cordoned: false, Key: rpcKeyB},
			MachineIntent{Cordoned: false, Key: rpcKeyB2},
		)
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
		case isRPCKey(prevKey[i]):
			newKey[i] = prevKey[i] // pinned RPC: sticky identity, never promoted
		case cordoned[i]:
			newKey[i] = topo.HomeKey(i)
		case isValidatorKey(prevKey[i]):
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
