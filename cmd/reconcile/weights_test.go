package main

import (
	"testing"

	"github.com/ava-labs/avalanchego/ids"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

func TestNextWeight(t *testing.T) {
	const A = valmgr.ValidatorWeight
	tests := []struct {
		name                    string
		current, desired, total uint64
		want                    uint64
	}{
		{"already there", A, A, 3 * A, A},
		{"small raise fits", 1, A / 10, 3 * A, A / 10},
		{"big raise clamped to 20% of total", 1, A, 3 * A, 1 + 3*A/5},
		{"big lower clamped", A, 1, 3 * A, A - 3*A/5},
		{"lower fits when budget large", A, 1, 6 * A, 1},
		{"tiny total still moves", 1, 5, 4, 2},
	}
	for _, tt := range tests {
		if got := nextWeight(tt.current, tt.desired, tt.total); got != tt.want {
			t.Errorf("%s: nextWeight(%d,%d,%d) = %d, want %d",
				tt.name, tt.current, tt.desired, tt.total, got, tt.want)
		}
	}
}

// TestPlanSeesaw replays the planned burst against a simulated churn tracker
// (period 0: every op re-seeds the budget from the running total) through a
// full DC failover: every step must fit the per-op cap, raises must all
// precede lowers, and the plan must converge quickly.
func TestPlanSeesaw(t *testing.T) {
	const A = valmgr.ValidatorWeight
	// Post-conversion two-site fleet: site A active, site B + spares standby.
	weights := []uint64{A, A, A, 1, 1, 1, 1, 1}
	desired := []uint64{1, 1, 1, 1, A, A, A, 1}

	targets := make([]stakingTarget, len(weights))
	current := map[ids.ID]uint64{}
	var total uint64
	for i := range weights {
		vid := ids.ID{byte(i + 1)}
		targets[i] = stakingTarget{slot: i, validationID: vid, desired: desired[i]}
		current[vid] = weights[i]
		total += weights[i]
	}

	steps := planSeesaw(targets, current, total)

	// planSeesaw must not mutate the caller's observation map: planning again
	// from the same observation must produce the identical burst.
	if again := planSeesaw(targets, current, total); len(again) != len(steps) {
		t.Fatalf("replan produced %d steps, first plan %d (observation map mutated?)", len(again), len(steps))
	}

	// Replay: the contract accepts a step iff delta <= live total / 5.
	sawLower := false
	for n, s := range steps {
		cur := current[s.ValidationID]
		var delta uint64
		if s.Weight > cur {
			delta = s.Weight - cur
			if sawLower {
				t.Fatalf("step %d: raise after a lower", n)
			}
		} else {
			delta = cur - s.Weight
			sawLower = true
		}
		if delta > total/5 {
			t.Fatalf("step %d: delta %d exceeds churn cap %d", n, delta, total/5)
		}
		total = total - cur + s.Weight
		current[s.ValidationID] = s.Weight
	}
	for _, tg := range targets {
		if current[tg.validationID] != tg.desired {
			t.Fatalf("did not converge: slot %d weight %d != desired %d",
				tg.slot, current[tg.validationID], tg.desired)
		}
	}
	if len(steps) > 15 {
		t.Errorf("seesaw took %d steps, expected ~10", len(steps))
	}
	if again := planSeesaw(targets, current, total); len(again) != 0 {
		t.Errorf("plan on converged state produced %d steps", len(again))
	}
	t.Logf("seesaw planned in %d initiates", len(steps))
}
