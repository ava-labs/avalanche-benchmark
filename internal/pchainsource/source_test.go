package pchainsource

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStartFollowingOmitsBootstrapFields(t *testing.T) {
	root := testRoot(t)
	manager := New(root, "fuji", os.Stdout)
	manager.exec = func(string, []string, []string) error {
		return nil
	}

	if err := manager.Start("following"); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(t, root)
	if _, exists := cfg["bootstrap-ips"]; exists {
		t.Fatal("following config contains bootstrap-ips")
	}
	if _, exists := cfg["bootstrap-ids"]; exists {
		t.Fatal("following config contains bootstrap-ids")
	}
}

func TestStartFrozenWritesExplicitEmptyBootstrapFields(t *testing.T) {
	root := testRoot(t)
	manager := New(root, "fuji", os.Stdout)
	manager.exec = func(string, []string, []string) error {
		return nil
	}

	if err := manager.Start("frozen"); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(t, root)
	for _, field := range []string{"bootstrap-ips", "bootstrap-ids"} {
		value, exists := cfg[field]
		if !exists || value != "" {
			t.Fatalf("%s=%v, exists=%t", field, value, exists)
		}
	}
}

func TestStartRejectsUnknownMode(t *testing.T) {
	manager := New(t.TempDir(), "fuji", os.Stdout)
	if err := manager.Start("freeze"); err == nil {
		t.Fatal("expected invalid mode error")
	}
}

func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "avalanchego"), []byte("test"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func readConfig(t *testing.T, root string) map[string]any {
	t.Helper()
	path := filepath.Join(root, "data", "pchain-source", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}
