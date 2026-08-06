package oraclerelay

import (
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// metricValue reads a single collector's value without pulling in testutil
// (which would add a new test-only module dependency).
func metricValue(t *testing.T, collector prometheus.Collector) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 1)
	collector.Collect(ch)
	close(ch)
	metric := <-ch
	var out dto.Metric
	if err := metric.Write(&out); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	switch {
	case out.Gauge != nil:
		return out.Gauge.GetValue()
	case out.Counter != nil:
		return out.Counter.GetValue()
	default:
		t.Fatalf("metric is neither gauge nor counter")
		return 0
	}
}

func TestFeedMetricsRecording(t *testing.T) {
	m := newFeedMetrics()

	m.recordSubmit("BTC-USD", 6000012345678) // 60000.12345678
	// The feed price gauge is deliberately NOT set on submit: it publishes
	// together with onchain_price and price_delta from the same read-back
	// instant, so the three exported values are always coherent.
	if got := metricValue(t, m.price.WithLabelValues("BTC-USD")); got != 0 {
		t.Fatalf("price gauge set on submit = %v, want 0 until read-back", got)
	}
	if got := metricValue(t, m.submitted.WithLabelValues("BTC-USD")); got != 1 {
		t.Fatalf("submitted = %v, want 1", got)
	}
	m.recordOnChain("BTC-USD", big.NewInt(6000012345678))
	if got := metricValue(t, m.price.WithLabelValues("BTC-USD")); got != 60000.12345678 {
		t.Fatalf("price gauge after read-back = %v, want 60000.12345678", got)
	}
	if got := metricValue(t, m.priceDelta.WithLabelValues("BTC-USD")); got != 0 {
		t.Fatalf("delta with matching prices = %v, want 0", got)
	}

	m.recordEnqueued()
	m.recordEnqueued()
	if got := metricValue(t, m.inflight); got != 2 {
		t.Fatalf("inflight after 2 enqueues = %v, want 2", got)
	}

	m.recordConfirmed("BTC-USD", 150*time.Millisecond)
	if got := metricValue(t, m.inflight); got != 1 {
		t.Fatalf("inflight after 1 confirm = %v, want 1", got)
	}
	if got := metricValue(t, m.confirmed.WithLabelValues("BTC-USD")); got != 1 {
		t.Fatalf("confirmed = %v, want 1", got)
	}

	// The on-chain read-back exports the stored price and its delta against
	// the last submission.
	m.recordOnChain("BTC-USD", big.NewInt(6000000345678)) // 60000.00345678
	if got := metricValue(t, m.onchainPrice.WithLabelValues("BTC-USD")); got != 60000.00345678 {
		t.Fatalf("onchain price gauge = %v, want 60000.00345678", got)
	}
	delta := metricValue(t, m.priceDelta.WithLabelValues("BTC-USD"))
	if math.Abs(delta-0.12) > 1e-6 {
		t.Fatalf("price delta = %v, want ~0.12", delta)
	}
}

func TestPriceWalkStaysWithinBand(t *testing.T) {
	w := newPriceWalk()
	for range 10000 {
		btc := w.next("BTC-USD")
		if btc < btcBase-btcBand || btc > btcBase+btcBand {
			t.Fatalf("BTC price %d escaped band [%d, %d]", btc, btcBase-btcBand, btcBase+btcBand)
		}
		usdc := w.next("USDC-USD")
		if usdc < usdcBase-usdcBand || usdc > usdcBase+usdcBand {
			t.Fatalf("USDC price %d escaped band [%d, %d]", usdc, usdcBase-usdcBand, usdcBase+usdcBand)
		}
	}
}

func TestDirectFees(t *testing.T) {
	tests := []struct {
		name         string
		suggestedTip int64
		gasPrice     int64
		wantTip      int64
		wantFeeCap   int64
	}{
		// Idle or flood-only chain: the node suggests no tip, the floor keeps
		// the ordering guarantee.
		{"zero suggestion floors", 0, 1, 10, 20},
		// Doubling a below-floor suggestion still lands under the floor.
		{"tiny suggestion floors", 4, 25, 10, 260},
		// A congested chain's suggestion is tracked with the 2x premium.
		{"real congestion tracks suggestion", 1000, 5000, 2000, 52000},
	}
	for _, tc := range tests {
		tip, feeCap := directFees(big.NewInt(tc.suggestedTip), big.NewInt(tc.gasPrice))
		if tip.Int64() != tc.wantTip {
			t.Errorf("%s: tip = %s, want %d", tc.name, tip, tc.wantTip)
		}
		if feeCap.Int64() != tc.wantFeeCap {
			t.Errorf("%s: feeCap = %s, want %d", tc.name, feeCap, tc.wantFeeCap)
		}
	}
}
