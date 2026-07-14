package main

import "fmt"

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
// Unregistered rpc nodes carry weight 0; a mid-seesaw value shows raw.
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
// haveActual=false means the P-chain was unreadable. role=rpc nodes are never
// registered, so they have no on-chain weight to show.
func stakeCell(actual uint64, haveActual, isValidator bool) string {
	if !isValidator {
		return "rpc"
	}
	if !haveActual {
		return "?"
	}
	return weightRole(actual)
}
