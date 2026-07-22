package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
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
		if !strings.Contains(output, "set-weight <identity-letter> <1|1000|100000>") {
			t.Fatalf("usage does not explain set-weight arguments: %q", output)
		}
		if !strings.Contains(output, "create [1|4]") {
			t.Fatalf("usage does not explain create committee argument: %q", output)
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

func TestCreateRejectsExistingDeploymentBeforeLoadingConfiguration(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "deployment"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := create(root, 1)
	if err == nil || err.Error() != "chain already exists in ./deployment; delete ./deployment only if you want a new chain" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSetWeightRejectsUnsupportedWeightBeforeLoadingFiles(t *testing.T) {
	err := setWeight(t.TempDir(), "d", "500")
	if err == nil || !strings.Contains(err.Error(), "must be 1, 1000, or 100000") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSetWeightRejectsNumericIdentityBeforeLoadingFiles(t *testing.T) {
	err := setWeight(t.TempDir(), "4", "1")
	if err == nil || !strings.Contains(err.Error(), "identity must be lowercase letters") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWeightsLoadLetteredIdentities(t *testing.T) {
	root := t.TempDir()
	nodes := []config.Node{
		{Number: 10, Role: config.RoleValidator},
		{Number: 20, Role: config.RoleRPC},
	}
	generated, err := identity.Generate(root, nodes, 1)
	if err != nil {
		t.Fatal(err)
	}
	names, err := loadIdentityNames(root, config.Config{Nodes: nodes})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("got %d weight identities, want main validator plus manager", len(names))
	}
	for _, generatedIdentity := range []struct {
		L1       string
		Identity identity.Identity
	}{
		{L1: "main", Identity: generated.Nodes[0]},
		{L1: "management", Identity: generated.Manager[0]},
	} {
		name := names[identityKey{L1: generatedIdentity.L1, NodeID: generatedIdentity.Identity.NodeID}]
		if name.Name != "a" {
			t.Errorf("%s identity name = %q, want a", generatedIdentity.L1, name.Name)
		}
	}
}

func TestDestroyRemovesUnconvertedDeploymentOnly(t *testing.T) {
	root := t.TempDir()
	environment := strings.Join([]string{
		"NETWORK=fuji",
		"PCHAIN_API=https://api.avax-test.network",
		"FUNDING_PRIVATE_KEY=" + strings.Repeat("1", 64),
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte(environment), 0o600); err != nil {
		t.Fatal(err)
	}
	deploymentPath := filepath.Join(root, "deployment")
	if err := os.Mkdir(deploymentPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploymentPath, "network.env"), []byte("NETWORK=fuji\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploymentPath, "private-key"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := destroyL1s(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(deploymentPath); !os.IsNotExist(err) {
		t.Fatalf("deployment must be removed, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".env")); err != nil {
		t.Fatalf(".env must remain: %v", err)
	}
}
