package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"math"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
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
	timeoutSLA     = 5 * time.Second
	ringBufferSize = 100000
)

type pendingEntry struct {
	sendStart   time.Time
	submittedAt time.Time
	workerID    int
	mined       time.Duration
	hasMined    bool
	confirm     time.Duration
	hasConfirm  bool
}

type latencySample struct {
	send    time.Duration
	mined   time.Duration
	confirm time.Duration
	total   time.Duration
}

type txTracker struct {
	mu        sync.Mutex
	pending   map[common.Hash]pendingEntry
	submitted uint64
	landed    uint64
	timeouts  uint64
	latencies []time.Duration

	ring     [ringBufferSize]latencySample
	ringHead int
	ringFull bool
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
	t.latencies = append(t.latencies, total)
	t.ring[t.ringHead] = latencySample{send: send, mined: mined, confirm: confirm, total: total}
	t.ringHead++
	if t.ringHead >= ringBufferSize {
		t.ringHead = 0
		t.ringFull = true
	}
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
	t.pending[h] = e
	t.maybeFinalizeLocked(h, e)
}

func (t *txTracker) maybeFinalizeLocked(h common.Hash, e pendingEntry) {
	if !e.hasMined || !e.hasConfirm {
		return
	}
	send := e.submittedAt.Sub(e.sendStart)
	total := send + e.confirm
	t.latencies = append(t.latencies, total)
	t.ring[t.ringHead] = latencySample{send: send, mined: e.mined, confirm: e.confirm, total: total}
	t.ringHead++
	if t.ringHead >= ringBufferSize {
		t.ringHead = 0
		t.ringFull = true
	}
	t.landed++
	delete(t.pending, h)
}

func (t *txTracker) snapshotRing() ([]latencySample, uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	timeouts := t.timeouts
	if !t.ringFull {
		out := make([]latencySample, t.ringHead)
		copy(out, t.ring[:t.ringHead])
		return out, timeouts
	}
	out := make([]latencySample, ringBufferSize)
	copy(out, t.ring[t.ringHead:])
	copy(out[ringBufferSize-t.ringHead:], t.ring[:t.ringHead])
	return out, timeouts
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

func signalExitCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
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

		samples, timeouts := t.snapshotRing()
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
		fmt.Printf("  PERCENTILES (last %d TXs, timeouts=%d, tps=%.0f)\n", len(samples), timeouts, tps)
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

func main() {
	rpcURL := flag.String("rpc", "", "RPC URL for submitting txs (auto-detected from network_data/rpcs.txt if omitted)")
	wsURL := flag.String("ws", "", "WebSocket URL for submitting txs (auto-derived from --rpc if omitted)")
	watchRPCURL := flag.String("watch-rpc", "", "RPC URL for the block watcher (defaults to --rpc; set to a DC2 follower to measure what a failover-side client sees)")
	watchWSURL := flag.String("watch-ws", "", "WebSocket URL for the block watcher (auto-derived from --watch-rpc if omitted)")
	defaultWSConns := runtime.NumCPU() * 10
	wsConns := flag.Int("ws-conns", defaultWSConns, "Number of WebSocket connections in the worker pool")
	watchInterval := flag.Duration("watch-interval", time.Millisecond, "How often the watcher polls for new blocks")
	confirmSource := flag.String("confirm-source", "block", "Confirmation source: block or accepted-sub")
	targetTps := flag.Int("tps", defaultTps, "Target transactions per second")
	targetTxs := flag.Uint64("txs", 0, "Stop after at least this many landed transactions; 0 means run until interrupted")
	runDuration := flag.Duration("duration", 0, "Stop after this duration; 0 means run until interrupted or --txs is reached")
	erc20Mode := flag.Bool("erc20", false, "Send ERC20 transfers instead of native transfers")
	dataDir := flag.String("data-dir", "./network_data", "Network data directory (for auto-detecting RPC URL)")
	flag.Parse()
	if *confirmSource != "block" && *confirmSource != "accepted-sub" {
		fmt.Printf("Invalid --confirm-source=%q; expected block or accepted-sub\n", *confirmSource)
		os.Exit(1)
	}

	if *rpcURL == "" {
		rpcsFile := filepath.Join(*dataDir, "rpcs.txt")
		data, err := os.ReadFile(rpcsFile)
		if err != nil {
			fmt.Printf("No --rpc provided and failed to read %s: %v\n", rpcsFile, err)
			os.Exit(1)
		}
		urls := strings.Split(strings.TrimSpace(string(data)), ",")
		if len(urls) == 0 || urls[0] == "" {
			fmt.Printf("No RPC URLs found in %s\n", rpcsFile)
			os.Exit(1)
		}
		*rpcURL = urls[0]
		fmt.Printf("Auto-detected RPC URL from %s\n", rpcsFile)
	}
	if *wsURL == "" {
		*wsURL = httpRPCToWS(*rpcURL)
	}
	if *watchRPCURL == "" {
		*watchRPCURL = *rpcURL
	}
	if *watchWSURL == "" {
		*watchWSURL = httpRPCToWS(*watchRPCURL)
	}

	// Calculate batch size based on target TPS
	batchSize := *targetTps * int(workerDelay/time.Millisecond) / 1000

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		os.Exit(signalExitCode(<-sigCh))
	}()

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

				_, landed, _, _ := tracker.counts()
				if landed >= *targetTxs {
					fmt.Printf("Transaction target reached: landed=%d target=%d\n", landed, *targetTxs)
					cancel()
					return
				}
			}
		}()
	}

	// Worker pool: fixed number of WS connections, each held exclusively for
	// one in-flight call at a time. Pool size is the rate limit; if the node
	// slows down, response time grows, fewer requests get through, producers
	// block — natural backpressure.
	pool, err := newWSPool(ctx, *wsURL, *wsConns)
	if err != nil {
		fmt.Printf("Failed to open WS pool: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()
	fmt.Printf("Opened %d WS connections to %s\n", *wsConns, *wsURL)

	// Dedicated WS connection for the block watcher (long-lived polling — must
	// not steal capacity from the worker pool). May target a different node
	// than the submission pool (e.g., bombard a DC1 validator while watching
	// a DC2 follower) so we can measure end-to-end latency from the failover
	// observer's vantage point.
	watcherRPC, err := rpc.DialWebsocket(ctx, *watchWSURL, "")
	if err != nil {
		fmt.Printf("Failed to dial watcher WS: %v\n", err)
		os.Exit(1)
	}
	defer watcherRPC.Close()
	if *watchWSURL != *wsURL {
		fmt.Printf("Watcher WS: %s (separate from submission pool)\n", *watchWSURL)
	}

	// Dedicated WS connection for one-shot setup work (chain ID, balances,
	// funding). Low volume, short-lived; keeping it off the pool simplifies
	// the funding code which still takes *ethclient.Client.
	setupRPC, err := rpc.DialWebsocket(ctx, *wsURL, "")
	if err != nil {
		fmt.Printf("Failed to dial setup WS: %v\n", err)
		os.Exit(1)
	}
	defer setupRPC.Close()
	client := ethclient.NewClient(setupRPC)

	// Get chain ID
	chainID, err := client.NetworkID(ctx)
	if err != nil {
		fmt.Printf("Failed to get chain ID: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Chain ID: %s\n", chainID)

	// Load key
	privateKey, err := crypto.HexToECDSA(ewoqPrivateKey)
	if err != nil {
		fmt.Printf("Failed to load key: %v\n", err)
		os.Exit(1)
	}
	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	fmt.Printf("Address: %s\n", address.Hex())

	signer := types.NewEIP155Signer(chainID)

	// Derive worker keys
	workerKeys, workerAddrs, err := DeriveWorkerKeys(privateKey, numWorkers)
	if err != nil {
		fmt.Printf("Failed to derive worker keys: %v\n", err)
		os.Exit(1)
	}

	// Fund workers that need it
	fmt.Println("\nChecking worker balances...")
	err = FundWorkers(ctx, client, privateKey, signer, workerAddrs)
	if err != nil {
		fmt.Printf("Failed to fund workers: %v\n", err)
		os.Exit(1)
	}

	// Fund workers with ERC20 tokens if in ERC20 mode
	if *erc20Mode {
		fmt.Println("Checking worker ERC20 balances...")
		err = FundWorkersERC20(ctx, client, privateKey, signer, workerAddrs, erc20Contract)
		if err != nil {
			fmt.Printf("Failed to fund workers with ERC20: %v\n", err)
			os.Exit(1)
		}
	}

	// Wait for funding txs to be mined
	fmt.Println("Waiting for funding transactions...")
	time.Sleep(3 * time.Second)

	// Start block watcher and stats reporters
	go watchBlocks(ctx, watcherRPC, *watchInterval, *confirmSource == "block")
	if *confirmSource == "accepted-sub" {
		go watchAcceptedTransactions(ctx, watcherRPC)
	}
	go tracker.reportLoop(ctx)
	go tracker.printTableLoop(ctx)

	mode := "native"
	if *erc20Mode {
		mode = "ERC20"
	}
	fmt.Printf("\nStarting %d workers (%s): send %d txs every %v, staggered by %v\n\n", numWorkers, mode, batchSize, tickerTime, workerDelay)

	// Start workers with staggered delays
	for i := 0; i < numWorkers; i++ {
		workerID := i + 1
		go runWorker(ctx, pool, workerKeys[i], signer, workerAddrs[i], workerID, batchSize, *erc20Mode)
		if i < numWorkers-1 {
			time.Sleep(workerDelay)
		}
	}

	// Wait for context cancellation
	<-ctx.Done()
	submitted, landed, timeouts, pending := tracker.counts()
	fmt.Printf("FINAL submitted=%d landed=%d timeouts=%d pending=%d\n", submitted, landed, timeouts, pending)
}

func runWorker(
	ctx context.Context,
	pool *wsPool,
	privateKey *ecdsa.PrivateKey,
	signer types.Signer,
	address common.Address,
	workerID int,
	batchSize int,
	erc20 bool,
) {
	ticker := time.NewTicker(tickerTime)
	defer ticker.Stop()

	round := 0

	// Run immediately on start
	runWorkerRound(ctx, pool, privateKey, signer, address, workerID, &round, batchSize, erc20)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runWorkerRound(ctx, pool, privateKey, signer, address, workerID, &round, batchSize, erc20)
		}
	}
}

func runWorkerRound(
	ctx context.Context,
	pool *wsPool,
	privateKey *ecdsa.PrivateKey,
	signer types.Signer,
	address common.Address,
	workerID int,
	round *int,
	batchSize int,
	erc20 bool,
) {
	*round++

	// Fetch nonce through the pool
	var nonce uint64
	err := pool.Do(ctx, func(c *ethclient.Client) error {
		var inner error
		nonce, inner = c.PendingNonceAt(ctx, address)
		return inner
	})
	if err != nil {
		fmt.Printf("[Worker %d] Failed to get nonce: %v\n", workerID, err)
		return
	}

	// Send batch (to self)
	_, errors := sendBatch(ctx, pool, privateKey, signer, address, address, nonce, batchSize, erc20, workerID)
	if errors > 0 {
		fmt.Printf("[Worker %d] Errors: %d\n", workerID, errors)
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
	pool *wsPool,
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
		err = pool.Do(ctx, func(c *ethclient.Client) error {
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
