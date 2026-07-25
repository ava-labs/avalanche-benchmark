package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFiles(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	nodesPath := filepath.Join(dir, "nodes.ini")
	writeFile(t, envPath, strings.Join([]string{
		"NETWORK=fuji",
		"PCHAIN_API=https://api.avax-test.network",
		"FUNDING_PRIVATE_KEY=" + strings.Repeat("1", 64),
		"SSH_USER=ubuntu",
		"SSH_KEY_PATH=/tmp/fleet-key",
	}, "\n"))
	writeFile(t, nodesPath, strings.Join([]string{
		"5 host=rpc.example role=rpc",
		"6 host=pchain.example role=pchain",
		"4 host=v4.example role=validator dc=B",
		"2 host=v2.example role=validator dc=A",
		"1 host=v1.example role=validator dc=A",
		"3 host=v3.example role=validator dc=B",
	}, "\n"))

	cfg, err := LoadFiles(envPath, nodesPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Environment.Network != "fuji" {
		t.Fatalf("unexpected environment: %+v", cfg.Environment)
	}
	if len(cfg.Nodes) != 6 {
		t.Fatalf("expected 6 nodes, got %d", len(cfg.Nodes))
	}
	for i, node := range cfg.Nodes {
		if node.Number != i+1 {
			t.Fatalf("nodes not sorted: %+v", cfg.Nodes)
		}
	}
	if cfg.Nodes[4].DC != "" {
		t.Fatalf("omitted dc must remain empty, got %q", cfg.Nodes[4].DC)
	}
}

func TestLoadEnvironmentFailsLoudly(t *testing.T) {
	tests := map[string]string{
		"missing key": strings.Join([]string{
			"NETWORK=fuji",
			"PCHAIN_API=https://api.avax-test.network",
		}, "\n"),
		"unknown field": strings.Join([]string{
			"NETWORK=fuji",
			"PCHAIN_API=https://api.avax-test.network",
			"FUNDING_PRIVATE_KEY=" + strings.Repeat("1", 64),
			"LEGACY_FALLBACK=yes",
		}, "\n"),
		"prefixed key": strings.Join([]string{
			"NETWORK=fuji",
			"PCHAIN_API=https://api.avax-test.network",
			"FUNDING_PRIVATE_KEY=0x" + strings.Repeat("1", 64),
		}, "\n"),
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			writeFile(t, path, contents)
			if _, err := LoadEnvironment(path); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoadNetworkEnvironmentDoesNotRequireFundingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	writeFile(t, path, strings.Join([]string{
		"NETWORK=fuji",
		"PCHAIN_API=https://api.avax-test.network",
		"FUNDING_PRIVATE_KEY=",
	}, "\n"))

	environment, err := LoadNetworkEnvironment(path)
	if err != nil {
		t.Fatal(err)
	}
	if environment.Network != "fuji" {
		t.Fatalf("network = %q, want fuji", environment.Network)
	}
	if environment.PChainAPI != "https://api.avax-test.network" {
		t.Fatalf("PChainAPI = %q", environment.PChainAPI)
	}
}

func TestLoadFleetEnvironmentRequiresSSHButNotFundingKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "fleet-key")
	writeFile(t, keyPath, "private")
	path := filepath.Join(dir, ".env")
	writeFile(t, path, strings.Join([]string{
		"NETWORK=fuji",
		"PCHAIN_API=https://api.avax-test.network",
		"FUNDING_PRIVATE_KEY=",
		"SSH_USER=ubuntu",
		"SSH_KEY_PATH=" + keyPath,
	}, "\n"))

	environment, err := LoadFleetEnvironment(path)
	if err != nil {
		t.Fatal(err)
	}
	if environment.Network != "fuji" || environment.SSHUser != "ubuntu" || environment.SSHKeyPath != keyPath {
		t.Fatalf("unexpected fleet environment: %+v", environment)
	}
}

func TestLoadNodesFailsLoudly(t *testing.T) {
	base := []string{
		"1 host=v1 role=validator",
		"2 host=v2 role=validator",
		"3 host=v3 role=validator",
		"4 host=v4 role=validator",
		"5 host=r1 role=rpc",
		"6 host=p1 role=pchain",
	}
	tests := map[string][]string{
		"duplicate number":             append(append([]string{}, base...), "5 host=r2 role=rpc"),
		"unknown field":                append(append([]string{}, base...), "7 host=r2 role=rpc site=A"),
		"invalid role":                 []string{"1 host=v1 role=validator", "2 host=v2 role=validator", "3 host=v3 role=validator", "4 host=v4 role=validator", "5 host=r1 role=spare", "6 host=p1 role=pchain"},
		"missing rpc":                  append(append([]string{}, base[:4]...), "6 host=p1 role=pchain"),
		"missing pchain":               base[:5],
		"single archive":               append(append([]string{}, base...), "7 host=a1 role=archive"),
		"oracle validator without rpc": append(append([]string{}, base...), "7 host=o1 role=oracle-validator"),
		"oracle rpc without validator": append(append([]string{}, base...), "7 host=o1 role=oracle-rpc"),
	}
	for name, lines := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nodes.ini")
			writeFile(t, path, strings.Join(lines, "\n"))
			if _, err := LoadNodes(path); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoadNodesOracleAndArchiveRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.ini")
	writeFile(t, path, strings.Join([]string{
		"1 host=v1 role=validator",
		"2 host=v2 role=validator",
		"3 host=v3 role=validator",
		"4 host=v4 role=validator",
		"5 host=r1 role=rpc",
		"6 host=p1 role=pchain",
		"7 host=a1 role=archive",
		"8 host=a2 role=archive",
		"9 host=o1 role=oracle-validator",
		"10 host=o2 role=oracle-validator",
		"11 host=r1 role=oracle-rpc", // co-hosted with the main rpc
	}, "\n"))

	nodes, err := LoadNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 11 {
		t.Fatalf("expected 11 nodes, got %d", len(nodes))
	}
	roles := map[Role]int{}
	for _, node := range nodes {
		roles[node.Role]++
	}
	if roles[RoleArchive] != 2 || roles[RoleOracleValidator] != 2 || roles[RoleOracleRPC] != 1 {
		t.Fatalf("unexpected role counts: %v", roles)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
