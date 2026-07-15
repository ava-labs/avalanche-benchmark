package main

import (
	"reflect"
	"testing"
)

func TestParseTargets(t *testing.T) {
	got, err := parseTargets("a1=100000, a2=100000 ,b1=1")
	if err != nil {
		t.Fatal(err)
	}
	want := []target{{"a1", 100000}, {"a2", 100000}, {"b1", 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseTargets = %v, want %v", got, want)
	}
	for _, bad := range []string{"", "a1", "a1=x", "a1=1,a1=2"} {
		if _, err := parseTargets(bad); err == nil {
			t.Errorf("parseTargets(%q) should error", bad)
		}
	}
}

func TestOrderTargetsRaisesFirst(t *testing.T) {
	ts := []target{
		{"a1", 1},      // lower
		{"b1", 100000}, // raise
		{"a2", 1000},   // no-op
		{"b2", 100000}, // raise
		{"a3", 1},      // lower
	}
	current := map[string]uint64{"a1": 100000, "b1": 1000, "a2": 1000, "b2": 1000, "a3": 100000}
	got := orderTargets(ts, current)
	want := []target{{"b1", 100000}, {"b2", 100000}, {"a1", 1}, {"a3", 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orderTargets = %v, want %v", got, want)
	}
	if out := orderTargets([]target{{"a1", 5}}, map[string]uint64{"a1": 5}); len(out) != 0 {
		t.Fatalf("all no-ops should return empty, got %v", out)
	}
}

// TestPlanStepsStaggered: from 8 equal validators to 4 heavy + 4 low, the
// plan must be raises-first, never exceed the per-step bound, and land exactly
// on target. No live chain needed.
func TestPlanStepsStaggered(t *testing.T) {
	names := []string{"a1", "a2", "a3", "a4", "b1", "b2", "b3", "b4"}
	targetW := map[string]uint64{
		"a1": 100000, "a2": 100000, "a3": 100000, "a4": 100000, // heavy (raise)
		"b1": 10, "b2": 10, "b3": 10, "b4": 10, // low (lower)
	}
	start := map[string]uint64{}
	var ts []target
	var total uint64
	for _, n := range names {
		start[n] = 1000 // 8 equal
		total += 1000
		ts = append(ts, target{name: n, weight: targetW[n]})
	}

	steps := planSteps(orderTargets(ts, start), start, total)
	if len(steps) == 0 {
		t.Fatal("planSteps produced no steps")
	}

	cur := map[string]uint64{}
	for k, v := range start {
		cur[k] = v
	}
	liveTotal := total
	seenLower := false
	for _, s := range steps {
		c := cur[s.name]
		var delta uint64
		raising := s.weight > c
		if raising {
			delta = s.weight - c
		} else {
			delta = c - s.weight
		}
		// (a) raises-first: no raise step may follow a lower step.
		if raising && seenLower {
			t.Fatalf("raise step for %s came after a lower step (not raises-first)", s.name)
		}
		if !raising {
			seenLower = true
		}
		// (b) per-step bound: no step moves more than maxStepDelta of the
		// live total at the moment it fires.
		if bound := maxStepDelta(liveTotal); delta > bound {
			t.Fatalf("step %s -> %d moves %d, exceeds bound %d (live total %d)",
				s.name, s.weight, delta, bound, liveTotal)
		}
		liveTotal = liveTotal - c + s.weight
		cur[s.name] = s.weight
	}
	// (c) ends exactly at target.
	for n, w := range targetW {
		if cur[n] != w {
			t.Fatalf("%s ended at %d, want %d", n, cur[n], w)
		}
	}
}

func TestRunwayDays(t *testing.T) {
	// 512 nAVAX/s for 7 days = 309,657,600 nAVAX (~0.31 AVAX).
	week := uint64(512 * 7 * 86400)
	if d := runwayDays(week, 512); d != 7 {
		t.Fatalf("runwayDays(week) = %v, want 7", d)
	}
	if d := runwayDays(week/2, 512); d >= runwayWarnDays {
		t.Fatalf("half a week (%v days) must be under the warn threshold", d)
	}
}
