// Command blockprobe is a tiny Prometheus exporter that derives L1 throughput
// and proposer-stall metrics straight from BLOCK DATA, the same way bombard's
// live TUI does — not from node counters scraped on a 5s interval.
//
// Why it exists: the obvious Grafana TPS query, rate(...txs_processed[1m]), is
// wrong on two counts. txs_processed counts processed transactions INCLUDING
// reorg replays (not txs that landed in accepted blocks), and a 1m rate over a
// 5s scrape is smoothed and lags ~30-60s — the opposite of realtime. blockprobe
// instead polls eth_getBlockByNumber on each site's RPC tracker, counts the
// transactions actually in each accepted block, and computes TPS over a short
// sliding window using the proposerVM millisecond block timestamps. The result
// is block-accurate and as realtime as the block cadence allows.
//
// It runs on the control host (the one box that survives both site outages and
// already reaches every node), probes the pinned non-validating RPC trackers on
// BOTH sites, labels every metric with site="a"/"b" so the dashboard reads as an
// A-vs-B overlay, and exposes /metrics for the local Prometheus to scrape.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	tps = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "l1_tps",
		Help: "Transactions per second derived from block contents over a sliding window of block timestamps (block-accurate, not scrape-sampled).",
	}, []string{"site"})
	gapLast = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "l1_block_gap_ms_last",
		Help: "Most recent inter-block gap in ms (consecutive proposerVM millisecond timestamps).",
	}, []string{"site"})
	gapMax = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "l1_block_gap_ms_max",
		Help: "Largest block gap in ms over the sliding window, folded with the live age of the latest block so an in-progress proposer stall shows immediately and persists for the window after recovery.",
	}, []string{"site"})
	blockAge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "l1_block_age_ms",
		Help: "Wall-clock ms since the latest block was observed. Grows live during a stall/failover; reads ~0 while a site is producing.",
	}, []string{"site"})
	height = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "l1_block_height",
		Help: "Highest block number observed on this site.",
	}, []string{"site"})
)

// downAfterMs: a site whose latest block is older than this is treated as DOWN
// (cordoned by a site-failover), not stalling — its gap/TPS series are broken
// rather than reporting an ever-growing block-age as a fictitious proposer stall.
// Set well above any legitimate cutover/stall (the worst consensus-timeout gap is
// ~5-10s) so a real, slow failback still shows fully; only a truly silent (dead)
// site is suppressed. The active site never goes quiet this long, so in practice
// this only ever hides the failed-away DC's unbounded climb.
const downAfterMs = 30000.0

// blockRec is one observed block kept in the sliding window.
type blockRec struct {
	num        uint64
	tsMs       uint64
	txs        int
	observedAt time.Time
}

// siteState aggregates the blocks seen by every watcher for one site, dedupes
// them by number (each RPC tracker sees the same chain), and recomputes the
// exported gauges. One per site, shared by that site's watchers.
type siteState struct {
	site   string
	window time.Duration

	mu     sync.Mutex
	blocks []blockRec // ascending by num, pruned to the window
	maxNum uint64
	lastAt time.Time
}

func newSiteState(site string, window time.Duration) *siteState {
	return &siteState{site: site, window: window}
}

// observe records a block if it advances the tip, then recomputes metrics.
// Blocks at or below the tip are duplicates from another watcher and ignored.
func (s *siteState) observe(num, tsMs uint64, txs int, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.blocks) > 0 && num <= s.maxNum {
		return
	}
	s.maxNum = num
	s.lastAt = at
	s.blocks = append(s.blocks, blockRec{num: num, tsMs: tsMs, txs: txs, observedAt: at})
	s.recomputeLocked(at)
}

// refresh recomputes metrics without a new block, so block age (and therefore a
// live stall) advances between blocks. Called on a ticker.
func (s *siteState) refresh(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.blocks) == 0 {
		return
	}
	s.recomputeLocked(now)
}

// recomputeLocked prunes the window and updates the gauges. Caller holds s.mu.
func (s *siteState) recomputeLocked(now time.Time) {
	cutoff := now.Add(-s.window)
	// Keep blocks within the window, but always retain the last two so a single
	// recent block (or a quiet site) still yields a defined last-gap.
	i := 0
	for i < len(s.blocks)-2 && s.blocks[i].observedAt.Before(cutoff) {
		i++
	}
	if i > 0 {
		s.blocks = append(s.blocks[:0], s.blocks[i:]...)
	}

	ageMs := float64(now.Sub(s.lastAt).Milliseconds())
	blockAge.WithLabelValues(s.site).Set(ageMs)
	height.WithLabelValues(s.site).Set(float64(s.maxNum))

	// A site silent past downAfterMs is DOWN (e.g. cordoned by a site-failover),
	// not merely stalling. Folding its ever-growing block-age into the max-gap
	// would climb unbounded (a 3-min "proposer stall") and blow out the gap panel,
	// burying the real cutover gap on the surviving site. So once a site crosses
	// that line, BREAK its gap/TPS series (DeleteLabelValues → the panel line ends,
	// reading as "down") instead of reporting a fictitious growing stall. blockAge
	// and height keep updating, so "site X frozen at block N for T" is still visible
	// on their own panels. The series resume automatically when the site produces
	// again. A real stall/cutover (sub-downAfterMs) still folds in live age below.
	if ageMs > downAfterMs {
		gapMax.DeleteLabelValues(s.site)
		gapLast.DeleteLabelValues(s.site)
		tps.WithLabelValues(s.site).Set(0)
		return
	}

	if len(s.blocks) < 2 {
		// Not enough history for a rate yet; report 0 TPS but still surface a
		// growing age as the live max-gap so an early stall isn't hidden.
		tps.WithLabelValues(s.site).Set(0)
		gapLast.WithLabelValues(s.site).Set(0)
		gapMax.WithLabelValues(s.site).Set(ageMs)
		return
	}

	first, last := s.blocks[0], s.blocks[len(s.blocks)-1]

	// TPS over the window: transactions accepted after the oldest block,
	// divided by the elapsed BLOCK time across the window (block-accurate).
	var txSum int
	for _, b := range s.blocks[1:] {
		txSum += b.txs
	}
	spanS := float64(last.tsMs-first.tsMs) / 1000.0
	if spanS > 0 {
		tps.WithLabelValues(s.site).Set(float64(txSum) / spanS)
	} else {
		tps.WithLabelValues(s.site).Set(0)
	}

	// Inter-block gaps from consecutive proposerVM timestamps.
	var maxGap uint64
	for j := 1; j < len(s.blocks); j++ {
		if g := s.blocks[j].tsMs - s.blocks[j-1].tsMs; g > maxGap {
			maxGap = g
		}
	}
	lastGap := s.blocks[len(s.blocks)-1].tsMs - s.blocks[len(s.blocks)-2].tsMs
	gapLast.WithLabelValues(s.site).Set(float64(lastGap))
	// Fold in the live age so an ongoing stall (no new block yet) shows now,
	// not only once the next block finally lands.
	liveMax := float64(maxGap)
	if ageMs > liveMax {
		liveMax = ageMs
	}
	gapMax.WithLabelValues(s.site).Set(liveMax)
}

// targets is a repeatable -target flag of the form site=rpcURL.
type targets []target

type target struct {
	site string
	url  string
}

func (t *targets) String() string { return "" }
func (t *targets) Set(v string) error {
	site, url, ok := strings.Cut(v, "=")
	if !ok || site == "" || url == "" {
		return errBadTarget
	}
	*t = append(*t, target{site: strings.TrimSpace(site), url: strings.TrimSpace(url)})
	return nil
}

var errBadTarget = &flagError{"want -target site=http://host:port/ext/bc/<chain>/rpc"}

type flagError struct{ msg string }

func (e *flagError) Error() string { return e.msg }

func main() {
	var tg targets
	flag.Var(&tg, "target", "Repeatable: site=rpcURL (e.g. a=http://10.0.0.1:9652/ext/bc/<chain>/rpc). Pass every RPC tracker; same-site endpoints are deduped.")
	listen := flag.String("listen", ":9101", "Address to serve Prometheus /metrics on.")
	poll := flag.Duration("poll", 250*time.Millisecond, "How often each watcher polls its node for new blocks.")
	window := flag.Duration("window", 5*time.Second, "Sliding window for the TPS rate and max-gap. Smaller = more realtime, noisier.")
	flag.Parse()

	if len(tg) == 0 {
		log.Fatal("blockprobe: no -target given")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One aggregator per distinct site; one watcher per target feeding it.
	sites := map[string]*siteState{}
	for _, t := range tg {
		st, ok := sites[t.site]
		if !ok {
			st = newSiteState(t.site, *window)
			sites[t.site] = st
		}
		go watchBlocks(ctx, t.url, st, *poll)
		log.Printf("blockprobe: watching site=%s %s", t.site, t.url)
	}

	// Refresh ticker so block age / live stall advance between blocks.
	go func() {
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tk.C:
				for _, st := range sites {
					st.refresh(now)
				}
			}
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: *listen, Handler: mux}

	go func() {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
		<-sigc
		cancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Printf("blockprobe: serving metrics on %s (%d target(s), window %s)", *listen, len(tg), *window)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("blockprobe: %v", err)
	}
}
