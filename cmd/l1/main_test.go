package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
)

func TestUsageRepeatsProgramName(t *testing.T) {
	for _, program := range []string{"l1", "avalanche-benchmark"} {
		output := usage(program)
		lines := strings.Split(output, "\n")
		if len(lines) != 7 {
			t.Fatalf("expected seven usage lines, got %q", output)
		}
		for _, line := range lines {
			if !strings.HasPrefix(line, "  "+program+" ") {
				t.Fatalf("usage line does not start with program name %q: %q", program, line)
			}
		}
	}
}

func TestPreCreationCommandsDoNotInventLifecycleState(t *testing.T) {
	root := t.TempDir()
	environment := config.Environment{Network: "fuji"}
	if err := rejectDestroyedDeployment(context.Background(), root, environment); err != nil {
		t.Fatalf("pre-creation address must remain available: %v", err)
	}
	deploymentDirectory := filepath.Join(root, "deployment")
	if err := os.Mkdir(deploymentDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploymentDirectory, "network.env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := generateKey(root); err == nil || !strings.Contains(err.Error(), "keygen is only valid before creation") {
		t.Fatalf("keygen must reject existing deployment state before mutation, got %v", err)
	}
}

func TestSetWeightRejectsUnsupportedWeightBeforeLoadingFiles(t *testing.T) {
	err := setWeight(t.TempDir(), "4", "500")
	if err == nil || !strings.Contains(err.Error(), "must be 1, 1000, or 100000") {
		t.Fatalf("unexpected error: %v", err)
	}
}
