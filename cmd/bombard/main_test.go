package main

import (
	"testing"
	"time"
)

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

func TestSnapshotLatencyWindowKeepsOnlyLastTenSeconds(t *testing.T) {
	now := time.Unix(100, 0)
	tracker := newTxTracker()

	tracker.mu.Lock()
	tracker.recordLatencyLocked(latencySample{
		observedAt: now.Add(-11 * time.Second),
		total:      time.Millisecond,
	})
	tracker.recordLatencyLocked(latencySample{
		observedAt: now.Add(-9 * time.Second),
		total:      2 * time.Millisecond,
	})
	tracker.mu.Unlock()

	samples, _ := tracker.snapshotLatencyWindow(now)
	if len(samples) != 1 {
		t.Fatalf("expected 1 recent sample, got %d", len(samples))
	}
	if samples[0].total != 2*time.Millisecond {
		t.Fatalf("unexpected sample total: %s", samples[0].total)
	}
}

func TestLatencyWindowKeepsCutoffSample(t *testing.T) {
	now := time.Unix(100, 0)
	tracker := newTxTracker()

	tracker.mu.Lock()
	tracker.recordLatencyLocked(latencySample{
		observedAt: now.Add(-latencyWindow),
		total:      time.Millisecond,
	})
	tracker.mu.Unlock()

	samples, _ := tracker.snapshotLatencyWindow(now)
	if len(samples) != 1 {
		t.Fatalf("expected cutoff sample to remain, got %d samples", len(samples))
	}
}

func TestLatestBlockSnapshot(t *testing.T) {
	tracker := newTxTracker()
	if _, ok := tracker.latestBlock(); ok {
		t.Fatal("expected latest block to start unset")
	}

	tracker.setLatestBlock(123)
	block, ok := tracker.latestBlock()
	if !ok {
		t.Fatal("expected latest block to be set")
	}
	if block != 123 {
		t.Fatalf("unexpected latest block: %d", block)
	}
}
