package main

import (
	"testing"

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

// TestSeesawConverges simulates the contract's optimistic churn tracker
// (period 0: every op re-seeds the budget from the running total) through a
// full DC failover and asserts the ratchet terminates quickly with every step
// within the per-op cap.
func TestSeesawConverges(t *testing.T) {
	const A = valmgr.ValidatorWeight
	// Post-conversion two-site fleet: site A active, site B + spares standby.
	weights := []uint64{A, A, A, 1, 1, 1, 1, 1}
	desired := []uint64{1, 1, 1, 1, A, A, A, 1}

	steps := 0
	for ; steps < weightRounds; steps++ {
		var total uint64
		for _, w := range weights {
			total += w
		}
		// Pick exactly like convergeContract: first raise, else first lower.
		pick, isRaise := -1, false
		for i := range weights {
			if weights[i] < desired[i] {
				pick, isRaise = i, true
				break
			}
		}
		if pick < 0 {
			for i := range weights {
				if weights[i] > desired[i] {
					pick = i
					break
				}
			}
		}
		if pick < 0 {
			break
		}
		next := nextWeight(weights[pick], desired[pick], total)
		delta := next - weights[pick]
		if !isRaise {
			delta = weights[pick] - next
		}
		if delta > total/5 {
			t.Fatalf("step %d: delta %d exceeds churn cap %d", steps, delta, total/5)
		}
		weights[pick] = next
	}
	for i := range weights {
		if weights[i] != desired[i] {
			t.Fatalf("did not converge: slot %d weight %d != desired %d", i, weights[i], desired[i])
		}
	}
	if steps > 15 {
		t.Errorf("seesaw took %d steps, expected ~10", steps)
	}
	t.Logf("seesaw converged in %d initiates", steps)
}
