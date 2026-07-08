package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// e2eLatency records, per tx, first-submit to first observation in an accepted
// block (observed in tracker.onMined, same place latSince is fed). Buckets are
// 5ms steps across the healthy 40-140ms band (targets: 70ms and 120ms), then a
// coarse tail out to 30s for failover proposer stalls.
var e2eLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name: "bombard_e2e_latency_seconds",
	Help: "Per-tx latency from first submit to first observation in an accepted block.",
	Buckets: append(prometheus.LinearBuckets(0.04, 0.005, 21),
		0.17, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.75, 1, 2, 5, 10, 30),
})

// serveMetrics exposes /metrics for the control-host Prometheus. The counters
// read the tracker's atomics at scrape time, so the send/mine hot paths are
// untouched. A bind failure (e.g. a second bombard) logs and continues.
func serveMetrics(addr string) {
	counter := func(name, help string, v interface{ Load() uint64 }) prometheus.CounterFunc {
		return prometheus.NewCounterFunc(prometheus.CounterOpts{Name: name, Help: help},
			func() float64 { return float64(v.Load()) })
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		e2eLatency,
		counter("bombard_txs_issued_total", "Nonces released by the issuer.", &track.issued),
		counter("bombard_txs_mined_total", "Txs observed included in a block.", &track.mined),
		counter("bombard_resubmits_total", "In-flight txs re-sent after the resubmit interval.", &track.resent),
	)
	go func() {
		if err := http.ListenAndServe(addr, promhttp.HandlerFor(reg, promhttp.HandlerOpts{})); err != nil {
			fmt.Fprintf(os.Stderr, "metrics: %v (continuing without /metrics)\n", err)
		}
	}()
}
