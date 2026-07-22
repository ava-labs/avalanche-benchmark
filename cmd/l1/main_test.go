package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanchego/ids"
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

func TestAddressLifecycleAllowsPreCreationAndRejectsDestroyedStateLocally(t *testing.T) {
	root := t.TempDir()
	environment := config.Environment{Network: "fuji"}
	if err := rejectDestroyedDeployment(context.Background(), root, environment); err != nil {
		t.Fatalf("pre-creation address must remain available: %v", err)
	}
	deploymentDirectory := filepath.Join(root, "deployment")
	if err := os.Mkdir(deploymentDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	state := strings.Join([]string{
		"NETWORK=fuji",
		"MANAGER_SUBNET_ID=" + ids.GenerateTestID().String(),
		"MANAGER_CHAIN_ID=" + ids.GenerateTestID().String(),
		"SUBNET_ID=" + ids.GenerateTestID().String(),
		"CHAIN_ID=" + ids.GenerateTestID().String(),
		"CONVERT_TX_ID=" + ids.GenerateTestID().String(),
		"DESTROYED=true",
	}, "\n")
	if err := os.WriteFile(filepath.Join(deploymentDirectory, "network.env"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectDestroyedDeployment(context.Background(), root, environment); err == nil || !strings.Contains(err.Error(), "deployment has no active validators") {
		t.Fatalf("expected local terminal-state rejection, got %v", err)
	}
	environmentFile := strings.Join([]string{
		"NETWORK=fuji",
		"PCHAIN_API=https://must-not-be-called.invalid",
		"FUNDING_PRIVATE_KEY=" + strings.Repeat("01", 32),
		"MANAGER_COMMITTEE=1",
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte(environmentFile), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := map[string]func() error{
		"address": func() error { return showAddress(root) },
		"weights": func() error { return showWeights(root) },
		"topup":   func() error { return topUp(root, "1") },
		"destroy": func() error { return destroyL1s(root) },
	}
	for name, command := range commands {
		if err := command(); err == nil || !strings.Contains(err.Error(), "deployment has no active validators") {
			t.Fatalf("%s must reject terminal state locally, got %v", name, err)
		}
	}
	if err := generateKey(root); err == nil || !strings.Contains(err.Error(), "keygen is only valid before creation") {
		t.Fatalf("keygen must reject existing deployment state before mutation, got %v", err)
	}
}
