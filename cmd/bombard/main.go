package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/rpc"
)

// Single-issuer bombard.
//
// One key, one strictly increasing nonce — a production-shaped workload (a
// single web2->web3 gateway issuing transactions in order). Two governors:
//
//  1. Rate limiter: a per-second budget. Every 1ms we may have sent up to
//     tps/1000 * msElapsedThisSecond txs; the budget resets each wall second.
//  2. In-flight cap: we never let ourselves get more than `inflight` nonces
//     ahead of the last-mined nonce. Hitting the cap is the backpressure /
//     "falling behind" signal — there is no timeout counter.
//
// Resilience: a tx leaves the system only when its nonce mines. Anything still
// in flight after the resubmit interval is re-sent verbatim (same bytes, same
// hash) to survive mempool loss from node overload or a crash. "already known"
// / "nonce too low" send errors are benign no-ops.

const (
	// EWOQ is the pre-funded test key for Avalanche local networks.
	ewoqPrivateKey = "56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027"

	gasLimitNative = 21000
	gasPrice       = 25

	ringBufferSize = 100000
)

type latencySample struct {
	mined time.Duration // first-send -> block timestamp
	total time.Duration // first-send -> observed by watcher
}

type txState struct {
	signed    *types.Transaction
	firstSend time.Time
	lastSend  time.Time
	resubmits int
}

// tracker is the single source of truth. issued/mined are atomic counters so
// the issuer can check the in-flight cap without taking the lock on every tick;
// the map and ring buffer are guarded by mu.
type tracker struct {
	mu       sync.Mutex
	inflight map[uint64]*txState     // nonce -> state, present until mined
	byHash   map[common.Hash]uint64 // tx hash -> nonce, for the watcher

	issued atomic.Uint64 // nonces released by the issuer
	mined  atomic.Uint64 // nonces observed in a block
	resent atomic.Uint64 // resubmissions

	latSince []time.Duration // total latencies since the last report tick
	ring     [ringBufferSize]latencySample
	ringHead int
	ringFull bool
}

func newTracker() *tracker {
	return &tracker{
		inflight: make(map[uint64]*txState),
		byHash:   make(map[common.Hash]uint64),
	}
}

// register records a freshly-issued tx. issued is bumped by the issuer (so the
// cap is accurate even while a tx waits in the send queue); register only fills
// the map so the watcher and resubmitter can find it.
func (t *tracker) register(nonce uint64, st *txState) {
	t.mu.Lock()
	t.inflight[nonce] = st
	t.byHash[st.signed.Hash()] = nonce
	t.mu.Unlock()
}

func (t *tracker) onMined(hash common.Hash, blockTime, observedAt time.Time) {
	t.mu.Lock()
	nonce, ok := t.byHash[hash]
	if !ok {
		t.mu.Unlock()
		return
	}
	st := t.inflight[nonce]
	delete(t.inflight, nonce)
	delete(t.byHash, hash)

	mined := blockTime.Sub(st.firstSend)
	if mined < 0 {
		mined = 0
	}
	total := observedAt.Sub(st.firstSend)
	if total < 0 {
		total = 0
	}
	t.latSince = append(t.latSince, total)
	t.ring[t.ringHead] = latencySample{mined: mined, total: total}
	t.ringHead++
	if t.ringHead >= ringBufferSize {
		t.ringHead = 0
		t.ringFull = true
	}
	t.mu.Unlock()

	t.mined.Add(1)
}

// inFlight is how many nonces we are ahead of the last-mined nonce.
func (t *tracker) inFlight() uint64 {
	return t.issued.Load() - t.mined.Load()
}

// dueForResubmit returns the signed txs still in flight whose last send is
// older than the interval, and stamps them as resent now.
func (t *tracker) dueForResubmit(interval time.Duration, now time.Time) []*types.Transaction {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*types.Transaction
	for _, st := range t.inflight {
		if now.Sub(st.lastSend) >= interval {
			st.lastSend = now
			st.resubmits++
			out = append(out, st.signed)
		}
	}
	t.resent.Add(uint64(len(out)))
	return out
}

func (t *tracker) snapshotRing() []latencySample {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.ringFull {
		out := make([]latencySample, t.ringHead)
		copy(out, t.ring[:t.ringHead])
		return out
	}
	out := make([]latencySample, ringBufferSize)
	copy(out, t.ring[t.ringHead:])
	copy(out[ringBufferSize-t.ringHead:], t.ring[:t.ringHead])
	return out
}

var track = newTracker()

const (
	// in-flight cap = rps / inflightDivisor nonces ahead of last-mined. A
	// divisor of 5 stops issuance once in-flight represents ~1/5s of latency,
	// providing early backpressure before a resubmit storm can build.
	inflightDivisor = 5
	// resubmitInterval re-sends any tx still in flight after this long, to
	// survive mempool loss from node overload or a crash.
	resubmitInterval = time.Second
	// pollInterval is how often each watcher asks its node for new blocks.
	pollInterval = time.Millisecond
)

func main() {
	rpcFlag := flag.String("rpc", "", "Comma-separated RPC URLs (auto-detected from network_data/rpcs.txt if omitted). Sends fan out across all; watchers race across all.")
	rps := flag.Int("rps", 4000, "Target transactions issued per second")
	targetTxs := flag.Uint64("txs", 0, "Stop after at least this many mined txs; 0 means run until interrupted")
	runDuration := flag.Duration("duration", 0, "Stop after this duration; 0 means run until interrupted or --txs is reached")
	flag.Parse()

	if *rps <= 0 {
		fmt.Println("--rps must be > 0")
		os.Exit(1)
	}
	cap := *rps / inflightDivisor
	if cap < 1 {
		cap = 1
	}

	// Resolve the RPC endpoint list.
	rpcURLs := splitNonEmpty(*rpcFlag)
	if len(rpcURLs) == 0 {
		rpcsFile := filepath.Join("./network_data", "rpcs.txt")
		data, err := os.ReadFile(rpcsFile)
		if err != nil {
			fmt.Printf("No --rpc provided and failed to read %s: %v\n", rpcsFile, err)
			os.Exit(1)
		}
		rpcURLs = splitNonEmpty(string(data))
		if len(rpcURLs) == 0 {
			fmt.Printf("No RPC URLs found in %s\n", rpcsFile)
			os.Exit(1)
		}
		fmt.Printf("Auto-detected %d RPC URL(s) from %s\n", len(rpcURLs), rpcsFile)
	}
	wsURLs := make([]string, len(rpcURLs))
	for i, u := range rpcURLs {
		wsURLs[i] = httpRPCToWS(u)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { os.Exit(signalExitCode(<-sigCh)) }()

	if *runDuration > 0 {
		go func() {
			timer := time.NewTimer(*runDuration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
			case <-timer.C:
				fmt.Printf("Duration target reached: %s\n", runDuration.String())
				cancel()
			}
		}()
	}
	if *targetTxs > 0 {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				if track.mined.Load() >= *targetTxs {
					fmt.Printf("Transaction target reached: mined=%d target=%d\n", track.mined.Load(), *targetTxs)
					cancel()
					return
				}
			}
		}()
	}

	// Send pool: connections spread across every endpoint so sends fan out.
	wsConns := runtime.NumCPU() * 10
	pool, err := newWSPool(ctx, wsURLs, wsConns)
	if err != nil {
		fmt.Printf("Failed to open WS pool: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()
	fmt.Printf("Opened %d WS connections across %d endpoint(s)\n", len(pool.rpcs), len(wsURLs))

	// One watcher per endpoint; first to see a tx in a block records it.
	for _, ws := range wsURLs {
		watcherRPC, err := rpc.DialWebsocket(ctx, ws, "")
		if err != nil {
			fmt.Printf("Failed to dial watcher WS %s: %v\n", ws, err)
			os.Exit(1)
		}
		defer watcherRPC.Close()
		go watchBlocks(ctx, watcherRPC, pollInterval)
	}

	// Setup connection: chain ID + start nonce.
	setupRPC, err := rpc.DialWebsocket(ctx, wsURLs[0], "")
	if err != nil {
		fmt.Printf("Failed to dial setup WS: %v\n", err)
		os.Exit(1)
	}
	defer setupRPC.Close()
	client := ethclient.NewClient(setupRPC)

	chainID, err := client.NetworkID(ctx)
	if err != nil {
		fmt.Printf("Failed to get chain ID: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Chain ID: %s\n", chainID)

	privateKey, err := crypto.HexToECDSA(ewoqPrivateKey)
	if err != nil {
		fmt.Printf("Failed to load key: %v\n", err)
		os.Exit(1)
	}
	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	signer := types.NewEIP155Signer(chainID)

	startNonce, err := client.PendingNonceAt(ctx, address)
	if err != nil {
		fmt.Printf("Failed to get nonce: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Issuer: %s  start nonce: %d\n", address.Hex(), startNonce)

	go reportLoop(ctx, *rps, cap)
	go printTableLoop(ctx)
	go resubmitLoop(ctx, pool, resubmitInterval)

	fmt.Printf("\nSingle issuer: target %d rps, in-flight cap %d nonces, resubmit after %s\n\n",
		*rps, cap, resubmitInterval.String())

	// Send workers sign + submit nonces handed to them by the issuer. Signing
	// is spread across all these goroutines (nproc cores) so it never gates the
	// issuer's 1ms cadence.
	sendCh := make(chan uint64, cap)
	var wg sync.WaitGroup
	for i := 0; i < wsConns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendWorker(ctx, pool, sendCh, privateKey, signer, address)
		}()
	}

	go issuer(ctx, sendCh, *rps, uint64(cap), startNonce)

	<-ctx.Done()
	close(sendCh)
	wg.Wait()
	fmt.Printf("FINAL issued=%d mined=%d inflight=%d resubmits=%d\n",
		track.issued.Load(), track.mined.Load(), track.inFlight(), track.resent.Load())
}

// issuer releases nonces under the per-second rate budget and the in-flight cap.
func issuer(ctx context.Context, sendCh chan<- uint64, tps int, cap, startNonce uint64) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	nextNonce := startNonce
	curSec := time.Now().Truncate(time.Second)
	var sentThisSecond int64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := time.Now()
		if sec := now.Truncate(time.Second); sec != curSec {
			curSec = sec
			sentThisSecond = 0
		}
		msPassed := now.Sub(curSec).Milliseconds()
		allowed := int64(tps) * msPassed / 1000

		for sentThisSecond < allowed && track.inFlight() < cap {
			select {
			case sendCh <- nextNonce:
				track.issued.Add(1)
				nextNonce++
				sentThisSecond++
			case <-ctx.Done():
				return
			default:
				// Send workers are saturated; let pressure build and retry next tick.
				sentThisSecond = allowed
			}
		}
	}
}

func sendWorker(
	ctx context.Context,
	pool *wsPool,
	sendCh <-chan uint64,
	key *ecdsa.PrivateKey,
	signer types.Signer,
	address common.Address,
) {
	for nonce := range sendCh {
		tx := types.NewTransaction(nonce, address, big.NewInt(1), gasLimitNative, big.NewInt(gasPrice), nil)
		signed, err := types.SignTx(tx, signer, key)
		if err != nil {
			fmt.Printf("sign nonce %d: %v\n", nonce, err)
			continue
		}
		now := time.Now()
		track.register(nonce, &txState{signed: signed, firstSend: now, lastSend: now})
		// A failed first send is fine: the tx stays in flight and the resubmit
		// loop will re-send it. We only drop it from accounting when it mines.
		_ = pool.Do(ctx, func(c *ethclient.Client) error { return c.SendTransaction(ctx, signed) })
	}
}

func resubmitLoop(ctx context.Context, pool *wsPool, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, signed := range track.dueForResubmit(interval, time.Now()) {
			s := signed
			err := pool.Do(ctx, func(c *ethclient.Client) error { return c.SendTransaction(ctx, s) })
			if err != nil && !benignSendErr(err) {
				// Real send failure; the tx is still in flight and will be
				// retried on the next tick.
				continue
			}
		}
	}
}

// benignSendErr reports send errors that mean the tx is already accepted or
// already mined — expected when resubmitting an identical transaction.
func benignSendErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already known") ||
		strings.Contains(s, "known transaction") ||
		strings.Contains(s, "already exists") ||
		strings.Contains(s, "nonce too low")
}

func reportLoop(ctx context.Context, tps, cap int) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	prevMined := track.mined.Load()
	prevAt := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		track.mu.Lock()
		lats := track.latSince
		track.latSince = nil
		track.mu.Unlock()

		mined := track.mined.Load()
		now := time.Now()
		elapsed := now.Sub(prevAt).Seconds()
		var minedTps float64
		if elapsed > 0 {
			minedTps = float64(mined-prevMined) / elapsed
		}
		prevMined = mined
		prevAt = now

		inflight := track.inFlight()
		behind := ""
		if inflight >= uint64(cap) {
			behind = " AT-CAP(behind)"
		}

		if len(lats) > 0 {
			sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
			fmt.Printf("STATS issued=%d mined=%d inflight=%d/%d resubmits=%d minedTps=%.0f/%d%s | total p50=%v p95=%v p99=%v\n",
				track.issued.Load(), mined, inflight, cap, track.resent.Load(), minedTps, tps, behind,
				pctDur(lats, 50).Round(time.Millisecond), pctDur(lats, 95).Round(time.Millisecond), pctDur(lats, 99).Round(time.Millisecond))
		} else {
			fmt.Printf("STATS issued=%d mined=%d inflight=%d/%d resubmits=%d minedTps=%.0f/%d%s | no landings this tick\n",
				track.issued.Load(), mined, inflight, cap, track.resent.Load(), minedTps, tps, behind)
		}
	}
}

func printTableLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		samples := track.snapshotRing()
		if len(samples) == 0 {
			continue
		}
		minedXs := make([]time.Duration, len(samples))
		totalXs := make([]time.Duration, len(samples))
		for i, s := range samples {
			minedXs[i] = s.mined
			totalXs[i] = s.total
		}
		sortedMined := append([]time.Duration(nil), minedXs...)
		sortedTotal := append([]time.Duration(nil), totalXs...)
		sort.Slice(sortedMined, func(i, j int) bool { return sortedMined[i] < sortedMined[j] })
		sort.Slice(sortedTotal, func(i, j int) bool { return sortedTotal[i] < sortedTotal[j] })

		fmt.Println()
		fmt.Printf("  PERCENTILES (last %d mined)\n", len(samples))
		fmt.Println("  ┌────────────────────┬───────────────┬───────────────┐")
		fmt.Println("  │ Metric             │  Mined        │  Total        │")
		fmt.Println("  ├────────────────────┼───────────────┼───────────────┤")
		fmt.Printf("  │ Min                │ %s │ %s │\n", fmtMs(sortedMined[0]), fmtMs(sortedTotal[0]))
		fmt.Printf("  │ Avg                │ %s │ %s │\n", fmtMs(meanDur(minedXs)), fmtMs(meanDur(totalXs)))
		fmt.Printf("  │ Median (P50)       │ %s │ %s │\n", fmtMs(pctDur(sortedMined, 50)), fmtMs(pctDur(sortedTotal, 50)))
		fmt.Printf("  │ P75                │ %s │ %s │\n", fmtMs(pctDur(sortedMined, 75)), fmtMs(pctDur(sortedTotal, 75)))
		fmt.Printf("  │ P90                │ %s │ %s │\n", fmtMs(pctDur(sortedMined, 90)), fmtMs(pctDur(sortedTotal, 90)))
		fmt.Printf("  │ P95                │ %s │ %s │\n", fmtMs(pctDur(sortedMined, 95)), fmtMs(pctDur(sortedTotal, 95)))
		fmt.Printf("  │ P99                │ %s │ %s │\n", fmtMs(pctDur(sortedMined, 99)), fmtMs(pctDur(sortedTotal, 99)))
		fmt.Printf("  │ Max                │ %s │ %s │\n", fmtMs(sortedMined[len(sortedMined)-1]), fmtMs(sortedTotal[len(sortedTotal)-1]))
		fmt.Println("  └────────────────────┴───────────────┴───────────────┘")
		fmt.Println()
	}
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

// splitNonEmpty splits on commas and whitespace, dropping empty fields.
func splitNonEmpty(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	return fields
}

func signalExitCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}
