package main

import (
	"strings"
	"testing"
	"time"
)

// TestInFlightLine pins the status "in flight" detector: only OTHER
// benchmark-fleet state-changing commands show; self, read-only verbs, and
// unrelated processes are ignored.
func TestInFlightLine(t *testing.T) {
	ps := `  PID ELAPSED COMMAND
    1     999 /sbin/init
  400     130 ./bin/benchmark-fleet up 7 8 9 10
  401    5000 ./bin/benchmark-fleet exporter
  402       1 ./bin/benchmark-fleet status
  403       2 /home/ubuntu/avalanche-benchmark/bin/benchmark-fleet status
  404      60 bash ./fleet up 7
`
	got := inFlightLine(ps, 403)
	want := "in flight: ./fleet up 7 8 9 10 (2m10s)"
	if got != want {
		t.Errorf("inFlightLine = %q, want %q", got, want)
	}
	if got := inFlightLine("  PID ELAPSED COMMAND\n  402  1 ./bin/benchmark-fleet status\n", 402); got != "" {
		t.Errorf("self/read-only should yield no line, got %q", got)
	}
}

// TestStateAgeLine pins the provenance line: fresh shows basename + age,
// >24h shouts STALE with the full path.
func TestStateAgeLine(t *testing.T) {
	if got := stateAgeLine("/home/u/kit/fleet-state.json", 27*time.Second); got != "state: fleet-state.json updated 27s ago" {
		t.Errorf("fresh = %q", got)
	}
	got := stateAgeLine("/home/u/kit/fleet-state.json", 49*time.Hour)
	if !strings.Contains(got, "STALE") || !strings.Contains(got, "/home/u/kit/fleet-state.json") {
		t.Errorf("stale = %q, want STALE with full path", got)
	}
}
