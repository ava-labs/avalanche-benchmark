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
