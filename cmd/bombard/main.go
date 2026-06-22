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
	"strconv"
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
//  1. Rate limiter: a rolling token bucket. Tokens accrue at `rps`(×overshoot)
//     per second and are capped at a 1-second burst, so a deficit in the
//     trailing ~1s (send workers briefly saturated, inflight grazing the cap)
//     is made up instead of being dropped at a wall-second boundary — the old
//     per-second-reset budget leaked the tail of every second (~the last few
//     txs each second), so mined always sat a hair under target. Carry-over is
//     bounded to 1s so a long stall (e.g. a failover dip) cannot trigger an
//     unbounded catch-up flood; at most one second is recovered.
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
)

type txState struct {
	signed    *types.Transaction
	firstSend time.Time
	lastSend  time.Time
	resubmits int
}

// tracker is the single source of truth. issued/mined are atomic counters so
// the issuer can check the in-flight cap without taking the lock on every tick;
// the maps and latSince are guarded by mu.
type tracker struct {
	mu       sync.Mutex
	inflight map[uint64]*txState    // nonce -> state, present until mined
	byHash   map[common.Hash]uint64 // tx hash -> nonce, for the watcher

	issued  atomic.Uint64 // nonces released by the issuer
	mined   atomic.Uint64 // nonces observed in a block
	resent  atomic.Uint64 // resubmissions
	dropped atomic.Uint64 // in-flight txs abandoned on a nonce resync (frontier regression)

	latSince []time.Duration // total latencies since the last report tick
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

func (t *tracker) onMined(hash common.Hash, observedAt time.Time) {
	t.mu.Lock()
	nonce, ok := t.byHash[hash]
	if !ok {
		t.mu.Unlock()
		return
	}
	st := t.inflight[nonce]
	delete(t.inflight, nonce)
	delete(t.byHash, hash)

	total := observedAt.Sub(st.firstSend)
	if total < 0 {
		total = 0
	}
	t.latSince = append(t.latSince, total)
	t.mu.Unlock()

	t.mined.Add(1)
}

// inFlight is how many nonces we are ahead of the last-mined nonce. Abandoned
// (dropped) txs don't count — they're no longer waiting to mine, so the in-flight
// cap unblocks the moment a resync drops the stranded set.
func (t *tracker) inFlight() uint64 {
	return t.issued.Load() - t.mined.Load() - t.dropped.Load()
}

// lowestInflightNonce returns the smallest nonce still in flight, and whether any
// exist. The chain must mine exactly this nonce next; if its frontier sits BELOW
// this, our in-flight set is stranded above an unfillable gap (see monitorNonce).
func (t *tracker) lowestInflightNonce() (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.inflight) == 0 {
		return 0, false
	}
	min := uint64(math.MaxUint64)
	for n := range t.inflight {
		if n < min {
			min = n
		}
	}
	return min, true
}

// dropInflight abandons every in-flight tx (used on a nonce resync after a frontier
// regression) and accounts for them as dropped so the in-flight cap unblocks and
// the resubmit loop stops re-sending the stranded set. Returns how many were dropped.
func (t *tracker) dropInflight() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.inflight)
	t.inflight = make(map[uint64]*txState)
	t.byHash = make(map[common.Hash]uint64)
	t.dropped.Add(uint64(n))
	return n
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

var track = newTracker()

// throttleRPS is an externally-imposed ingress cap in rps (0 = none). `reconcile
// restore` writes a reduced rps to the throttle file while a rejoining site is
// catching up, so the issuer eases off automatically and the failback converges,
// then removes the file when caught up — restoring full rate. No permanent change
// to the configured target. Path is shared with cmd/reconcile (defaultThrottleFile
// / BOMBARD_THROTTLE_FILE).
var throttleRPS atomic.Int64

const defaultThrottleFile = "/tmp/bombard.throttle"

// resyncPending/resyncTarget let monitorNonce ask the issuer to jump to a live
// frontier nonce after a failover/reorg strands our in-flight nonces above the
// chain. The monitor sets the target, then the flag; the issuer consumes both.
var (
	resyncPending atomic.Bool
	resyncTarget  atomic.Uint64
)

const (
	// in-flight cap = rps / inflightDivisor nonces ahead of last-mined. A
	// divisor of 2 stops issuance once in-flight represents ~1/2s of latency,
	// providing early backpressure before a resubmit storm can build.
	inflightDivisor = 2
	// resubmitInterval re-sends any tx still in flight after this long, to
	// survive mempool loss from node overload or a crash.
	resubmitInterval = time.Second
	// pollInterval is how often each watcher asks its node for new blocks.
	pollInterval = time.Millisecond
)

func main() {
	rpcFlag := flag.String("rpc", "", "Comma-separated RPC URLs (auto-detected from network_data/rpcs.txt if omitted). Sends fan out across all; watchers race across all.")
	rps := flag.Int("rps", 1000, "Target transactions issued per second")
	targetTxs := flag.Uint64("txs", 0, "Stop after at least this many mined txs; 0 means run until interrupted")
	runDuration := flag.Duration("duration", 0, "Stop after this duration; 0 means run until interrupted or --txs is reached")
	resubmitFlag := flag.Duration("resubmit", resubmitInterval, "Re-send still-in-flight txs after this long. Set above the worst block latency (e.g. failover proposer stalls) to avoid resubmit storms.")
	inflightCapFlag := flag.Int("inflight", 0, "Absolute in-flight cap (nonces ahead of last-mined). 0 = rps/inflightDivisor.")
	overshootFlag := flag.Float64("overshoot", 0, "Issue this fraction above -rps to offset reject/jitter losses so mined lands at-or-above target (e.g. 0.01 = 1% over). Still bounded by the in-flight cap.")
	sendTimeoutFlag := flag.Duration("sendtimeout", time.Second, "Per-node send timeout. Every tx is broadcast to every node over HTTP; a node slower than this is skipped for that tx (errors ignored).")
	tuiFlag := flag.Bool("tui", true, "Render a live terminal UI when stdout is a terminal. Set false to force one-line STATS output.")
	scrapeFlag := flag.String("scrape", "", "Comma-separated RPC URLs to scrape /ext/metrics from at run start/end (decoupled from -rpc). Empty = scrape the -rpc nodes. Lets you send to the tracker but observe every validator.")
	sampleFlag := flag.Duration("sample", 0, "If >0, scrape a focused set of rate/gauge node metrics every interval (e.g. 1s) and print compact SAMPLE rows. Forces -tui=false. Reveals per-second dynamics the begin/end panel hides.")
	flag.Parse()

	if *rps <= 0 {
		fmt.Println("--rps must be > 0")
		os.Exit(1)
	}
	cap := *rps / inflightDivisor
	if *inflightCapFlag > 0 {
		cap = *inflightCapFlag
	}
	if cap < 1 {
		cap = 1
	}
	resubmitEvery := *resubmitFlag
	useTUI := *tuiFlag && stdoutIsTerminal()

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
	go func() {
		sig := <-sigCh
		restoreTerminal()
		if useTUI {
			fmt.Println()
		}
		os.Exit(signalExitCode(sig))
	}()

	if *runDuration > 0 {
		go func() {
			timer := time.NewTimer(*runDuration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
			case <-timer.C:
				if !useTUI {
					fmt.Printf("Duration target reached: %s\n", runDuration.String())
				}
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
					if !useTUI {
						fmt.Printf("Transaction target reached: mined=%d target=%d\n", track.mined.Load(), *targetTxs)
					}
					cancel()
					return
				}
			}
		}()
	}

	// Broadcaster: every tx is sent to every node over HTTP behind a single
	// keep-alive transport (connections reused, not churned). A down or slow
	// node is dropped per-tx and never blocks the issuer or the healthy nodes.
	bc, err := newBroadcaster(ctx, rpcURLs, *sendTimeoutFlag)
	if err != nil {
		fmt.Printf("Failed to start broadcaster: %v\n", err)
		os.Exit(1)
	}
	defer bc.Close()
	sendWorkers := runtime.NumCPU() * 4
	fmt.Printf("Broadcasting to %d node(s) over HTTP (%d conns/node, %s send timeout)\n",
		len(bc.nodes), sendConcPerNode, sendTimeoutFlag.String())

	// One self-reconnecting watcher per endpoint; first to see a tx in a block
	// records it. Each owns and re-dials its own websocket, so a node restarting
	// (failover/restore) no longer permanently kills its watcher. A node that's
	// unreachable at startup is retried until it comes up rather than skipped; the
	// setup connection below still aborts the run if nothing is reachable at all.
	for _, ws := range wsURLs {
		go watchBlocks(ctx, ws, pollInterval)
	}

	// Setup connection (chain ID + start nonce): use the first reachable endpoint.
	var setupRPC *rpc.Client
	for _, ws := range wsURLs {
		rc, err := rpc.DialWebsocket(ctx, ws, "")
		if err != nil {
			continue
		}
		setupRPC = rc
		break
	}
	if setupRPC == nil {
		fmt.Println("No reachable endpoint for setup")
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

	// Read the start nonce as the real ACCEPTED (latest-block) nonce, MAX across
	// all nodes. We deliberately use NonceAt(latest), NOT PendingNonceAt: the
	// pending nonce counts mempool txs, so leftover in-flight txs from a previous
	// run inflate it ABOVE a gap and a fresh run would issue past the missing
	// nonce — stranding the account behind an unfilled gap. The accepted nonce is
	// the true chain frontier and can never skip a gap; re-issuing from there is
	// safe because every tx is idempotent (deterministic signing -> same hash)
	// and a re-sent already-known/already-mined nonce is a benign no-op. MAX
	// ignores a lagging spare that reports a stale-low accepted nonce.
	var startNonce uint64
	gotNonce := false
	for _, n := range bc.nodes {
		nctx, ncancel := context.WithTimeout(ctx, *sendTimeoutFlag)
		nn, nerr := n.client.NonceAt(nctx, address, nil) // nil = latest accepted block
		ncancel()
		if nerr != nil {
			continue
		}
		gotNonce = true
		if nn > startNonce {
			startNonce = nn
		}
	}
	if !gotNonce {
		fmt.Println("Failed to get nonce from any node")
		os.Exit(1)
	}
	fmt.Printf("Issuer: %s  start nonce: %d (max accepted across %d node(s))\n", address.Hex(), startNonce, len(bc.nodes))

	// Metric scrape only makes sense for a bounded run (start/end window to
	// subtract over). Scrape targets default to the send nodes but can be set
	// independently (-scrape) to watch validators while sending to the tracker.
	scrapeURLs := splitNonEmpty(*scrapeFlag)
	if len(scrapeURLs) == 0 {
		scrapeURLs = rpcURLs
	}
	metricsChainID := chainIDFromRPC(scrapeURLs[0])
	var startSnaps []nodeSnapshot
	if *runDuration > 0 {
		startSnaps = scrapeAllNodes(scrapeURLs)
	}

	// Per-second sampler: rate/gauge metrics the begin/end panel can't show.
	if *sampleFlag > 0 {
		useTUI = false
		printSampleLegend(scrapeURLs)
		smp := newSampler(scrapeURLs, metricsChainID)
		go func() {
			tk := time.NewTicker(*sampleFlag)
			defer tk.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case t := <-tk.C:
					smp.tick(t)
				}
			}
		}()
	}

	reportDone := make(chan struct{})
	go func() {
		defer close(reportDone)
		reportLoop(ctx, *rps, cap, useTUI)
	}()
	go resubmitLoop(ctx, bc, resubmitEvery)

	overshootNote := ""
	if *overshootFlag > 0 {
		overshootNote = fmt.Sprintf(" (+%.1f%% overshoot)", *overshootFlag*100)
	}
	fmt.Printf("\nSingle issuer: target %d rps%s, in-flight cap %d nonces, resubmit after %s\n\n",
		*rps, overshootNote, cap, resubmitEvery.String())

	// Send workers sign + submit nonces handed to them by the issuer. Signing
	// is spread across all these goroutines (nproc cores) so it never gates the
	// issuer's 1ms cadence.
	sendCh := make(chan uint64, cap)
	var wg sync.WaitGroup
	for i := 0; i < sendWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendWorker(bc, sendCh, privateKey, signer, address)
		}()
	}

	go watchThrottle(ctx, throttleFilePath(), *rps)
	go monitorNonce(ctx, bc, address)
	go issuer(ctx, sendCh, float64(*rps), *overshootFlag, uint64(cap), startNonce)

	<-ctx.Done()
	close(sendCh)
	wg.Wait()
	<-reportDone
	fmt.Printf("FINAL issued=%d mined=%d inflight=%d resubmits=%d dropped=%d\n",
		track.issued.Load(), track.mined.Load(), track.inFlight(), track.resent.Load(), track.dropped.Load())

	if *runDuration > 0 {
		endSnaps := scrapeAllNodes(scrapeURLs)
		printNodeMetrics(startSnaps, endSnaps, metricsChainID, runDuration.Seconds())
	}
}

// issuer releases nonces under a rolling token bucket and the in-flight cap.
// Tokens accrue at the effective rate (configured rps×overshoot, unless an
// external throttle caps it lower) and are bounded by `burst` (= 1 second's
// worth), so the issuer makes up a deficit from the trailing ~1s instead of
// dropping the tail of each wall-second — while the burst cap keeps a long stall
// from triggering an unbounded catch-up flood. The rate is recomputed each tick,
// so a throttle (set by `reconcile restore` during a failback) eases ingress
// promptly and lifts the moment it clears.
func issuer(ctx context.Context, sendCh chan<- uint64, baseRPS, overshoot float64, cap, startNonce uint64) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	nextNonce := startNonce
	tokens := 0.0
	last := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Frontier-regression rescue: a hard failover to a site that was behind the
		// old tip leaves our in-flight nonces stranded above the chain's frontier,
		// where they can never mine. monitorNonce detects that and requests a resync;
		// jump to the live frontier and abandon the stranded txs so we mine again
		// with no manual restart.
		if resyncPending.Load() {
			rt := resyncTarget.Load()
			n := track.dropInflight()
			nextNonce = rt
			tokens = 0 // don't burst the catch-up right after a resync
			resyncPending.Store(false)
			fmt.Fprintf(os.Stderr, "\nnonce: resynced issuer to live frontier %d, abandoned %d stranded tx(s)\n", rt, n)
		}

		now := time.Now()

		// Effective target rps: the configured rate unless an external throttle
		// caps it lower (recomputed each tick so it takes effect promptly).
		target := baseRPS
		if th := float64(throttleRPS.Load()); th > 0 && th < target {
			target = th
		}
		ratePerSec := target * (1 + overshoot)
		burst := target // rolling ~1s window: bounded catch-up

		tokens += ratePerSec * now.Sub(last).Seconds()
		last = now
		if tokens > burst {
			tokens = burst
		}

		saturated := false
		for tokens >= 1 && track.inFlight() < cap && !saturated {
			select {
			case sendCh <- nextNonce:
				track.issued.Add(1)
				nextNonce++
				tokens--
			case <-ctx.Done():
				return
			default:
				// Send workers saturated; keep the tokens (bounded by burst on the
				// next refill) and retry next tick rather than dropping them.
				saturated = true
			}
		}
	}
}

// throttleFilePath is where the issuer looks for an external ingress cap (written
// by `reconcile restore` during a failback). Override with BOMBARD_THROTTLE_FILE.
func throttleFilePath() string {
	if p := os.Getenv("BOMBARD_THROTTLE_FILE"); p != "" {
		return p
	}
	return defaultThrottleFile
}

// watchThrottle polls the throttle file and publishes any positive rps below the
// configured rate as the issuer's cap; an absent/empty/invalid file means full
// rate. Polled (not inotify-watched) so it's robust to the file being created and
// removed repeatedly across a run.
func watchThrottle(ctx context.Context, path string, fullRPS int) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var last int64 = -1
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		var n int64
		if b, err := os.ReadFile(path); err == nil {
			if v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && v > 0 {
				n = v
			}
		}
		throttleRPS.Store(n)
		if n != last {
			switch {
			case n > 0:
				fmt.Fprintf(os.Stderr, "\nthrottle: ingress capped at %d rps (restore catch-up)\n", n)
			case last > 0:
				fmt.Fprintf(os.Stderr, "\nthrottle: lifted — back to %d rps\n", fullRPS)
			}
			last = n
		}
	}
}

// monitorNonce rescues bombard from a frontier regression. A hard failover (or any
// reorg) can leave the chain's accepted nonce BELOW our lowest in-flight nonce: the
// chain then expects a nonce we already "mined" on the old frontier and will never
// see, so it can never mine our in-flight txs (an unfillable gap) and we wedge
// at-cap forever — issuing, resubmitting, mining nothing. When the live frontier
// sits below our lowest in-flight nonce for two consecutive checks (so a momentary
// race can't trigger it), we request a resync to the live frontier; the issuer then
// jumps there and abandons the stranded set. This is what lets bombard ride through
// a hard failover without a restart.
//
// A clean failover (new site already at the old tip) keeps frontier == lowest
// in-flight, so this never fires — only a genuine regression does.
func monitorNonce(ctx context.Context, bc *broadcaster, address common.Address) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	lastMined := track.mined.Load()
	stalls := 0
	resync := func(frontier uint64, why string) {
		resyncTarget.Store(frontier)
		resyncPending.Store(true)
		fmt.Fprintf(os.Stderr, "\nnonce: %s — resyncing issuer to live frontier %d\n", why, frontier)
		stalls = 0
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		mined := track.mined.Load()
		inflight := track.inFlight()
		frontier, ok := maxAcceptedNonce(ctx, bc, address)

		// Fast path: we're AHEAD of the frontier (a gap — e.g. failover to a site
		// behind the old tip). Our in-flight can never mine; resync down to it now.
		if ok && inflight > 0 {
			if low, have := track.lowestInflightNonce(); have && frontier < low {
				resync(frontier, fmt.Sprintf("frontier %d below lowest in-flight %d (failover gap)", frontier, low))
				lastMined = mined
				continue
			}
		}

		// General path: mined FROZEN while we still hold in-flight work is a stall —
		// our nonce view has desynced from the chain (e.g. the watcher missed mines
		// during node churn, leaving already-accepted "zombie" nonces that pin the
		// in-flight cap and starve new issuance). Confirm over a few ticks (so a
		// normal cutover pause doesn't twitch it), then resync to the live frontier:
		// jump the issuer there and drop the stale in-flight — the manual-restart
		// recovery, automatic. (Resyncing during a benign pause is harmless: the same
		// nonces re-issue to the same deterministic txs.)
		if mined == lastMined && inflight > 0 {
			stalls++
		} else {
			stalls = 0
		}
		lastMined = mined
		if stalls >= 3 && ok {
			resync(frontier, fmt.Sprintf("stalled ~%ds with %d in-flight and 0 mined", stalls*5, inflight))
		}
	}
}

// maxAcceptedNonce reads the issuer account's accepted (latest-block) nonce from
// every HEALTHY node and returns the max — the live chain frontier. Healthy-only so
// a downed/lagging site (e.g. the one we just failed away from) can't report a
// stale-high nonce and mask a regression. Returns false if nothing is reachable.
func maxAcceptedNonce(ctx context.Context, bc *broadcaster, address common.Address) (uint64, bool) {
	var max uint64
	got := false
	for _, n := range bc.nodes {
		if !n.healthy.Load() {
			continue
		}
		nctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		nn, err := n.client.NonceAt(nctx, address, nil) // nil = latest accepted block
		cancel()
		if err != nil {
			continue
		}
		got = true
		if nn > max {
			max = nn
		}
	}
	return max, got
}

func sendWorker(
	bc *broadcaster,
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
		// Fire-and-forget to every node; a failed/dropped send is fine — the tx
		// stays in flight and the resubmit loop re-broadcasts it. We only drop
		// it from accounting when it mines.
		bc.broadcast(signed)
	}
}

func resubmitLoop(ctx context.Context, bc *broadcaster, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, signed := range track.dueForResubmit(interval, time.Now()) {
			bc.broadcast(signed)
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

func reportLoop(ctx context.Context, tps, cap int, useTUI bool) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var ui *terminalStatsUI
	if useTUI {
		ui = newTerminalStatsUI(os.Stdout)
		defer ui.close()
	}

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
		snapshot := statsSnapshot{
			at:        now,
			targetRPS: tps,
			cap:       cap,
			issued:    track.issued.Load(),
			mined:     mined,
			inflight:  inflight,
			resubmits: track.resent.Load(),
			minedTPS:  minedTps,
			atCap:     inflight >= uint64(cap),
			p50:       0,
			p95:       0,
			p99:       0,
		}

		if len(lats) > 0 {
			sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
			snapshot.latencySamples = len(lats)
			snapshot.p50 = pctDur(lats, 50)
			snapshot.p95 = pctDur(lats, 95)
			snapshot.p99 = pctDur(lats, 99)
		}

		if ui != nil {
			ui.render(snapshot)
		} else {
			printStats(snapshot)
		}
	}
}

func pctDur(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * (len(sorted) - 1) / 100
	return sorted[idx]
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
