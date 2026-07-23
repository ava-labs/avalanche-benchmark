package pchainsource

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingAcceptedMetricMeansInitializing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# no P-chain metric yet\n"))
	}))
	t.Cleanup(server.Close)

	manager := New(t.TempDir(), "fuji", "https://api.avax-test.network", os.Stdout)
	value, found, err := manager.fetchMetric(
		context.Background(),
		server.URL,
		`avalanche_snowman_bs_accepted{chain="P"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if found || value != 0 {
		t.Fatalf("missing metric returned value=%f found=%t", value, found)
	}
}

func TestFetchStartHeightUsesLatestBootstrapLog(t *testing.T) {
	manager := New(t.TempDir(), "fuji", "https://api.avax-test.network", os.Stdout)
	manager.runner = staticRunner{
		output: []byte(`[07-23|08:45:39.683] INFO <P Chain> bootstrap/bootstrapper.go:212 starting bootstrapper {"lastAcceptedID":"id","lastAcceptedHeight":289529}`),
	}
	height, found, err := manager.fetchStartHeight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !found || height != 289529 {
		t.Fatalf("startup height=%d found=%t", height, found)
	}
}

type staticRunner struct {
	output []byte
	err    error
}

func (r staticRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return r.output, r.err
}

func TestRenderUnitPreservesOneProcessAndOneConfig(t *testing.T) {
	unit := renderUnit("ubuntu", "/opt/benchmark/bin/avalanchego", "/opt/benchmark/data/pchain-source/config.json")
	for _, expected := range []string{
		"User=ubuntu",
		`ExecStart="/opt/benchmark/bin/avalanchego" --config-file="/opt/benchmark/data/pchain-source/config.json"`,
		"Restart=on-failure",
	} {
		if !strings.Contains(unit, expected) {
			t.Fatalf("unit does not contain %q:\n%s", expected, unit)
		}
	}
}

func TestLoadConfigDerivesModeFromBootstrapLists(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data", "pchain-source")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := nodeConfig{
		NetworkID:    "fuji",
		BootstrapIPs: "1.2.3.4:9651",
		BootstrapIDs: "NodeID-7Xhw2mDxuDS44j42TCB6U5579esbSt3Lg",
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New(root, "fuji", "https://api.avax-test.network", os.Stdout)
	loaded, err := manager.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BootstrapIPs == "" || loaded.BootstrapIDs == "" {
		t.Fatalf("expected following config, got %+v", loaded)
	}
}

func TestLoadConfigRejectsHalfConfiguredBootstrap(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data", "pchain-source")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"network-id":"fuji","bootstrap-ips":"1.2.3.4:9651"}`)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New(root, "fuji", "https://api.avax-test.network", os.Stdout)
	if _, err := manager.loadConfig(); err == nil || !strings.Contains(err.Error(), "only one bootstrap list") {
		t.Fatalf("expected half-configured bootstrap error, got %v", err)
	}
}
