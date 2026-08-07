package identity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/internal/config"
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
	if set.Nodes[0].Name != "a" || set.Nodes[1].Name != "b" || set.Manager[0].Name != "a" {
		t.Fatalf("unexpected identity names: %+v", set)
	}
	if _, err := os.Stat(filepath.Join(root, "identities", "b", "signer.key")); !os.IsNotExist(err) {
		t.Fatalf("RPC signer.key must not exist, got %v", err)
	}
	if set.Manager[0].Proof == nil {
		t.Fatal("manager identity must have a BLS proof")
	}

	if _, err := Generate(root, nodes, 1); err == nil {
		t.Fatal("identity generation must refuse existing output")
	}
}

func TestIdentityNames(t *testing.T) {
	for index, expected := range map[int]string{0: "a", 25: "z", 26: "aa", 27: "ab", 701: "zz", 702: "aaa"} {
		name := Name(index)
		if name != expected {
			t.Errorf("Name(%d) = %q, want %q", index, name, expected)
		}
		parsed, err := Index(name)
		if err != nil || parsed != index {
			t.Errorf("Index(%q) = %d, %v; want %d", name, parsed, err, index)
		}
	}
	for _, invalid := range []string{"", "A", "1", "a-", "zzzzzzzzzzzzzzzzzzzz"} {
		if _, err := Index(invalid); err == nil {
			t.Errorf("Index(%q) succeeded", invalid)
		}
	}
}
