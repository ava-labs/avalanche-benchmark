package oraclerelay

import (
	"math/big"
	"testing"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
)

func TestFreshnessGateBySeq(t *testing.T) {
	gate := newFreshnessGate()
	var btc, avax [32]byte
	btc[0], avax[0] = 1, 2

	if !gate.fresher(btc, 1) {
		t.Fatal("first seq for an asset must pass")
	}
	if gate.fresher(btc, 1) {
		t.Fatal("a repeated seq must be skipped")
	}
	if gate.fresher(btc, 1) {
		t.Fatal("a lower/equal seq must be skipped")
	}
	if !gate.fresher(btc, 2) {
		t.Fatal("a higher seq must pass")
	}
	// A different asset is tracked independently.
	if !gate.fresher(avax, 1) {
		t.Fatal("first seq for a second asset must pass")
	}
	if gate.fresher(btc, 2) {
		t.Fatal("btc seq 2 must not pass twice")
	}
}

func TestPriorityGasPrice(t *testing.T) {
	cases := []struct {
		suggested int64
		want      int64
	}{
		{0, minDeliveryGasPrice}, // floor guards a zero suggestion
		{1, 10},                  // 1 * 10 == floor
		{25, 250},                // 25 * 10
		{1_000_000_000, 10_000_000_000},
	}
	for _, tc := range cases {
		got := priorityGasPrice(big.NewInt(tc.suggested))
		if got.Int64() != tc.want {
			t.Fatalf("priorityGasPrice(%d) = %s, want %d", tc.suggested, got, tc.want)
		}
	}
}

func TestCanonicalSetCacheRefetchDecision(t *testing.T) {
	var cache canonicalSetCache

	if !cache.needsRefetch(10) {
		t.Fatal("empty cache must require a fetch")
	}

	cache.store(10, validators.WarpSet{TotalWeight: 42})
	if cache.needsRefetch(10) {
		t.Fatal("same pinned height must reuse the cached set")
	}
	if cache.set.TotalWeight != 42 {
		t.Fatalf("cached set not stored: TotalWeight = %d", cache.set.TotalWeight)
	}
	if !cache.needsRefetch(11) {
		t.Fatal("a new pinned height must trigger a refetch")
	}

	cache.store(11, validators.WarpSet{TotalWeight: 7})
	if cache.needsRefetch(11) {
		t.Fatal("cache must track the latest stored height")
	}
	if cache.needsRefetch(10) {
		// A height going backwards is still a change and must refetch.
		return
	}
	t.Fatal("a different (earlier) height must trigger a refetch")
}

func TestWSEndpoint(t *testing.T) {
	chainID, err := ids.FromString("2oYMBNV4eNHyqk2fjjV5nVQLDbtmNJzq5s3qs3Lo6ftnC6FByM")
	if err != nil {
		t.Fatalf("parse chain ID: %v", err)
	}
	cases := map[string]string{
		"http://10.0.0.15:9650":  "ws://10.0.0.15:9650/ext/bc/" + chainID.String() + "/ws",
		"https://node.example":   "wss://node.example/ext/bc/" + chainID.String() + "/ws",
		"http://10.0.0.15:9650/": "ws://10.0.0.15:9650/ext/bc/" + chainID.String() + "/ws",
		"ws://10.0.0.15:9650":    "ws://10.0.0.15:9650/ext/bc/" + chainID.String() + "/ws",
	}
	for input, want := range cases {
		got, err := wsEndpoint(input, chainID)
		if err != nil {
			t.Fatalf("wsEndpoint(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("wsEndpoint(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWSEndpointRejectsBadScheme(t *testing.T) {
	if _, err := wsEndpoint("ftp://node.example", ids.Empty); err == nil {
		t.Fatal("expected error for non-http/ws scheme")
	}
	if _, err := wsEndpoint("not a url", ids.Empty); err == nil {
		t.Fatal("expected error for malformed URL")
	}
}
