package oraclerelay

import (
	"fmt"
	"math/big"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metricsNamespace prefixes every exported series as oracle_relay_*.
const metricsNamespace = "oracle_relay"

// MetricsListenAddress is the fixed, unconfigurable /metrics bind address. It is
// printed at startup so the operator knows where Grafana scrapes.
const MetricsListenAddress = "0.0.0.0:9700"

// priceDecimals is the fixed-point scale prices are submitted in (8 decimals).
var priceScale = big.NewFloat(1e8)

// metrics holds the relay's Prometheus collectors on a dedicated registry so no
// global state leaks between the relay and any test.
type metrics struct {
	registry           *prometheus.Registry
	price              *prometheus.GaugeVec
	priceUpdatedAt     *prometheus.GaugeVec
	seq                *prometheus.GaugeVec
	e2eLatency         *prometheus.HistogramVec
	pipelineLatency    *prometheus.HistogramVec
	delivered          *prometheus.CounterVec
	confirmed          *prometheus.CounterVec
	skipped            *prometheus.CounterVec
	inflight           prometheus.Gauge
	canonicalRefreshes prometheus.Counter
}

func newMetrics() *metrics {
	registry := prometheus.NewRegistry()
	m := &metrics{
		registry: registry,
		price: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "price",
			Help:      "Latest price scaled to whole units (raw / 1e8), by asset and chain.",
		}, []string{"asset", "chain"}),
		priceUpdatedAt: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "price_updated_at",
			Help:      "Unix seconds of the latest price, by asset and chain.",
		}, []string{"asset", "chain"}),
		seq: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "seq",
			Help:      "Last per-asset sequence number seen, by asset and chain. Only chain=\"oracle\" is exported: the receiver's latestPrice returns (price, updatedAt) with no seq.",
		}, []string{"asset", "chain"}),
		e2eLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Name:      "e2e_latency_seconds",
			Help:      "Delivery-confirmed time minus payload updatedAt, by asset.",
			Buckets:   []float64{0.25, 0.5, 1, 2, 3, 5, 8, 13, 21},
		}, []string{"asset"}),
		pipelineLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Name:      "pipeline_latency_seconds",
			Help:      "Delivery-confirmed time minus ws-event-seen time, by asset.",
			Buckets:   []float64{0.025, 0.05, 0.1, 0.2, 0.35, 0.5, 0.75, 1, 2, 5},
		}, []string{"asset"}),
		delivered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "delivered_total",
			Help:      "Deliveries sent to the main chain, by asset.",
		}, []string{"asset"}),
		confirmed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "confirmed_total",
			Help:      "Deliveries confirmed on the main chain, by asset.",
		}, []string{"asset"}),
		skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "skipped_stale_total",
			Help:      "Messages not delivered because their seq was not higher than an already-delivered one, by asset.",
		}, []string{"asset"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "inflight",
			Help:      "Sent-but-unconfirmed deliveries in the confirmer queue.",
		}),
		canonicalRefreshes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "canonical_set_refreshes_total",
			Help:      "Canonical validator-set fetches triggered by an epoch-height change.",
		}),
	}
	registry.MustRegister(
		m.price,
		m.priceUpdatedAt,
		m.seq,
		m.e2eLatency,
		m.pipelineLatency,
		m.delivered,
		m.confirmed,
		m.skipped,
		m.inflight,
		m.canonicalRefreshes,
	)
	return m
}

// serve binds the /metrics endpoint synchronously so a bind failure is fatal to
// the caller, then serves in the background.
func (m *metrics) serve(address string) error {
	return serveMetrics(m.registry, address)
}

// serveMetrics binds address up front (so a bind failure is fatal to the caller)
// and serves the registry's /metrics in the background. Shared by the relay and
// feed metrics servers.
func serveMetrics(registry *prometheus.Registry, address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("bind metrics endpoint %s: %w", address, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	return nil
}

// scaledPrice converts an 8-decimal fixed-point price to whole units.
func scaledPrice(raw *big.Int) float64 {
	value, _ := new(big.Float).Quo(new(big.Float).SetInt(raw), priceScale).Float64()
	return value
}

func (m *metrics) recordDelivery(asset string, price *big.Int, updatedAt uint64) {
	m.price.WithLabelValues(asset, "oracle").Set(scaledPrice(price))
	m.priceUpdatedAt.WithLabelValues(asset, "oracle").Set(float64(updatedAt))
	m.delivered.WithLabelValues(asset).Inc()
}

func (m *metrics) recordEnqueued() {
	m.inflight.Inc()
}

func (m *metrics) recordConfirmation(asset string, seenAt time.Time, updatedAt uint64, now time.Time) {
	m.confirmed.WithLabelValues(asset).Inc()
	m.e2eLatency.WithLabelValues(asset).Observe(now.Sub(time.Unix(int64(updatedAt), 0)).Seconds())
	m.pipelineLatency.WithLabelValues(asset).Observe(now.Sub(seenAt).Seconds())
	m.inflight.Dec()
}

func (m *metrics) recordMainPrice(asset string, price *big.Int, updatedAt uint64) {
	m.price.WithLabelValues(asset, "main").Set(scaledPrice(price))
	m.priceUpdatedAt.WithLabelValues(asset, "main").Set(float64(updatedAt))
}

// recordSeq exports the last delivered seq for an asset. Only chain="oracle" is
// set: the receiver's latestPrice view returns no seq, so a chain="main" seq
// would need a new contract view call and is deliberately not exported.
func (m *metrics) recordSeq(asset string, seq uint64) {
	m.seq.WithLabelValues(asset, "oracle").Set(float64(seq))
}

func (m *metrics) recordSkipped(asset string) {
	m.skipped.WithLabelValues(asset).Inc()
}

func (m *metrics) recordCanonicalRefresh() {
	m.canonicalRefreshes.Inc()
}
