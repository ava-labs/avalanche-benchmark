package main

import "testing"

func TestParseConfigOneShot(t *testing.T) {
	cfg, err := parseConfig([]string{
		"--rpcs=http://127.0.0.1:9650/ext/bc/chain/rpc,http://127.0.0.2:9650/ext/bc/chain/rpc",
		"--time=40s",
		"--starting-tps=1000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.oneShot {
		t.Fatal("expected one-shot mode")
	}
	if cfg.duration.String() != "40s" {
		t.Fatalf("unexpected duration: %s", cfg.duration)
	}
	if cfg.startingTPS != 1000 {
		t.Fatalf("unexpected starting TPS: %d", cfg.startingTPS)
	}
	if len(cfg.rpcs) != 2 {
		t.Fatalf("expected 2 RPCs, got %d", len(cfg.rpcs))
	}
}

func TestParseConfigRejectsBareDurationNumber(t *testing.T) {
	_, err := parseConfig([]string{
		"--rpcs=http://127.0.0.1:9650/ext/bc/chain/rpc",
		"--time=40",
	})
	if err == nil {
		t.Fatal("expected bare duration number to fail")
	}
}

func TestParseConfigRejectsOutOfRangeTPS(t *testing.T) {
	for _, tps := range []string{"99", "6001"} {
		_, err := parseConfig([]string{
			"--rpcs=http://127.0.0.1:9650/ext/bc/chain/rpc",
			"--starting-tps=" + tps,
		})
		if err == nil {
			t.Fatalf("expected TPS %s to fail", tps)
		}
	}
}

func TestParseConfigRejectsOldFlags(t *testing.T) {
	_, err := parseConfig([]string{
		"--rpc=http://127.0.0.1:9650/ext/bc/chain/rpc",
		"--duration=40s",
	})
	if err == nil {
		t.Fatal("expected old flags to fail")
	}
}
