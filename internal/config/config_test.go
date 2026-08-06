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
	if environment.SystemInstall {
		t.Fatal("the user-level install must be the default")
	}
}

// The system install is an explicit opt-in with fixed paths, so it cannot be
// combined with the user-install directory overrides, and the flag itself
// must parse strictly.
func TestLoadFleetEnvironmentSystemInstallRules(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "fleet-key")
	writeFile(t, keyPath, "private")
	base := []string{
		"NETWORK=fuji",
		"PCHAIN_API=https://api.avax-test.network",
		"SSH_USER=ubuntu",
		"SSH_KEY_PATH=" + keyPath,
	}
	load := func(t *testing.T, extra ...string) (FleetEnvironment, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), ".env")
		writeFile(t, path, strings.Join(append(append([]string{}, base...), extra...), "\n"))
		return LoadFleetEnvironment(path)
	}

	environment, err := load(t, "SYSTEM_INSTALL=true")
	if err != nil || !environment.SystemInstall {
		t.Fatalf("SYSTEM_INSTALL=true: environment=%+v err=%v", environment, err)
	}
	if environment, err = load(t, "SYSTEM_INSTALL=false"); err != nil || environment.SystemInstall {
		t.Fatalf("SYSTEM_INSTALL=false: environment=%+v err=%v", environment, err)
	}
	if _, err := load(t, "SYSTEM_INSTALL=yes"); err == nil {
		t.Fatal("SYSTEM_INSTALL=yes must be rejected")
	}
	if _, err := load(t, "SYSTEM_INSTALL=true", "REMOTE_DIR=/home/op/kit"); err == nil {
		t.Fatal("SYSTEM_INSTALL with REMOTE_DIR must be rejected")
	}
	if _, err := load(t, "SYSTEM_INSTALL=true", "REMOTE_DATA_DIR=/nvme/data"); err == nil {
		t.Fatal("SYSTEM_INSTALL with REMOTE_DATA_DIR must be rejected")
	}
	// REMOTE_DATA_DIR alone is valid now: it re-points the data of the
	// DEFAULT user install.
	if environment, err = load(t, "REMOTE_DATA_DIR=/nvme/data"); err != nil || environment.RemoteDataDir != "/nvme/data" {
		t.Fatalf("REMOTE_DATA_DIR alone: environment=%+v err=%v", environment, err)
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
		"missing pchain":               base[:5],
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

// Shape opinions warn instead of refusing: operators may experiment with any
// validator, rpc, and archive count, and only the structural rules stay hard.
func TestLoadNodesAllowsUnconventionalShapes(t *testing.T) {
	for name, lines := range map[string][]string{
		"single validator, no rpc": {"1 host=v1 role=validator", "2 host=p1 role=pchain"},
		"three validators":         {"1 host=v1 role=validator", "2 host=v2 role=validator", "3 host=v3 role=validator", "4 host=r1 role=rpc", "5 host=p1 role=pchain"},
		"single archive":           {"1 host=v1 role=validator", "2 host=v2 role=validator", "3 host=v3 role=validator", "4 host=v4 role=validator", "5 host=r1 role=rpc", "6 host=p1 role=pchain", "7 host=a1 role=archive"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nodes.ini")
			writeFile(t, path, strings.Join(lines, "\n"))
			if _, err := LoadNodes(path); err != nil {
				t.Fatalf("unconventional shape refused: %v", err)
			}
		})
	}
}

// The chain field defaults to main, oracle roles pin the oracle chain, and
// the P-chain node belongs to no chain.
func TestLoadNodesChainField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.ini")
	writeFile(t, path, strings.Join([]string{
		"1 host=v1 role=validator",
		"2 host=v2 role=validator",
		"3 host=v3 role=validator",
		"4 host=v4 role=validator",
		"5 host=r1 role=rpc",
		"6 host=p1 role=pchain",
		"7 host=t1 role=validator chain=trading",
		"8 host=t2 role=validator chain=trading",
		"9 host=t3 role=validator chain=trading",
		"10 host=t4 role=validator chain=trading",
		"11 host=t5 role=rpc chain=trading",
		"12 host=o1 role=oracle-validator",
		"13 host=o2 role=oracle-rpc chain=oracle",
	}, "\n"))

	nodes, err := LoadNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	byNumber := make(map[int]Node, len(nodes))
	for _, node := range nodes {
		byNumber[node.Number] = node
	}
	for number, want := range map[int]string{
		1: "main", 5: "main", 6: "", 7: "trading", 11: "trading", 12: "oracle", 13: "oracle",
	} {
		if byNumber[number].Chain != want {
			t.Fatalf("node %d chain = %q, want %q", number, byNumber[number].Chain, want)
		}
	}
	chains := Chains(nodes)
	if len(chains) != 3 || chains[0] != "main" || chains[1] != "oracle" || chains[2] != "trading" {
		t.Fatalf("Chains = %v, want [main oracle trading]", chains)
	}
}

// Chains puts main first even when other chains sort before it, and skips
// the P-chain node.
func TestChainsOrdersMainFirst(t *testing.T) {
	chains := Chains([]Node{
		{Number: 1, Chain: "alpha"},
		{Number: 2, Chain: "main"},
		{Number: 3, Chain: "beta"},
		{Number: 4, Chain: ""},
		{Number: 5, Chain: "alpha"},
	})
	if len(chains) != 3 || chains[0] != "main" || chains[1] != "alpha" || chains[2] != "beta" {
		t.Fatalf("Chains = %v, want [main alpha beta]", chains)
	}
}

func TestLoadNodesChainErrors(t *testing.T) {
	base := []string{
		"1 host=v1 role=validator",
		"2 host=v2 role=validator",
		"3 host=v3 role=validator",
		"4 host=v4 role=validator",
		"5 host=r1 role=rpc",
		"6 host=p1 role=pchain",
	}
	tests := map[string][]string{
		"uppercase chain name":      append(append([]string{}, base...), "7 host=t1 role=validator chain=Trading"),
		"chain name too long":       append(append([]string{}, base...), "7 host=t1 role=validator chain=aaaaaaaaaaaaaaaaaaaaa"),
		"reserved oracle name":      append(append([]string{}, base...), "7 host=t1 role=validator chain=oracle"),
		"reserved management name":  append(append([]string{}, base...), "7 host=t1 role=validator chain=management"),
		"oracle role other chain":   append(append([]string{}, base...), "7 host=o1 role=oracle-validator chain=trading", "8 host=o2 role=oracle-rpc"),
		"pchain with chain":         []string{"1 host=v1 role=validator", "2 host=v2 role=validator", "3 host=v3 role=validator", "4 host=v4 role=validator", "5 host=r1 role=rpc", "6 host=p1 role=pchain chain=main"},
		"chain without a validator": append(append([]string{}, base...), "7 host=t1 role=rpc chain=trading"),
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

// weight= overrides the default ladder per node. A chain sets it on all of
// its validators or on none.
func TestLoadNodesWeightTag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.ini")
	writeFile(t, path, strings.Join([]string{
		"1 host=v1 role=validator weight=70000",
		"2 host=v2 role=validator weight=70000",
		"3 host=v3 role=validator weight=500",
		"4 host=v4 role=validator weight=1",
		"5 host=r1 role=rpc",
		"6 host=p1 role=pchain",
		"7 host=t1 role=validator chain=trading",
		"8 host=t2 role=validator chain=trading",
		"9 host=o1 role=oracle-validator weight=2000",
		"10 host=o2 role=oracle-rpc",
	}, "\n"))

	nodes, err := LoadNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	byNumber := make(map[int]Node, len(nodes))
	for _, node := range nodes {
		byNumber[node.Number] = node
	}
	for number, want := range map[int]uint64{
		1: 70000, 2: 70000, 3: 500, 4: 1, 5: 0, 6: 0, 7: 0, 8: 0, 9: 2000, 10: 0,
	} {
		if byNumber[number].Weight != want {
			t.Fatalf("node %d weight = %d, want %d", number, byNumber[number].Weight, want)
		}
	}
}

func TestLoadNodesWeightErrors(t *testing.T) {
	base := []string{
		"1 host=v1 role=validator",
		"2 host=v2 role=validator",
		"3 host=v3 role=validator",
		"4 host=v4 role=validator",
		"5 host=r1 role=rpc",
		"6 host=p1 role=pchain",
	}
	tests := map[string]struct {
		lines []string
		want  string
	}{
		"weight on rpc role": {
			lines: []string{"1 host=v1 role=validator weight=100", "2 host=r1 role=rpc weight=100", "3 host=p1 role=pchain"},
			want:  "weight= is valid only with role=validator or role=oracle-validator",
		},
		"weight on pchain role": {
			lines: []string{"1 host=v1 role=validator weight=100", "2 host=r1 role=rpc", "3 host=p1 role=pchain weight=100"},
			want:  "weight= is valid only with role=validator or role=oracle-validator",
		},
		"weight zero": {
			lines: []string{"1 host=v1 role=validator weight=0", "2 host=r1 role=rpc", "3 host=p1 role=pchain"},
			want:  "weight must be an integer of at least 1",
		},
		"weight not a number": {
			lines: []string{"1 host=v1 role=validator weight=heavy", "2 host=r1 role=rpc", "3 host=p1 role=pchain"},
			want:  "weight must be an integer of at least 1",
		},
		"mix on one chain": {
			lines: append(append([]string{}, base...), "7 host=v5 role=validator weight=100", "8 host=v6 role=validator weight=100"),
			want:  `chain "main" mixes explicit and default validator weights: weight= is set on node(s) 7, 8 and not set on node(s) 1, 2, 3, 4`,
		},
		"mix on the oracle chain": {
			lines: append(append([]string{}, base...), "7 host=o1 role=oracle-validator weight=100", "8 host=o2 role=oracle-validator", "9 host=o3 role=oracle-rpc"),
			want:  `chain "oracle" mixes explicit and default validator weights: weight= is set on node(s) 7 and not set on node(s) 8`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nodes.ini")
			writeFile(t, path, strings.Join(test.lines, "\n"))
			_, err := LoadNodes(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

// One chain with explicit weights leaves another chain's default ladder
// untouched.
func TestLoadNodesWeightPerChainIndependence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.ini")
	writeFile(t, path, strings.Join([]string{
		"1 host=v1 role=validator",
		"2 host=v2 role=validator",
		"3 host=r1 role=rpc",
		"4 host=p1 role=pchain",
		"5 host=t1 role=validator chain=trading weight=9",
		"6 host=t2 role=validator chain=trading weight=9",
	}, "\n"))
	if _, err := LoadNodes(path); err != nil {
		t.Fatalf("independent chains refused: %v", err)
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
