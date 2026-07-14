package main

import (
	"testing"
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
