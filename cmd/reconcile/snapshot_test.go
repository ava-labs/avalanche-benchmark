package main

import (
	"strings"
	"testing"
)

// TestSnapshotSourceVerdict pins the accept/refuse rules that protect every recovering
// node from being cloned off a stale or forked tracker (the 2026-06-22 site-A brick).
func TestSnapshotSourceVerdict(t *testing.T) {
	const h1, h2 = "0xaaaa", "0xbbbb"
	cases := []struct {
		name        string
		refTip      uint64
		srcTip      uint64
		common      uint64
		refHash     string
		srcHash     string
		wantOK      bool
		wantReasony string // substring the reason must contain
	}{
		{"at tip, same branch", 1000, 1000, 984, h1, h1, true, "OK"},
		{"minor load lag, same branch", 1000, 900, 884, h1, h1, true, "OK"},
		{"source slightly ahead (finalization skew)", 1000, 1005, 984, h1, h1, true, "OK"},
		{"lag exactly at limit", 3000, 1000, 984, h1, h1, true, "OK"},
		{"lag one past limit", 3001, 1000, 984, h1, h1, false, "STALE"},
		{"badly stale (the b4 case)", 308000, 283501, 283485, h1, h2, false, "STALE"},
		{"forked but caught up", 1000, 1000, 984, h1, h2, false, "FORKED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := snapshotSourceVerdict("b4", tc.refTip, tc.srcTip, tc.common, tc.refHash, tc.srcHash)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (reason: %s)", ok, tc.wantOK, reason)
			}
			if !strings.Contains(reason, tc.wantReasony) {
				t.Fatalf("reason %q does not contain %q", reason, tc.wantReasony)
			}
		})
	}
}

func TestCommonHeight(t *testing.T) {
	cases := []struct {
		refTip, srcTip, want uint64
	}{
		{1000, 1000, 984}, // equal tips, backed off 16
		{1000, 900, 884},  // lower of the two, backed off 16
		{900, 1000, 884},  // symmetric
		{20, 20, 4},       // small heights still back off
		{10, 5, 5},        // below the margin: no back-off (avoid underflow)
		{0, 0, 0},         // genesis
	}
	for _, tc := range cases {
		if got := commonHeight(tc.refTip, tc.srcTip); got != tc.want {
			t.Errorf("commonHeight(%d,%d) = %d, want %d", tc.refTip, tc.srcTip, got, tc.want)
		}
	}
}
