package creation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/internal/config"
	"github.com/ava-labs/avalanche-benchmark/internal/identity"
	ethcommon "github.com/ava-labs/libevm/common"
)

// Records written before the chain= field spell the oracle roles the legacy
// way. LoadPublic rewrites them to the generic role pinned to the oracle
// chain, with the flat oracle weight intact.
func TestLoadPublicNormalizesLegacyOracleRoles(t *testing.T) {
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
		{Number: 7, Role: config.RoleValidator, Chain: config.OracleChain},
		{Number: 8, Role: config.RoleRPC, Chain: config.OracleChain},
	}
	generated, err := identity.Generate(private, nodes, 1)
	if err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(root, "public.json")
	if _, err := SavePublic(publicPath, NewPublic(
		generated,
		nodes,
		ethcommon.HexToAddress("0x1234567890123456789012345678901234567890"),
		ethcommon.HexToAddress("0xAbcDef0123456789abCDef0123456789ABcdEF01"),
	)); err != nil {
		t.Fatal(err)
	}

	// Rewrite the file into the legacy shape: the old role strings and no
	// chain field, exactly what pre-multichain keygen wrote.
	contents, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}
	rewritten := 0
	for _, entry := range document["nodes"].([]any) {
		node := entry.(map[string]any)
		if node["chain"] != config.OracleChain {
			continue
		}
		delete(node, "chain")
		if node["role"] == string(config.RoleValidator) {
			node["role"] = string(config.RoleOracleValidator)
		} else {
			node["role"] = string(config.RoleOracleRPC)
		}
		rewritten++
	}
	if rewritten != 2 {
		t.Fatalf("expected to rewrite 2 oracle nodes, rewrote %d", rewritten)
	}
	legacy, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, legacy, 0o644); err != nil {
		t.Fatal(err)
	}

	public, _, err := LoadPublic(publicPath)
	if err != nil {
		t.Fatalf("legacy oracle record must load: %v", err)
	}
	oracleValidators, oracleRPCs := 0, 0
	for _, node := range public.Nodes {
		if node.Role == config.RoleOracleValidator || node.Role == config.RoleOracleRPC {
			t.Fatalf("legacy role survived load: %+v", node)
		}
		if node.ChainName() != config.OracleChain {
			continue
		}
		switch node.Role {
		case config.RoleValidator:
			if node.Weight != OracleWeight {
				t.Fatalf("oracle validator weight = %d, want %d", node.Weight, OracleWeight)
			}
			oracleValidators++
		case config.RoleRPC:
			oracleRPCs++
		}
	}
	if oracleValidators != 1 || oracleRPCs != 1 {
		t.Fatalf("oracle chain shape after load: validators=%d rpcs=%d, want 1 and 1", oracleValidators, oracleRPCs)
	}
	if !public.HasOracle() {
		t.Fatal("HasOracle must report the normalized oracle chain")
	}
}

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
		nodes,
		ethcommon.HexToAddress("0x1234567890123456789012345678901234567890"),
		ethcommon.HexToAddress("0xAbcDef0123456789abCDef0123456789ABcdEF01"),
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

	// A hand edit that marks one validator explicit while the rest keep the
	// ladder violates the all-or-none rule per chain.
	mixed := strings.Replace(string(contents), `"weight": 100000`, `"weight": 100000, "explicitWeight": true`, 1)
	if err := os.WriteFile(publicPath, []byte(mixed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadPublic(publicPath); err == nil || !strings.Contains(err.Error(), "mixes explicit and default validator weights") {
		t.Fatalf("mixed explicit and default weights were accepted: %v", err)
	}
}
