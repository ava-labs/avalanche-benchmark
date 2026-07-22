package main

import (
	"strings"
	"testing"
)

func TestUsageRepeatsProgramName(t *testing.T) {
	for _, program := range []string{"l1", "avalanche-benchmark"} {
		output := usage(program)
		lines := strings.Split(output, "\n")
		if len(lines) != 6 {
			t.Fatalf("expected six usage lines, got %q", output)
		}
		for _, line := range lines {
			if !strings.HasPrefix(line, "  "+program+" ") {
				t.Fatalf("usage line does not start with program name %q: %q", program, line)
			}
		}
	}
}
