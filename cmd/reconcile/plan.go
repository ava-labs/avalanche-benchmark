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
