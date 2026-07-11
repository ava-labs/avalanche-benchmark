package main

import (
	"testing"
	"time"

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

// TestSlotConverged pins THE convergence predicate: contract state only,
// converged iff weight == desired AND sentNonce == receivedNonce (the
// receivedNonce is the courier-delivered ack proving the P-chain applied the
// update; no P-chain read exists anywhere in the weight flow).
func TestSlotConverged(t *testing.T) {
	tests := []struct {
		name    string
		v       valmgr.Validator
		desired uint64
		want    bool
	}{
		{"converged", valmgr.Validator{Weight: 100, SentNonce: 7, ReceivedNonce: 7}, 100, true},
		{"weight pending", valmgr.Validator{Weight: 50, SentNonce: 7, ReceivedNonce: 7}, 100, false},
		{"ack pending: weight matches but courier has not acked", valmgr.Validator{Weight: 100, SentNonce: 7, ReceivedNonce: 6}, 100, false},
		{"both pending", valmgr.Validator{Weight: 50, SentNonce: 7, ReceivedNonce: 6}, 100, false},
		{"fresh slot, no updates ever", valmgr.Validator{Weight: 100}, 100, true},
	}
	for _, tt := range tests {
		if got := slotConverged(tt.v, tt.desired); got != tt.want {
			t.Errorf("%s: slotConverged(%+v, %d) = %v, want %v", tt.name, tt.v, tt.desired, got, tt.want)
		}
	}
}

// TestContractWeights: the projection keeps nil (contract unreadable) intact
// so reportHealth falls back to desired weights, and maps slot -> weight.
func TestContractWeights(t *testing.T) {
	if contractWeights(nil) != nil {
		t.Error("contractWeights(nil) != nil")
	}
	got := contractWeights(map[int]valmgr.Validator{2: {Weight: 42}})
	if len(got) != 1 || got[2] != 42 {
		t.Errorf("contractWeights = %v", got)
	}
}

// TestDelayForAttempt pins the escalating retry schedule: fast early (cold
// aggregator resolves in seconds), settling at 2m for slow Fuji coverage.
func TestDelayForAttempt(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{2, time.Second},
		{3, 5 * time.Second},
		{4, 10 * time.Second},
		{5, 15 * time.Second},
		{6, 30 * time.Second},
		{7, time.Minute},
		{8, 2 * time.Minute},
		{9, 2 * time.Minute},
		{24, 2 * time.Minute},
	}
	for _, tt := range tests {
		if got := delayForAttempt(tt.attempt); got != tt.want {
			t.Errorf("delayForAttempt(%d) = %s, want %s", tt.attempt, got, tt.want)
		}
	}
	// Total wait across all attempts stays within the 30-45 min cap.
	var total time.Duration
	for a := 2; a <= weightConvergeAttempts; a++ {
		total += delayForAttempt(a)
	}
	if total < 30*time.Minute || total > 45*time.Minute {
		t.Errorf("total retry wait %s outside 30-45 min", total)
	}
}
