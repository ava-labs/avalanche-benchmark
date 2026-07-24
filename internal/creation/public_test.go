package creation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	ethcommon "github.com/ava-labs/libevm/common"
)

func TestLoadPublicRejectsPolicyDriftAndUnknownFields(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	nodes := []config.Node{
		{Number: 1, Role: config.RoleValidator},
		{Number: 2, Role: config.RoleValidator},
		{Number: 3, Role: config.RoleValidator},
		{Number: 4, Role: config.RoleValidator},
		{Number: 5, Role: config.RoleRPC},
		{Number: 6, Role: config.RolePChain},
	}
	generated, err := identity.Generate(private, nodes, 1)
	if err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(root, "public.json")
	if _, err := SavePublic(publicPath, NewPublic(
		generated,
		ethcommon.HexToAddress("0x1234567890123456789012345678901234567890"),
	)); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}

	wrongWeight := strings.Replace(string(contents), `"weight": 100000`, `"weight": 999`, 1)
	if err := os.WriteFile(publicPath, []byte(wrongWeight), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadPublic(publicPath); err == nil || !strings.Contains(err.Error(), "weight must be 100000") {
		t.Fatalf("wrong initial weight was accepted: %v", err)
	}

	unknown := strings.Replace(string(contents), `"genesisAddress":`, `"unknown": true, "genesisAddress":`, 1)
	if err := os.WriteFile(publicPath, []byte(unknown), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadPublic(publicPath); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field was accepted: %v", err)
	}
}
