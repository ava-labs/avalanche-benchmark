package oraclerelay

import (
	"io"
	"math/big"
	"testing"

	"github.com/ava-labs/avalanchego/ids"
	ethcommon "github.com/ava-labs/libevm/common"
	ethtypes "github.com/ava-labs/libevm/core/types"
)

// makeLog builds a SendWarpMessage-style log carrying the given submission.
func makeLog(t *testing.T, assetID ethcommon.Hash, price, updatedAt, seq int64, block uint64) ethtypes.Log {
	t.Helper()
	data, _ := buildEventData(t, WarpPrecompileAddress, Submission{
		AssetID:   assetID,
		Price:     big.NewInt(price),
		UpdatedAt: big.NewInt(updatedAt),
		Seq:       big.NewInt(seq),
	})
	return ethtypes.Log{Data: data, BlockNumber: block}
}

func TestCollectBatchOrderAndSeqGate(t *testing.T) {
	fresh := newFreshnessGate()
	meters := newMetrics()
	logs := make(chan ethtypes.Log, 8)
	logs <- makeLog(t, assetUSDC, 25e8, 100, 1, 11)   // fresh
	logs <- makeLog(t, assetBTC, 60000e8, 100, 1, 12) // stale: BTC seq 1 already delivered as first
	logs <- makeLog(t, assetBTC, 60050e8, 101, 2, 13) // fresh

	first := makeLog(t, assetBTC, 60000e8, 100, 1, 10)
	batch, err := collectBatch(first, logs, fresh, meters, io.Discard)
	if err != nil {
		t.Fatalf("collectBatch: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("batch size = %d, want 3 (one dup skipped)", len(batch))
	}
	// Arrival order preserved; this order becomes the predicate order.
	if batch[0].asset != "BTC-USD" || batch[0].seq != 1 || batch[0].oracleBlock != 10 {
		t.Fatalf("batch[0] = %+v", batch[0])
	}
	if batch[1].asset != "USDC-USD" || batch[1].seq != 1 {
		t.Fatalf("batch[1] = %+v", batch[1])
	}
	if batch[2].asset != "BTC-USD" || batch[2].seq != 2 {
		t.Fatalf("batch[2] = %+v", batch[2])
	}
}

func TestCollectBatchCapsAtMaxBatchSize(t *testing.T) {
	fresh := newFreshnessGate()
	meters := newMetrics()
	logs := make(chan ethtypes.Log, maxBatchSize+8)
	// seq 2..(maxBatchSize+4), all fresh and increasing for one asset.
	for seq := 2; seq <= maxBatchSize+4; seq++ {
		logs <- makeLog(t, assetBTC, 60000e8, int64(100+seq), int64(seq), uint64(seq))
	}
	first := makeLog(t, assetBTC, 60000e8, 100, 1, 1) // seq 1

	batch, err := collectBatch(first, logs, fresh, meters, io.Discard)
	if err != nil {
		t.Fatalf("collectBatch: %v", err)
	}
	if len(batch) != maxBatchSize {
		t.Fatalf("batch size = %d, want %d", len(batch), maxBatchSize)
	}
	// 1 first + 19 buffered = 20 total; a batch takes 16, so 4 stay buffered for
	// the next batch.
	if remaining := len(logs); remaining != 4 {
		t.Fatalf("remaining buffered = %d, want 4", remaining)
	}
}

func TestBatchGasLimit(t *testing.T) {
	cases := map[int]uint64{
		0:  400000,
		1:  650000,
		2:  900000,
		16: 4400000,
	}
	for n, want := range cases {
		if got := batchGasLimit(n); got != want {
			t.Fatalf("batchGasLimit(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestPackReceivePrices(t *testing.T) {
	data := packReceivePrices(5)
	if len(data) != 4+32 {
		t.Fatalf("calldata length = %d, want 36", len(data))
	}
	// receivePrices(uint32) = keccak256("receivePrices(uint32)")[:4].
	wantSelector := []byte{0x0f, 0xb5, 0x7c, 0xdd}
	for i, b := range wantSelector {
		if data[i] != b {
			t.Fatalf("selector byte %d = %#x, want %#x", i, data[i], b)
		}
	}
	if got := new(big.Int).SetBytes(data[4:36]).Int64(); got != 5 {
		t.Fatalf("count word = %d, want 5", got)
	}
}

func TestFreshnessGateBySeq(t *testing.T) {
	gate := newFreshnessGate()
	var btc, usdc [32]byte
	btc[0], usdc[0] = 1, 2

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
	if !gate.fresher(usdc, 1) {
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
