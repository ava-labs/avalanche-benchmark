package identity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
)

func TestGenerateCreatesFreshRoleSpecificIdentities(t *testing.T) {
	root := filepath.Join(t.TempDir(), "deployment")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	nodes := []config.Node{
		{Number: 1, Host: "v1", Role: config.RoleValidator},
		{Number: 2, Host: "r1", Role: config.RoleRPC},
	}

	set, err := Generate(root, nodes, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Nodes) != 2 || len(set.Manager) != 1 {
		t.Fatalf("unexpected identity counts: %+v", set)
	}
	if set.Nodes[0].Proof == nil {
		t.Fatal("validator must have a BLS proof")
	}
	if set.Nodes[1].Proof != nil {
		t.Fatal("RPC must not have a BLS proof")
	}
	if _, err := os.Stat(filepath.Join(root, "nodes", "2", "signer.key")); !os.IsNotExist(err) {
		t.Fatalf("RPC signer.key must not exist, got %v", err)
	}
	if set.Manager[0].Proof == nil {
		t.Fatal("manager identity must have a BLS proof")
	}

	if _, err := Generate(root, nodes, 1); err == nil {
		t.Fatal("identity generation must refuse existing output")
	}
}
