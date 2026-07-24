package oraclerelay

import (
	"testing"

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
	if got := metricValue(t, m.price.WithLabelValues("BTC-USD")); got != 60000.12345678 {
		t.Fatalf("price gauge = %v, want 60000.12345678", got)
	}
	if got := metricValue(t, m.submitted.WithLabelValues("BTC-USD")); got != 1 {
		t.Fatalf("submitted = %v, want 1", got)
	}

	m.recordEnqueued()
	m.recordEnqueued()
	if got := metricValue(t, m.inflight); got != 2 {
		t.Fatalf("inflight after 2 enqueues = %v, want 2", got)
	}

	m.recordConfirmed("BTC-USD")
	if got := metricValue(t, m.inflight); got != 1 {
		t.Fatalf("inflight after 1 confirm = %v, want 1", got)
	}
	if got := metricValue(t, m.confirmed.WithLabelValues("BTC-USD")); got != 1 {
		t.Fatalf("confirmed = %v, want 1", got)
	}
}

func TestPriceWalkStaysWithinBand(t *testing.T) {
	w := newPriceWalk()
	for range 10000 {
		btc := w.next("BTC-USD")
		if btc < btcBase-btcBand || btc > btcBase+btcBand {
			t.Fatalf("BTC price %d escaped band [%d, %d]", btc, btcBase-btcBand, btcBase+btcBand)
		}
		avax := w.next("AVAX-USD")
		if avax < avaxBase-avaxBand || avax > avaxBase+avaxBand {
			t.Fatalf("AVAX price %d escaped band [%d, %d]", avax, avaxBase-avaxBand, avaxBase+avaxBand)
		}
	}
}
