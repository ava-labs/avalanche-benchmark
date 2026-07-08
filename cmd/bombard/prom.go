package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// e2eLatency records, per tx, first-submit to first observation in an accepted
// block (observed in tracker.onMined, same place latSince is fed). Buckets
// stretch to 64s to cover failover proposer stalls.
var e2eLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "bombard_e2e_latency_seconds",
	Help:    "Per-tx latency from first submit to first observation in an accepted block.",
	Buckets: []float64{0.25, 0.5, 1, 2, 4, 8, 16, 32, 64},
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
