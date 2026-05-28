package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/rpc"
)

const (
	timeoutSLA    = 5 * time.Second
	latencyWindow = 10 * time.Second
)

type pendingEntry struct {
	sendStart   time.Time
	submittedAt time.Time
	workerID    int
	mined       time.Duration
	hasMined    bool
	confirm     time.Duration
	hasConfirm  bool
	observedAt  time.Time
}

type latencySample struct {
	observedAt time.Time
	send       time.Duration
	mined      time.Duration
	confirm    time.Duration
	total      time.Duration
}

type txTracker struct {
	mu        sync.Mutex
	pending   map[common.Hash]pendingEntry
	submitted uint64
	landed    uint64
	timeouts  uint64
	block     uint64
	hasBlock  bool
	latencies []time.Duration

	recent []latencySample
}

func newTxTracker() *txTracker {
	return &txTracker{pending: make(map[common.Hash]pendingEntry)}
}

func (t *txTracker) markSubmitted(h common.Hash, workerID int, sendStart, sendEnd time.Time) {
	t.mu.Lock()
	t.pending[h] = pendingEntry{sendStart: sendStart, submittedAt: sendEnd, workerID: workerID}
	t.submitted++
	t.mu.Unlock()
}

func (t *txTracker) markLanded(h common.Hash, blockTime time.Time, observedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.pending[h]
	if !ok {
		return
	}
	send := e.submittedAt.Sub(e.sendStart)
	mined := blockTime.Sub(e.submittedAt)
	if mined < 0 {
		mined = 0
	}
	confirm := observedAt.Sub(e.submittedAt)
	if confirm < 0 {
		confirm = 0
	}
	total := send + confirm
	t.recordLatencyLocked(latencySample{
		observedAt: observedAt,
		send:       send,
		mined:      mined,
		confirm:    confirm,
		total:      total,
	})
	t.landed++
	delete(t.pending, h)
}

func (t *txTracker) markMined(h common.Hash, blockTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.pending[h]
	if !ok {
		return
	}
	mined := blockTime.Sub(e.submittedAt)
	if mined < 0 {
		mined = 0
	}
	e.mined = mined
	e.hasMined = true
	t.pending[h] = e
	t.maybeFinalizeLocked(h, e)
}

func (t *txTracker) markConfirmed(h common.Hash, observedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.pending[h]
	if !ok {
		return
	}
	confirm := observedAt.Sub(e.submittedAt)
	if confirm < 0 {
		confirm = 0
	}
	e.confirm = confirm
	e.hasConfirm = true
	e.observedAt = observedAt
	t.pending[h] = e
	t.maybeFinalizeLocked(h, e)
}

func (t *txTracker) maybeFinalizeLocked(h common.Hash, e pendingEntry) {
	if !e.hasMined || !e.hasConfirm {
		return
	}
	send := e.submittedAt.Sub(e.sendStart)
	total := send + e.confirm
	observedAt := e.observedAt
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	t.recordLatencyLocked(latencySample{
		observedAt: observedAt,
		send:       send,
		mined:      e.mined,
		confirm:    e.confirm,
		total:      total,
	})
	t.landed++
	delete(t.pending, h)
}

func (t *txTracker) recordLatencyLocked(sample latencySample) {
	t.latencies = append(t.latencies, sample.total)
	t.recent = append(t.recent, sample)
	t.pruneLatencyWindowLocked(sample.observedAt)
}

func (t *txTracker) pruneLatencyWindowLocked(now time.Time) {
	cutoff := now.Add(-latencyWindow)
	keepFrom := 0
	for keepFrom < len(t.recent) && t.recent[keepFrom].observedAt.Before(cutoff) {
		keepFrom++
	}
	if keepFrom == 0 {
		return
	}
	copy(t.recent, t.recent[keepFrom:])
	clear(t.recent[len(t.recent)-keepFrom:])
	t.recent = t.recent[:len(t.recent)-keepFrom]
}

func (t *txTracker) snapshotLatencyWindow(now time.Time) ([]latencySample, uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLatencyWindowLocked(now)
	timeouts := t.timeouts
	out := make([]latencySample, len(t.recent))
	copy(out, t.recent)
	return out, timeouts
}

func (t *txTracker) setLatestBlock(block uint64) {
	t.mu.Lock()
	t.block = block
	t.hasBlock = true
	t.mu.Unlock()
}

func (t *txTracker) latestBlock() (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.block, t.hasBlock
}

func (t *txTracker) counts() (submitted, landed, timeouts uint64, pending int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.submitted, t.landed, t.timeouts, len(t.pending)
}

func fmtMs(d time.Duration) string {
	return fmt.Sprintf("%10d ms", d.Milliseconds())
}

func pctDur(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * (len(sorted) - 1) / 100
	return sorted[idx]
}

func meanDur(xs []time.Duration) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	var sum time.Duration
	for _, x := range xs {
		sum += x
	}
	return sum / time.Duration(len(xs))
}

func stddevDur(xs []time.Duration, mean time.Duration) time.Duration {
	if len(xs) < 2 {
		return 0
	}
	var sq float64
	m := float64(mean)
	for _, x := range xs {
		d := float64(x) - m
		sq += d * d
	}
	variance := sq / float64(len(xs)-1)
	return time.Duration(math.Sqrt(variance))
}

func (t *txTracker) printTableLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var prevLanded uint64
	prevAt := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		samples, timeouts := t.snapshotLatencyWindow(time.Now())
		if len(samples) == 0 {
			continue
		}

		t.mu.Lock()
		landed := t.landed
		t.mu.Unlock()
		now := time.Now()
		elapsed := now.Sub(prevAt).Seconds()
		var tps float64
		if elapsed > 0 {
			tps = float64(landed-prevLanded) / elapsed
		}
		prevLanded = landed
		prevAt = now

		sendXs := make([]time.Duration, len(samples))
		minedXs := make([]time.Duration, len(samples))
		confirmXs := make([]time.Duration, len(samples))
		totalXs := make([]time.Duration, len(samples))
		for i, s := range samples {
			sendXs[i] = s.send
			minedXs[i] = s.mined
			confirmXs[i] = s.confirm
			totalXs[i] = s.total
		}
		sortedSend := append([]time.Duration(nil), sendXs...)
		sortedMined := append([]time.Duration(nil), minedXs...)
		sortedConfirm := append([]time.Duration(nil), confirmXs...)
		sortedTotal := append([]time.Duration(nil), totalXs...)
		sort.Slice(sortedSend, func(i, j int) bool { return sortedSend[i] < sortedSend[j] })
		sort.Slice(sortedMined, func(i, j int) bool { return sortedMined[i] < sortedMined[j] })
		sort.Slice(sortedConfirm, func(i, j int) bool { return sortedConfirm[i] < sortedConfirm[j] })
		sort.Slice(sortedTotal, func(i, j int) bool { return sortedTotal[i] < sortedTotal[j] })

		meanSend := meanDur(sendXs)
		meanMined := meanDur(minedXs)
		meanConfirm := meanDur(confirmXs)
		meanTotal := meanDur(totalXs)

		fmt.Println()
		fmt.Println("═══════════════════════════════════════════════════════════════════════════════════════════════════════════════")
		fmt.Printf("  PERCENTILES (last 10s, samples=%d, timeouts=%d, tps=%.0f)\n", len(samples), timeouts, tps)
		fmt.Println("═══════════════════════════════════════════════════════════════════════════════════════════════════════════════")
		fmt.Println()
		fmt.Println("  ┌────────────────────┬───────────────┬───────────────┬───────────────┬───────────────┐")
		fmt.Println("  │ Metric             │  Send         │  Mined        │  Confirm      │  Total        │")
		fmt.Println("  ├────────────────────┼───────────────┼───────────────┼───────────────┼───────────────┤")
		fmt.Printf("  │ Min                │ %s │ %s │ %s │ %s │\n", fmtMs(sortedSend[0]), fmtMs(sortedMined[0]), fmtMs(sortedConfirm[0]), fmtMs(sortedTotal[0]))
		fmt.Printf("  │ Avg                │ %s │ %s │ %s │ %s │\n", fmtMs(meanSend), fmtMs(meanMined), fmtMs(meanConfirm), fmtMs(meanTotal))
		fmt.Printf("  │ Median (P50)       │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 50)), fmtMs(pctDur(sortedMined, 50)), fmtMs(pctDur(sortedConfirm, 50)), fmtMs(pctDur(sortedTotal, 50)))
		fmt.Printf("  │ P75                │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 75)), fmtMs(pctDur(sortedMined, 75)), fmtMs(pctDur(sortedConfirm, 75)), fmtMs(pctDur(sortedTotal, 75)))
		fmt.Printf("  │ P90                │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 90)), fmtMs(pctDur(sortedMined, 90)), fmtMs(pctDur(sortedConfirm, 90)), fmtMs(pctDur(sortedTotal, 90)))
		fmt.Printf("  │ P95                │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 95)), fmtMs(pctDur(sortedMined, 95)), fmtMs(pctDur(sortedConfirm, 95)), fmtMs(pctDur(sortedTotal, 95)))
		fmt.Printf("  │ P99                │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 99)), fmtMs(pctDur(sortedMined, 99)), fmtMs(pctDur(sortedConfirm, 99)), fmtMs(pctDur(sortedTotal, 99)))
		fmt.Printf("  │ Max                │ %s │ %s │ %s │ %s │\n", fmtMs(sortedSend[len(sortedSend)-1]), fmtMs(sortedMined[len(sortedMined)-1]), fmtMs(sortedConfirm[len(sortedConfirm)-1]), fmtMs(sortedTotal[len(sortedTotal)-1]))
		fmt.Printf("  │ Std Dev            │ %s │ %s │ %s │ %s │\n", fmtMs(stddevDur(sendXs, meanSend)), fmtMs(stddevDur(minedXs, meanMined)), fmtMs(stddevDur(confirmXs, meanConfirm)), fmtMs(stddevDur(totalXs, meanTotal)))
		fmt.Println("  └────────────────────┴───────────────┴───────────────┴───────────────┴───────────────┘")
		fmt.Println()
	}
}

func (t *txTracker) printFinalTable(w io.Writer, tps float64) {
	samples, timeouts := t.snapshotLatencyWindow(time.Now())

	sendXs := make([]time.Duration, len(samples))
	minedXs := make([]time.Duration, len(samples))
	confirmXs := make([]time.Duration, len(samples))
	totalXs := make([]time.Duration, len(samples))
	for i, s := range samples {
		sendXs[i] = s.send
		minedXs[i] = s.mined
		confirmXs[i] = s.confirm
		totalXs[i] = s.total
	}
	sortedSend := append([]time.Duration(nil), sendXs...)
	sortedMined := append([]time.Duration(nil), minedXs...)
	sortedConfirm := append([]time.Duration(nil), confirmXs...)
	sortedTotal := append([]time.Duration(nil), totalXs...)
	sort.Slice(sortedSend, func(i, j int) bool { return sortedSend[i] < sortedSend[j] })
	sort.Slice(sortedMined, func(i, j int) bool { return sortedMined[i] < sortedMined[j] })
	sort.Slice(sortedConfirm, func(i, j int) bool { return sortedConfirm[i] < sortedConfirm[j] })
	sort.Slice(sortedTotal, func(i, j int) bool { return sortedTotal[i] < sortedTotal[j] })

	if len(samples) == 0 {
		sortedSend = []time.Duration{0}
		sortedMined = []time.Duration{0}
		sortedConfirm = []time.Duration{0}
		sortedTotal = []time.Duration{0}
	}

	meanSend := meanDur(sendXs)
	meanMined := meanDur(minedXs)
	meanConfirm := meanDur(confirmXs)
	meanTotal := meanDur(totalXs)

	fmt.Fprintln(w, "═══════════════════════════════════════════════════════════════════════════════════════════════════════════════")
	fmt.Fprintf(w, "  PERCENTILES (last 10s, samples=%d, timeouts=%d, tps=%.0f)\n", len(samples), timeouts, tps)
	fmt.Fprintln(w, "═══════════════════════════════════════════════════════════════════════════════════════════════════════════════")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  ┌────────────────────┬───────────────┬───────────────┬───────────────┬───────────────┐")
	fmt.Fprintln(w, "  │ Metric             │  Send         │  Mined        │  Confirm      │  Total        │")
	fmt.Fprintln(w, "  ├────────────────────┼───────────────┼───────────────┼───────────────┼───────────────┤")
	fmt.Fprintf(w, "  │ Min                │ %s │ %s │ %s │ %s │\n", fmtMs(sortedSend[0]), fmtMs(sortedMined[0]), fmtMs(sortedConfirm[0]), fmtMs(sortedTotal[0]))
	fmt.Fprintf(w, "  │ Avg                │ %s │ %s │ %s │ %s │\n", fmtMs(meanSend), fmtMs(meanMined), fmtMs(meanConfirm), fmtMs(meanTotal))
	fmt.Fprintf(w, "  │ Median (P50)       │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 50)), fmtMs(pctDur(sortedMined, 50)), fmtMs(pctDur(sortedConfirm, 50)), fmtMs(pctDur(sortedTotal, 50)))
	fmt.Fprintf(w, "  │ P75                │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 75)), fmtMs(pctDur(sortedMined, 75)), fmtMs(pctDur(sortedConfirm, 75)), fmtMs(pctDur(sortedTotal, 75)))
	fmt.Fprintf(w, "  │ P90                │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 90)), fmtMs(pctDur(sortedMined, 90)), fmtMs(pctDur(sortedConfirm, 90)), fmtMs(pctDur(sortedTotal, 90)))
	fmt.Fprintf(w, "  │ P95                │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 95)), fmtMs(pctDur(sortedMined, 95)), fmtMs(pctDur(sortedConfirm, 95)), fmtMs(pctDur(sortedTotal, 95)))
	fmt.Fprintf(w, "  │ P99                │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 99)), fmtMs(pctDur(sortedMined, 99)), fmtMs(pctDur(sortedConfirm, 99)), fmtMs(pctDur(sortedTotal, 99)))
	fmt.Fprintf(w, "  │ Max                │ %s │ %s │ %s │ %s │\n", fmtMs(sortedSend[len(sortedSend)-1]), fmtMs(sortedMined[len(sortedMined)-1]), fmtMs(sortedConfirm[len(sortedConfirm)-1]), fmtMs(sortedTotal[len(sortedTotal)-1]))
	fmt.Fprintf(w, "  │ Std Dev            │ %s │ %s │ %s │ %s │\n", fmtMs(stddevDur(sendXs, meanSend)), fmtMs(stddevDur(minedXs, meanMined)), fmtMs(stddevDur(confirmXs, meanConfirm)), fmtMs(stddevDur(totalXs, meanTotal)))
	fmt.Fprintln(w, "  └────────────────────┴───────────────┴───────────────┴───────────────┴───────────────┘")
	fmt.Fprintln(w)
}

func (t *txTracker) timeoutLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.expireTimeouts(time.Now())
		}
	}
}

func (t *txTracker) expireTimeouts(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for h, e := range t.pending {
		if now.Sub(e.submittedAt) > timeoutSLA {
			delete(t.pending, h)
			t.timeouts++
		}
	}
}

func drainPending(ctx context.Context, maxWait time.Duration) {
	deadline := time.NewTimer(maxWait)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, _, _, pending := tracker.counts()
		if pending == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			tracker.expireTimeouts(time.Now())
			return
		case <-ticker.C:
			tracker.expireTimeouts(time.Now())
		}
	}
}

func (t *txTracker) reportLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		t.mu.Lock()
		now := time.Now()
		var expired []common.Hash
		for h, e := range t.pending {
			if now.Sub(e.submittedAt) > timeoutSLA {
				expired = append(expired, h)
			}
		}
		for _, h := range expired {
			e := t.pending[h]
			fmt.Printf("ERROR: tx %s worker=%d not mined after %.1fs\n",
				h.Hex(), e.workerID, now.Sub(e.submittedAt).Seconds())
			delete(t.pending, h)
			t.timeouts++
		}

		lats := t.latencies
		t.latencies = nil
		sub, land, to, pend := t.submitted, t.landed, t.timeouts, len(t.pending)
		t.mu.Unlock()

		if len(lats) > 0 {
			sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
			var sum time.Duration
			for _, l := range lats {
				sum += l
			}
			mean := sum / time.Duration(len(lats))
			p50 := lats[len(lats)/2]
			max := lats[len(lats)-1]
			fmt.Printf("STATS submitted=%d landed=%d timeouts=%d pending=%d | total-confirm-latency(last1s) n=%d mean=%v p50=%v max=%v\n",
				sub, land, to, pend, len(lats),
				mean.Round(time.Millisecond), p50.Round(time.Millisecond), max.Round(time.Millisecond))
		} else {
			fmt.Printf("STATS submitted=%d landed=%d timeouts=%d pending=%d | no landings this tick\n",
				sub, land, to, pend)
		}
	}
}

var tracker = newTxTracker()

const (
	// EWOQ is the pre-funded test key for Avalanche local networks
	ewoqPrivateKey = "56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027"

	// Transaction parameters
	defaultTps = 4000

	tickerTime  = 90 * time.Second // Interval between sends (mempool expires in 60s, so 90s ensures clean slate)
	workerDelay = 50 * time.Millisecond
	numWorkers  = int(tickerTime / workerDelay)

	gasLimitNative = 21000
	gasLimitERC20  = 65000
	gasPrice       = 25
)

var erc20Contract = common.HexToAddress("0xB0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5")

const (
	minTPS = 100
	maxTPS = 6000
)

type runConfig struct {
	rpcs        []string
	oneShot     bool
	duration    time.Duration
	startingTPS int
}

var verboseOutput bool

func progressf(format string, args ...any) {
	if verboseOutput {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func parseConfig(args []string) (runConfig, error) {
	fs := flag.NewFlagSet("bombard", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	rpcsRaw := fs.String("rpcs", "", "Comma-separated chain RPC URLs")
	timeRaw := fs.String("time", "", "One-shot run duration, e.g. 40s, 2m, 1m30s")
	startingTPS := fs.Int("starting-tps", defaultTps, "Starting target transactions per second")
	if err := fs.Parse(args); err != nil {
		return runConfig{}, err
	}
	if fs.NArg() != 0 {
		return runConfig{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}

	rpcs := parseRPCs(*rpcsRaw)
	if len(rpcs) == 0 {
		return runConfig{}, fmt.Errorf("--rpcs is required")
	}
	if *startingTPS < minTPS || *startingTPS > maxTPS {
		return runConfig{}, fmt.Errorf("--starting-tps must be between %d and %d", minTPS, maxTPS)
	}

	var timeSet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "time" {
			timeSet = true
		}
	})

	cfg := runConfig{
		rpcs:        rpcs,
		startingTPS: *startingTPS,
	}
	if !timeSet {
		return cfg, nil
	}
	if strings.TrimSpace(*timeRaw) == "" {
		return runConfig{}, fmt.Errorf("--time must be at least 1s when provided")
	}
	duration, err := time.ParseDuration(*timeRaw)
	if err != nil {
		return runConfig{}, fmt.Errorf("invalid --time: %w", err)
	}
	if duration < time.Second {
		return runConfig{}, fmt.Errorf("--time must be at least 1s")
	}
	cfg.oneShot = true
	cfg.duration = duration
	return cfg, nil
}

func parseRPCs(raw string) []string {
	parts := strings.Split(raw, ",")
	rpcs := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			rpcs = append(rpcs, part)
		}
	}
	return rpcs
}

func pickActiveRPC(ctx context.Context, rpcs []string) (int, *big.Int, error) {
	for i, rpcURL := range rpcs {
		chainID, err := probeChainID(ctx, rpcURL)
		if err == nil {
			return i, chainID, nil
		}
		progressf("RPC %s is down: %v\n", rpcURL, err)
	}
	return -1, nil, fmt.Errorf("no RPC URLs are alive")
}

func probeChainID(ctx context.Context, rpcURL string) (*big.Int, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	client, err := ethclient.DialContext(probeCtx, rpcURL)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.NetworkID(probeCtx)
}

type benchmarkRun struct {
	endpoints *endpointManager
	target    *targetRate

	watcherCtx  context.Context
	stopWatcher context.CancelFunc
	stopWorkers context.CancelFunc
	launchWG    sync.WaitGroup
	workerWG    sync.WaitGroup
	startedAt   time.Time
}

func startBenchmark(ctx context.Context, cfg runConfig, target *targetRate) (*benchmarkRun, error) {
	defaultWSConns := runtime.NumCPU() * 10
	activeIndex, chainID, err := pickActiveRPC(ctx, cfg.rpcs)
	if err != nil {
		return nil, err
	}

	endpoints, err := newEndpointManager(ctx, cfg.rpcs, activeIndex, defaultWSConns)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize active endpoint: %w", err)
	}
	cleanupEndpoints := true
	defer func() {
		if cleanupEndpoints {
			endpoints.Close()
		}
	}()

	activeWS := endpoints.activeWS()
	progressf("Opened %d WS connections to %s\n", defaultWSConns, activeWS)

	// Dedicated WS connection for setup work. Funding failure is a hard setup
	// error; failover is only part of the measured runtime path.
	setupRPC, err := rpc.DialWebsocket(ctx, activeWS, "")
	if err != nil {
		return nil, fmt.Errorf("failed to dial setup WS: %w", err)
	}
	defer setupRPC.Close()
	client := ethclient.NewClient(setupRPC)

	progressf("Chain ID: %s\n", chainID)

	privateKey, err := crypto.HexToECDSA(ewoqPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load key: %w", err)
	}
	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	progressf("Address: %s\n", address.Hex())

	signer := types.NewEIP155Signer(chainID)

	workerKeys, workerAddrs, err := DeriveWorkerKeys(privateKey, numWorkers)
	if err != nil {
		return nil, fmt.Errorf("failed to derive worker keys: %w", err)
	}

	progressf("\nChecking worker balances...\n")
	if err := FundWorkers(ctx, client, privateKey, signer, workerAddrs); err != nil {
		return nil, fmt.Errorf("failed to fund workers: %w", err)
	}

	progressf("Waiting for funding transactions...\n")
	time.Sleep(3 * time.Second)

	watcherCtx, stopWatcher := context.WithCancel(ctx)
	go endpoints.probeLoop(watcherCtx)
	go watchBlocksManaged(watcherCtx, endpoints, time.Millisecond)
	go tracker.timeoutLoop(watcherCtx)

	workerCtx, stopWorkers := context.WithCancel(ctx)
	run := &benchmarkRun{
		endpoints:   endpoints,
		target:      target,
		watcherCtx:  watcherCtx,
		stopWatcher: stopWatcher,
		stopWorkers: stopWorkers,
		startedAt:   time.Now(),
	}

	progressf("\nStarting %d workers (native): starting TPS %d, staggered by %v\n\n", numWorkers, target.get(), workerDelay)

	run.launchWG.Add(1)
	go func() {
		defer run.launchWG.Done()
		for i := 0; i < numWorkers; i++ {
			select {
			case <-workerCtx.Done():
				return
			default:
			}

			workerID := i + 1
			workerKey := workerKeys[i]
			workerAddr := workerAddrs[i]
			run.workerWG.Add(1)
			go func() {
				defer run.workerWG.Done()
				runWorker(workerCtx, endpoints, target, workerKey, signer, workerAddr, workerID, false)
			}()

			if i < numWorkers-1 {
				select {
				case <-workerCtx.Done():
					return
				case <-time.After(workerDelay):
				}
			}
		}
	}()

	cleanupEndpoints = false
	return run, nil
}

func (r *benchmarkRun) stop(drain time.Duration) {
	r.stopWorkers()
	r.launchWG.Wait()
	r.workerWG.Wait()
	if drain > 0 {
		drainPending(r.watcherCtx, drain)
	}
	r.stopWatcher()
	r.endpoints.Close()
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		fatalf("ERROR: %v", err)
	}
	verboseOutput = !cfg.oneShot

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	interrupted := make(chan os.Signal, 1)
	go func() {
		sig := <-sigCh
		select {
		case interrupted <- sig:
		default:
		}
		cancel()
	}()

	target := newTargetRate(cfg.startingTPS)
	run, err := startBenchmark(ctx, cfg, target)
	if err != nil {
		fatalf("ERROR: %v", err)
	}

	if !cfg.oneShot {
		if err := runTUI(ctx, cancel, run); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: TUI failed: %v\n", err)
		}
		run.stop(0)
		return
	}

	timer := time.NewTimer(cfg.duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	run.stop(2 * time.Second)

	wasInterrupted := false
	select {
	case <-interrupted:
		wasInterrupted = true
	default:
	}

	submitted, landed, timeouts, pending := tracker.counts()
	elapsed := cfg.duration
	if wasInterrupted {
		elapsed = time.Since(run.startedAt)
	}
	tps := 0.0
	if elapsed > 0 {
		tps = float64(landed) / elapsed.Seconds()
	}
	tracker.printFinalTable(os.Stdout, tps)
	fmt.Printf("FINAL submitted=%d landed=%d timeouts=%d pending=%d\n", submitted, landed, timeouts, pending)

	if wasInterrupted {
		os.Exit(130)
	}
}

func runWorker(
	ctx context.Context,
	endpoints *endpointManager,
	target *targetRate,
	privateKey *ecdsa.PrivateKey,
	signer types.Signer,
	address common.Address,
	workerID int,
	erc20 bool,
) {
	ticker := time.NewTicker(tickerTime)
	defer ticker.Stop()

	round := 0

	// Run immediately on start
	runWorkerRound(ctx, endpoints, target, privateKey, signer, address, workerID, &round, erc20)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runWorkerRound(ctx, endpoints, target, privateKey, signer, address, workerID, &round, erc20)
		}
	}
}

func runWorkerRound(
	ctx context.Context,
	endpoints *endpointManager,
	target *targetRate,
	privateKey *ecdsa.PrivateKey,
	signer types.Signer,
	address common.Address,
	workerID int,
	round *int,
	erc20 bool,
) {
	*round++
	batchSize := batchSizeForTPS(target.get())

	// Fetch nonce through the pool
	var nonce uint64
	err := endpoints.Do(ctx, func(c *ethclient.Client) error {
		var inner error
		nonce, inner = c.PendingNonceAt(ctx, address)
		return inner
	})
	if err != nil {
		progressf("[Worker %d] Failed to get nonce: %v\n", workerID, err)
		return
	}

	// Send batch (to self)
	_, errors := sendBatch(ctx, endpoints, privateKey, signer, address, address, nonce, batchSize, erc20, workerID)
	if errors > 0 {
		progressf("[Worker %d] Errors: %d\n", workerID, errors)
	}
}

// encodeERC20Transfer returns calldata for transfer(address,uint256)
func encodeERC20Transfer(to common.Address, amount *big.Int) []byte {
	data := make([]byte, 68)
	copy(data[0:4], []byte{0xa9, 0x05, 0x9c, 0xbb}) // transfer(address,uint256) selector
	copy(data[16:36], to.Bytes())                   // address padded to 32 bytes
	amount.FillBytes(data[36:68])                   // uint256
	return data
}

func sendBatch(
	ctx context.Context,
	endpoints *endpointManager,
	privateKey *ecdsa.PrivateKey,
	signer types.Signer,
	from, to common.Address,
	startNonce uint64,
	count int,
	erc20 bool,
	workerID int,
) (sent, errors int) {
	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var tx *types.Transaction
		if erc20 {
			data := encodeERC20Transfer(to, big.NewInt(1))
			tx = types.NewTransaction(
				startNonce+uint64(i),
				erc20Contract,
				big.NewInt(0),
				gasLimitERC20,
				big.NewInt(gasPrice),
				data,
			)
		} else {
			tx = types.NewTransaction(
				startNonce+uint64(i),
				to,
				big.NewInt(1), // 1 wei
				gasLimitNative,
				big.NewInt(gasPrice),
				nil,
			)
		}

		signed, err := types.SignTx(tx, signer, privateKey)
		if err != nil {
			errors++
			continue
		}

		sendStart := time.Now()
		err = endpoints.Do(ctx, func(c *ethclient.Client) error {
			return c.SendTransaction(ctx, signed)
		})
		if err != nil {
			errors++
			continue
		}
		sendEnd := time.Now()
		tracker.markSubmitted(signed.Hash(), workerID, sendStart, sendEnd)
		sent++
	}
	return
}
