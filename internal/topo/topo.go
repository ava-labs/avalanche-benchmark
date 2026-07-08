// Package topo is the single source of truth for the machine-pool layout:
// slot order, per-slot permanent staking key, and which slots are registered
// as L1 validators. cmd/create-l1 (conversion order = validationID index),
// cmd/reconcile (weight reconciliation) and cmd/fuji-wallet (funding math)
// all derive from it, so the slot -> key -> validationID mapping can never
// drift between them.
//
// Staking-key layout (staking/l1/<idx>): pool keys are 1-based, KeyOf(i) =
// KeyBase + i for slot i. Every pool slot wears ONE permanent identity. Identities
// never move between machines (key-swap failover produced forks); instead the
// validator+spare slots of BOTH sites are all registered as L1 validators at
// conversion and failover only changes their weights through the
// ValidatorManager contract on the Fuji C-chain.
package topo

import (
	"fmt"
	"strconv"
	"strings"
)

// KeyBase is the first staking key index; slot i wears key KeyBase+i.
const KeyBase = 1

const (
	SiteA = 0
	SiteB = 1
)

// Topology describes the machine pool: one site, or two (primary A + backup B).
// Each site has the same fixed-by-config shape, laid out per site as
// [v0..v(NVal-1), spare0..., rpc0...].
type Topology struct {
	TwoSite bool
	NVal    int // validators per site (>=3)
	NSpare  int // hot spares per site (>=0)
	NRPC    int // pinned dedicated RPCs per site (>=1)
}

// SitePool is the number of slots in one site.
func (t Topology) SitePool() int { return t.NVal + t.NSpare + t.NRPC }

func (t Topology) Size() int {
	if t.TwoSite {
		return 2 * t.SitePool()
	}
	return t.SitePool()
}

// Site reports which site slot i (0-based) belongs to.
func (t Topology) Site(i int) int {
	if t.TwoSite && i >= t.SitePool() {
		return SiteB
	}
	return SiteA
}

// SlotInSite is slot i's 0-based position within its own site.
func (t Topology) SlotInSite(i int) int { return i % t.SitePool() }

// Role predicates by slot position within a site: [validators | spares | rpcs].
func (t Topology) IsValidatorSlot(i int) bool { return t.SlotInSite(i) < t.NVal }
func (t Topology) IsSpareSlot(i int) bool {
	s := t.SlotInSite(i)
	return s >= t.NVal && s < t.NVal+t.NSpare
}
func (t Topology) IsRPCSlot(i int) bool { return t.SlotInSite(i) >= t.NVal+t.NSpare }

// IsStakingSlot reports whether slot i is registered as an L1 validator at
// conversion (validators and spares of every site; RPC slots never validate).
func (t Topology) IsStakingSlot(i int) bool { return !t.IsRPCSlot(i) }

// MachineName renders the operator-facing name, encoding role and site:
// stake-bearing slots (validators+spares) are a1..aN (site A) / b1..bN
// (site B); RPC slots are rpc_a1.. / rpc_b1... Fleet commands still address
// machines by pool number (1-based slot index), which status prints alongside.
func (t Topology) MachineName(i int) string {
	site := "a"
	if t.Site(i) == SiteB {
		site = "b"
	}
	s := t.SlotInSite(i)
	if s < t.NVal+t.NSpare {
		return site + strconv.Itoa(s+1)
	}
	return "rpc_" + site + strconv.Itoa(s-t.NVal-t.NSpare+1)
}

// KeyOf is slot i's permanent committed staking key index.
func (t Topology) KeyOf(i int) int { return KeyBase + i }

// AllKeys lists every committed key index the pool uses: one per slot.
func (t Topology) AllKeys() []int {
	keys := make([]int, t.Size())
	for i := range keys {
		keys[i] = t.KeyOf(i)
	}
	return keys
}

// StakingSlots lists the registered-validator slots in slot order. A slot's
// position in this list IS its conversion-tx validator index, and therefore
// its validationID = sha256(subnetID || uint32BE(position)). Append-only
// semantics: this order is committed on-chain at conversion and must never
// change for a deployed subnet.
func (t Topology) StakingSlots() []int {
	var slots []int
	for i := 0; i < t.Size(); i++ {
		if t.IsStakingSlot(i) {
			slots = append(slots, i)
		}
	}
	return slots
}

// StakingIndex is slot i's position in StakingSlots (its conversion validator
// index), or -1 for an RPC slot.
func (t Topology) StakingIndex(i int) int {
	if !t.IsStakingSlot(i) {
		return -1
	}
	n := 0
	for j := 0; j < i; j++ {
		if t.IsStakingSlot(j) {
			n++
		}
	}
	return n
}

// SlotRoleName labels a pool slot by its role within its site
// ([v1..vN, spare..., rpc...]). Human-facing only.
func (t Topology) SlotRoleName(i int) string {
	switch s := t.SlotInSite(i); {
	case s < t.NVal:
		return "v" + strconv.Itoa(s+1)
	case s < t.NVal+t.NSpare:
		return "spare"
	default:
		return "rpc"
	}
}

// SiteFromName parses an operator site argument ("a" or "b").
func (t Topology) SiteFromName(s string) (int, bool) {
	switch s {
	case "a", "A":
		return SiteA, true
	case "b", "B":
		if !t.TwoSite {
			return 0, false
		}
		return SiteB, true
	}
	return 0, false
}

// splitIPs parses a comma-separated IP list, trimming blanks and dropping empties.
func splitIPs(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// FromEnv derives the topology and the ordered pool IPs from the per-role IP
// env. Each site is defined by three lists whose LENGTHS set the counts and
// whose VALUES set placement (a repeated IP co-locates another process on that
// box):
//
//	VALIDATOR_IPS / SPARE_IPS / RPC_IPS                      (site A)
//	BACKUP_VALIDATOR_IPS / BACKUP_SPARE_IPS / BACKUP_RPC_IPS (site B, optional)
//
// getenv is os.Getenv in production; injectable for tests.
func FromEnv(getenv func(string) string) (Topology, []string, error) {
	valA := splitIPs(getenv("VALIDATOR_IPS"))
	spareA := splitIPs(getenv("SPARE_IPS"))
	rpcA := splitIPs(getenv("RPC_IPS"))
	if len(valA) == 0 && len(spareA) == 0 && len(rpcA) == 0 {
		return Topology{}, nil, fmt.Errorf("no fleet topology configured (VALIDATOR_IPS etc. are empty); copy .env.example to .env and set the per-role IP lists; if you meant to drive a remote fleet, run this on its control host")
	}
	if len(valA) < 3 {
		return Topology{}, nil, fmt.Errorf("VALIDATOR_IPS has %d IP(s); the topology needs at least 3 validator slots per site (stake tiers are set separately via fleet weight)", len(valA))
	}
	if len(rpcA) < 1 {
		return Topology{}, nil, fmt.Errorf("RPC_IPS has %d IP(s); the topology needs at least 1 RPC slot per site to serve ingress", len(rpcA))
	}

	t := Topology{NVal: len(valA), NSpare: len(spareA), NRPC: len(rpcA)}
	pool := append(append(append([]string{}, valA...), spareA...), rpcA...)

	if getenv("BACKUP_VALIDATOR_IPS") != "" {
		valB := splitIPs(getenv("BACKUP_VALIDATOR_IPS"))
		spareB := splitIPs(getenv("BACKUP_SPARE_IPS"))
		rpcB := splitIPs(getenv("BACKUP_RPC_IPS"))
		if len(valB) != t.NVal || len(spareB) != t.NSpare || len(rpcB) != t.NRPC {
			return Topology{}, nil, fmt.Errorf(
				"backup site shape (%dv/%ds/%dr) must match primary (%dv/%ds/%dr)",
				len(valB), len(spareB), len(rpcB), t.NVal, t.NSpare, t.NRPC)
		}
		t.TwoSite = true
		pool = append(pool, valB...)
		pool = append(pool, spareB...)
		pool = append(pool, rpcB...)
	}
	return t, pool, nil
}
