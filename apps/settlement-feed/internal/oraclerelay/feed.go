package oraclerelay

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"io"
	"math/big"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ethcommon "github.com/ava-labs/libevm/common"
	ethtypes "github.com/ava-labs/libevm/core/types"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	feedGasLimit = 200000
	// Submit cadence. Every configured asset is submitted every tick: 10
	// ticks/s means 10 updates/s per asset. The contract assigns each a
	// monotonic per-asset seq, so sub-second updates are ordered without
	// timestamp clashes. Batched relay delivery removed the old per-message
	// ceiling, so the feed runs at full rate; a relay backlog now drains by
	// packing more messages per tx.
	feedInterval = 100 * time.Millisecond
	// FeedMetricsListenAddress is the fixed, unconfigurable /metrics bind address.
	FeedMetricsListenAddress = "0.0.0.0:9701"
	feedMetricsNamespace     = "oracle_feed"
)

// Direct submissions ride type-2 (EIP-1559) transactions. The priority fee is
// the one optional field that keeps oracle updates ahead of benchmark flood
// traffic: the block builder orders by effective tip and bombard pays none.
// The node's eth_maxPriorityFeePerGas suggestion tracks real congestion but
// sits at ~0 on an idle or flood-only chain, so a small floor preserves the
// ordering guarantee, and the 2x premium lets a latest-nonce restart's resends
// replace stale queue entries (replacement requires a price bump). The fee cap
// is generous headroom over the suggested gas price: unlike a legacy GasPrice
// bid, a type-2 sender pays baseFee+tip regardless of the cap, so headroom
// costs nothing.
const (
	directTipMultiplier    = 2
	minDirectTip           = 10 // wei
	directFeeCapMultiplier = 10
	// onChainPollInterval paces the read-back of the contract's latestPrice,
	// which feeds the on-chain price and feed-vs-chain delta gauges.
	onChainPollInterval = 500 * time.Millisecond
)

// Mock feed parameters in 8-decimal fixed point: a slow bounded random walk so
// the demo shows moving prices without needing a real data source.
const (
	btcBase int64 = 60000 * 1e8
	btcStep int64 = 20 * 1e8 // +/- up to $20 per second
	btcBand int64 = 2000 * 1e8
	// USDC is a stablecoin, so it holds near $1.00 with only peg-jitter.
	usdcBase int64 = 1 * 1e8
	usdcStep int64 = 5e4  // +/- up to $0.0005 per second
	usdcBand int64 = 20e4 // stays within +/- $0.002 of the peg
)

type priceWalk struct {
	rng  *rand.Rand
	btc  int64
	usdc int64
}

func newPriceWalk() *priceWalk {
	// Fixed seed: the walk only needs to look plausible, not be unpredictable.
	return &priceWalk{rng: rand.New(rand.NewSource(1)), btc: btcBase, usdc: usdcBase}
}

func step(rng *rand.Rand, current, base, maxStep, band int64) int64 {
	next := current + rng.Int63n(2*maxStep+1) - maxStep
	if next > base+band {
		next = base + band
	}
	if next < base-band {
		next = base - band
	}
	return next
}

func (w *priceWalk) next(name string) int64 {
	switch name {
	case "BTC-USD":
		w.btc = step(w.rng, w.btc, btcBase, btcStep, btcBand)
		return w.btc
	default:
		w.usdc = step(w.rng, w.usdc, usdcBase, usdcStep, usdcBand)
		return w.usdc
	}
}

func formatPrice(price int64) string {
	return fmt.Sprintf("%d.%08d", price/1e8, price%1e8)
}

// feedMetrics holds the feeder's Prometheus collectors on a dedicated registry.
type feedMetrics struct {
	registry       *prometheus.Registry
	submitted      *prometheus.CounterVec
	confirmed      *prometheus.CounterVec
	inflight       prometheus.Gauge
	price          *prometheus.GaugeVec
	onchainPrice   *prometheus.GaugeVec
	priceDelta     *prometheus.GaugeVec
	confirmLatency *prometheus.HistogramVec
	settlementOpen prometheus.Gauge
	settlementGate *prometheus.GaugeVec
	settledTotal   prometheus.Gauge

	mu            sync.Mutex
	lastSubmitted map[string]*big.Int
}

// settlementGateStates are the one-hot states the Settlement example reports:
// the contract's two refusal reasons, plus "no data" for the pre-first-round
// revert, plus "open".
var settlementGateStates = []string{"open", "depegged", "stale price", "no data"}

func newFeedMetrics() *feedMetrics {
	registry := prometheus.NewRegistry()
	m := &feedMetrics{
		registry:      registry,
		lastSubmitted: make(map[string]*big.Int),
		submitted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: feedMetricsNamespace,
			Name:      "submitted_total",
			Help:      "submitPrice transactions sent, by asset.",
		}, []string{"asset"}),
		confirmed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: feedMetricsNamespace,
			Name:      "confirmed_total",
			Help:      "submitPrice transactions confirmed, by asset.",
		}, []string{"asset"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: feedMetricsNamespace,
			Name:      "inflight",
			Help:      "Sent-but-unconfirmed submitPrice transactions.",
		}),
		price: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: feedMetricsNamespace,
			Name:      "price",
			Help:      "Last submitted mock price as of the same on-chain read-back that sets onchain_price and price_delta, scaled to whole units (raw / 1e8), by asset.",
		}, []string{"asset"}),
		onchainPrice: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: feedMetricsNamespace,
			Name:      "onchain_price",
			Help:      "Latest price read back from the on-chain contract scaled to whole units (raw / 1e8), by asset.",
		}, []string{"asset"}),
		priceDelta: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: feedMetricsNamespace,
			Name:      "price_delta",
			Help:      "Latest submitted feed price minus latest on-chain price, in whole units, by asset.",
		}, []string{"asset"}),
		confirmLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: feedMetricsNamespace,
			Name:      "confirm_latency_seconds",
			Help:      "Submit-to-mined latency per submitPrice transaction, by asset.",
			// Fine-grained through the expected 50-300ms range: quantile
			// panels interpolate inside a bucket, so coarse buckets there
			// overstate p95 by up to half a bucket width.
			Buckets:   []float64{0.025, 0.05, 0.075, 0.1, 0.125, 0.15, 0.2, 0.25, 0.35, 0.5, 1, 2},
		}, []string{"asset"}),
	}
	m.settlementOpen = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: feedMetricsNamespace,
		Name:      "settlement_open",
		Help:      "1 while the Settlement example's canSettle() allows settlement, else 0.",
	})
	m.settlementGate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: feedMetricsNamespace,
		Name:      "settlement_gate",
		Help:      "One-hot settlement gate state from canSettle(): open, depegged, stale price, or no data.",
	}, []string{"state"})
	m.settledTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: feedMetricsNamespace,
		Name:      "settled_total",
		Help:      "The Settlement example's settled() accumulator, read from chain.",
	})
	registry.MustRegister(
		m.submitted,
		m.confirmed,
		m.inflight,
		m.price,
		m.onchainPrice,
		m.priceDelta,
		m.confirmLatency,
		m.settlementOpen,
		m.settlementGate,
		m.settledTotal,
	)
	return m
}

// recordSettlement one-hots the gate state so the dashboard can both light a
// single OPEN/CLOSED stat and chart which refusal reason is active.
func (m *feedMetrics) recordSettlement(open bool, reason string) {
	state := "open"
	if !open {
		state = reason
	}
	if open {
		m.settlementOpen.Set(1)
	} else {
		m.settlementOpen.Set(0)
	}
	for _, known := range settlementGateStates {
		value := 0.0
		if known == state {
			value = 1
		}
		m.settlementGate.WithLabelValues(known).Set(value)
	}
}

func (m *feedMetrics) recordSettled(total *big.Int) {
	value, _ := new(big.Float).SetInt(total).Float64()
	m.settledTotal.Set(value)
}

func (m *feedMetrics) serve(address string) error {
	return serveMetrics(m.registry, address)
}

func (m *feedMetrics) recordSubmit(asset string, price int64) {
	m.submitted.WithLabelValues(asset).Inc()
	m.mu.Lock()
	m.lastSubmitted[asset] = big.NewInt(price)
	m.mu.Unlock()
}

// recordOnChain exports the contract's latest stored price beside the feed's
// own submission, and their difference, so the dashboard shows both series and
// the delta from one scrape target. All three gauges are set here, from the
// same instant: publishing the feed gauge on submit instead would let the two
// price stats show values a few hundred milliseconds apart while the delta
// between them reads zero, which is confusing even though it is not wrong.
func (m *feedMetrics) recordOnChain(asset string, price *big.Int) {
	m.onchainPrice.WithLabelValues(asset).Set(scaledPrice(price))
	m.mu.Lock()
	submitted, ok := m.lastSubmitted[asset]
	m.mu.Unlock()
	if ok {
		m.price.WithLabelValues(asset).Set(scaledPrice(submitted))
		m.priceDelta.WithLabelValues(asset).Set(scaledPrice(submitted) - scaledPrice(price))
	}
}

func (m *feedMetrics) recordEnqueued() {
	m.inflight.Inc()
}

func (m *feedMetrics) recordConfirmed(asset string, elapsed time.Duration) {
	m.confirmed.WithLabelValues(asset).Inc()
	m.confirmLatency.WithLabelValues(asset).Observe(elapsed.Seconds())
	m.inflight.Dec()
}

// feedPending is a sent-but-unconfirmed submission handed to the confirmer.
type feedPending struct {
	asset  string
	hash   ethcommon.Hash
	sentAt time.Time
}

// Feed submits mock prices every tick until the context is cancelled. With an
// oracle L1 it submits every known asset to the aggregator on the oracle
// chain; without one it publishes the direct asset set straight to the
// Chainlink-shaped aggregator on the main chain with type-2 priority-fee
// transactions. In
// both modes submissions pipeline: each is signed and sent while a background
// goroutine confirms receipts. Any RPC error, reverted receipt, or unconfirmed
// delivery is fatal; the operator restarts.
// settlement, when non-zero, is a deployed Settlement example whose gate the
// direct feed's poller watches read-only for the dashboard.
func Feed(ctx context.Context, deployment Deployment, deploymentDirectory, nodeURL string, settlement ethcommon.Address, output io.Writer) error {
	feederKey, feederAddress, err := loadFeederKey(filepath.Join(deploymentDirectory, "oracle-feeder.key"), deployment.FeederAddress)
	if err != nil {
		return err
	}
	if deployment.HasOracle() {
		if settlement != (ethcommon.Address{}) {
			return fmt.Errorf("the settlement watch reads the main chain's direct feed; it is not available with an oracle L1")
		}
		return feedOracleChain(ctx, deployment, feederKey, feederAddress, nodeURL, output)
	}
	return feedDirect(ctx, deployment, feederKey, feederAddress, nodeURL, settlement, output)
}

// feedOracleChain is the oracle-L1 feed path: legacy-priced submissions to the
// aggregator, whose Warp broadcasts the relay then delivers to main.
func feedOracleChain(ctx context.Context, deployment Deployment, feederKey *ecdsa.PrivateKey, feederAddress ethcommon.Address, oracleNodeURL string, output io.Writer) error {
	oracle := newEVMClient(oracleNodeURL, deployment.OracleChainID)
	chainID, err := oracle.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("read oracle chain ID: %w", err)
	}
	signer := ethtypes.LatestSignerForChainID(chainID)
	// Start from the LATEST mined nonce, not pending. A crashed run leaves
	// nonce-gapped txs queued in the mempool; starting at pending would stack new
	// txs behind that unminable gap forever. Latest-nonce re-sends over the queue
	// (see the 2x gas-price premium below, which lets the resends replace the
	// stale, minimum-fee queue entries).
	nonce, err := oracle.LatestNonce(ctx, feederAddress)
	if err != nil {
		return fmt.Errorf("read feeder nonce: %w", err)
	}
	aggregator := deployment.AggregatorAddress

	meters := newFeedMetrics()
	if err := meters.serve(FeedMetricsListenAddress); err != nil {
		return err
	}
	fmt.Fprintf(output, "serving Prometheus metrics at http://%s/metrics (fixed port)\n", FeedMetricsListenAddress)
	fmt.Fprintf(output, "feeding oracle chain %s at %s as %s\n", deployment.OracleChainID, oracle.url, feederAddress.Hex())

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pending := make(chan feedPending, maxInFlight)
	confirmErr := make(chan error, 1)
	fail := func(err error) {
		select {
		case confirmErr <- err:
		default:
		}
		cancel()
	}
	go confirmSubmissions(runCtx, oracle, pending, fail, meters)

	walk := newPriceWalk()
	ticker := time.NewTicker(feedInterval)
	defer ticker.Stop()
	for {
		select {
		case <-runCtx.Done():
			return drainConfirmError(confirmErr)
		case err := <-confirmErr:
			return err
		case <-ticker.C:
		}
		suggestedGasPrice, err := oracle.GasPrice(ctx)
		if err != nil {
			return fmt.Errorf("read oracle gas price: %w", err)
		}
		// Pay 2x the suggested price on every feed tx. On a minBaseFee=1 chain this
		// is negligible, and it lets a latest-nonce restart's gap-refills replace
		// any stale, minimum-fee entries still queued in the mempool.
		gasPrice := new(big.Int).Mul(suggestedGasPrice, big.NewInt(2))
		for _, asset := range KnownAssets {
			price := walk.next(asset.name)
			sentAt := time.Now()
			hash, err := submitPrice(ctx, oracle, signer, feederKey, aggregator, &nonce, asset.id, price, gasPrice)
			if err != nil {
				return err
			}
			meters.recordSubmit(asset.name, price)
			fmt.Fprintf(output, "submitted %s price %s tx %s\n", asset.name, formatPrice(price), hash.Hex())
			select {
			case pending <- feedPending{asset: asset.name, hash: hash, sentAt: sentAt}:
				meters.recordEnqueued()
			case <-runCtx.Done():
				return drainConfirmError(confirmErr)
			}
		}
	}
}

// submitPrice signs and sends one submitPrice tx, bumping the local nonce.
func submitPrice(
	ctx context.Context,
	oracle *evmClient,
	signer ethtypes.Signer,
	feederKey *ecdsa.PrivateKey,
	aggregator ethcommon.Address,
	nonce *uint64,
	assetID ethcommon.Hash,
	price int64,
	gasPrice *big.Int,
) (ethcommon.Hash, error) {
	name := AssetName(assetID)
	tx := ethtypes.NewTx(&ethtypes.LegacyTx{
		Nonce:    *nonce,
		GasPrice: gasPrice,
		Gas:      feedGasLimit,
		To:       &aggregator,
		Value:    big.NewInt(0),
		Data:     packSubmitPrice(assetID, big.NewInt(price)),
	})
	signed, err := ethtypes.SignTx(tx, signer, feederKey)
	if err != nil {
		return ethcommon.Hash{}, fmt.Errorf("sign submitPrice for %s: %w", name, err)
	}
	rawTx, err := signed.MarshalBinary()
	if err != nil {
		return ethcommon.Hash{}, fmt.Errorf("encode submitPrice for %s: %w", name, err)
	}
	hash, err := oracle.SendRawTransaction(ctx, rawTx)
	if err != nil {
		return ethcommon.Hash{}, fmt.Errorf("submit %s price: %w", name, err)
	}
	*nonce++
	return hash, nil
}

// directAssets is what the direct feed publishes: the consumers asked for the
// single low-volatility pair first.
var directAssets = []assetRef{{"USDC-USD", assetUSDC}}

// feedDirect is the no-oracle-chain feed path: type-2 submissions straight to
// the Chainlink-shaped aggregator on the main chain, plus a poller that reads
// latestRoundData back through the consumer-facing proxy so the dashboard
// charts exactly what a consumer contract would see.
func feedDirect(ctx context.Context, deployment Deployment, feederKey *ecdsa.PrivateKey, feederAddress ethcommon.Address, mainNodeURL string, settlement ethcommon.Address, output io.Writer) error {
	main := newEVMClient(mainNodeURL, deployment.MainChainID)
	chainID, err := main.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("read main chain ID: %w", err)
	}
	signer := ethtypes.LatestSignerForChainID(chainID)
	// Latest-nonce start for the same reason as the oracle-chain path: a
	// crashed run's queued txs must be replaced, not stacked behind.
	nonce, err := main.LatestNonce(ctx, feederAddress)
	if err != nil {
		return fmt.Errorf("read feeder nonce: %w", err)
	}
	proxy := deployment.PriceFeedAddress
	aggregator := deployment.PriceFeedAggregatorAddress
	// A submit to a codeless address succeeds and does nothing, so a chain
	// without the app installed must fail loudly here, not publish nonsense.
	hasCode, err := main.HasCode(ctx, aggregator)
	if err != nil {
		return fmt.Errorf("check aggregator code at %s: %w", aggregator.Hex(), err)
	}
	if !hasCode {
		return fmt.Errorf("no contract at aggregator %s: the settlement-feed app is not installed on this chain. Install it with `oracle upgrade` and `fleet upgrade upgrade.json` (playbooks/08-install-app.md)", aggregator.Hex())
	}

	meters := newFeedMetrics()
	if err := meters.serve(FeedMetricsListenAddress); err != nil {
		return err
	}
	fmt.Fprintf(output, "serving Prometheus metrics at http://%s/metrics (fixed port)\n", FeedMetricsListenAddress)
	fmt.Fprintf(output, "publishing prices directly to main chain %s at %s as %s (aggregator %s, consumer proxy %s)\n", deployment.MainChainID, main.url, feederAddress.Hex(), aggregator.Hex(), proxy.Hex())

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pending := make(chan feedPending, maxInFlight)
	confirmErr := make(chan error, 1)
	fail := func(err error) {
		select {
		case confirmErr <- err:
		default:
		}
		cancel()
	}
	if settlement != (ethcommon.Address{}) {
		fmt.Fprintf(output, "watching Settlement example gate at %s\n", settlement.Hex())
	}
	go confirmSubmissions(runCtx, main, pending, fail, meters)
	go pollOnChainPrices(runCtx, main, proxy, settlement, directAssets, fail, meters)

	walk := newPriceWalk()
	ticker := time.NewTicker(feedInterval)
	defer ticker.Stop()
	for {
		select {
		case <-runCtx.Done():
			return drainConfirmError(confirmErr)
		case err := <-confirmErr:
			return err
		case <-ticker.C:
		}
		suggestedTip, err := main.MaxPriorityFeePerGas(ctx)
		if err != nil {
			return fmt.Errorf("read main priority fee: %w", err)
		}
		suggestedGasPrice, err := main.GasPrice(ctx)
		if err != nil {
			return fmt.Errorf("read main gas price: %w", err)
		}
		tipCap, feeCap := directFees(suggestedTip, suggestedGasPrice)
		for _, asset := range directAssets {
			price := walk.next(asset.name)
			sentAt := time.Now()
			hash, err := submitPriceDirect(ctx, main, signer, feederKey, aggregator, &nonce, chainID, asset.name, price, tipCap, feeCap)
			if err != nil {
				return err
			}
			meters.recordSubmit(asset.name, price)
			fmt.Fprintf(output, "submitted %s price %s tx %s\n", asset.name, formatPrice(price), hash.Hex())
			select {
			case pending <- feedPending{asset: asset.name, hash: hash, sentAt: sentAt}:
				meters.recordEnqueued()
			case <-runCtx.Done():
				return drainConfirmError(confirmErr)
			}
		}
	}
}

// directFees derives one tick's type-2 fee fields; see the constants above for
// why the tip has a premium and a floor while the cap is only headroom.
func directFees(suggestedTip, suggestedGasPrice *big.Int) (tipCap, feeCap *big.Int) {
	tipCap = new(big.Int).Mul(suggestedTip, big.NewInt(directTipMultiplier))
	if floor := big.NewInt(minDirectTip); tipCap.Cmp(floor) < 0 {
		tipCap = floor
	}
	feeCap = new(big.Int).Mul(suggestedGasPrice, big.NewInt(directFeeCapMultiplier))
	feeCap.Add(feeCap, tipCap)
	return tipCap, feeCap
}

// submitPriceDirect signs and sends one type-2 submit tx to the aggregator,
// bumping the local nonce.
func submitPriceDirect(
	ctx context.Context,
	main *evmClient,
	signer ethtypes.Signer,
	feederKey *ecdsa.PrivateKey,
	aggregator ethcommon.Address,
	nonce *uint64,
	chainID *big.Int,
	name string,
	price int64,
	tipCap *big.Int,
	feeCap *big.Int,
) (ethcommon.Hash, error) {
	tx := ethtypes.NewTx(&ethtypes.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     *nonce,
		GasTipCap: tipCap,
		GasFeeCap: feeCap,
		Gas:       feedGasLimit,
		To:        &aggregator,
		Value:     big.NewInt(0),
		Data:      packSubmit(big.NewInt(price)),
	})
	signed, err := ethtypes.SignTx(tx, signer, feederKey)
	if err != nil {
		return ethcommon.Hash{}, fmt.Errorf("sign submitPrice for %s: %w", name, err)
	}
	rawTx, err := signed.MarshalBinary()
	if err != nil {
		return ethcommon.Hash{}, fmt.Errorf("encode submitPrice for %s: %w", name, err)
	}
	hash, err := main.SendRawTransaction(ctx, rawTx)
	if err != nil {
		return ethcommon.Hash{}, fmt.Errorf("submit %s price: %w", name, err)
	}
	*nonce++
	return hash, nil
}

// pollOnChainPrices reads latestRoundData through the consumer-facing proxy on
// a fixed cadence, so the exported on-chain series is exactly a consumer
// contract's view, and, when a Settlement example address is configured, its
// canSettle()/settled() beside it. A revert before the first round ("No data
// present") is expected at startup and skipped; other errors are fatal through
// fail, matching the feed's fail-fast posture.
func pollOnChainPrices(ctx context.Context, main *evmClient, proxy, settlement ethcommon.Address, assets []assetRef, fail func(error), meters *feedMetrics) {
	ticker := time.NewTicker(onChainPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, asset := range assets {
			result, err := main.CallContract(ctx, proxy, packLatestRoundData())
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if strings.Contains(err.Error(), "No data present") {
					continue
				}
				fail(fmt.Errorf("read on-chain %s price: %w", asset.name, err))
				return
			}
			answer, _, err := decodeLatestRoundData(result)
			if err != nil {
				fail(fmt.Errorf("decode on-chain %s price: %w", asset.name, err))
				return
			}
			meters.recordOnChain(asset.name, answer)
		}
		if settlement == (ethcommon.Address{}) {
			continue
		}
		if err := pollSettlementGate(ctx, main, settlement, meters); err != nil {
			if ctx.Err() != nil {
				return
			}
			fail(err)
			return
		}
	}
}

// pollSettlementGate reads the Settlement example's view surface. canSettle
// reverting means the feed has no round yet, which the gate reports as the
// closed "no data" state rather than an error.
func pollSettlementGate(ctx context.Context, main *evmClient, settlement ethcommon.Address, meters *feedMetrics) error {
	result, err := main.CallContract(ctx, settlement, packCanSettle())
	switch {
	case err != nil && strings.Contains(err.Error(), "No data present"):
		meters.recordSettlement(false, "no data")
		return nil
	case err != nil:
		return fmt.Errorf("read settlement gate: %w", err)
	}
	open, reason, err := decodeCanSettle(result)
	if err != nil {
		return fmt.Errorf("decode settlement gate: %w", err)
	}
	meters.recordSettlement(open, reason)

	result, err = main.CallContract(ctx, settlement, packSettled())
	if err != nil {
		return fmt.Errorf("read settled total: %w", err)
	}
	total, err := decodeSettled(result)
	if err != nil {
		return fmt.Errorf("decode settled total: %w", err)
	}
	meters.recordSettled(total)
	return nil
}

// confirmSubmissions confirms sent submissions in order. A reverted or
// unconfirmed receipt is fatal for the whole feeder via fail.
func confirmSubmissions(ctx context.Context, oracle *evmClient, pending <-chan feedPending, fail func(error), meters *feedMetrics) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-pending:
			receiptCtx, cancel := context.WithTimeout(ctx, confirmTimeout)
			// 10ms: the poll interval is the floor of the confirm-latency
			// histogram's resolution, and at 10 submits/s the extra receipt
			// queries are negligible next to bombard's load.
			r, err := oracle.WaitReceipt(receiptCtx, item.hash, 10*time.Millisecond)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				fail(fmt.Errorf("await %s submitPrice receipt tx %s: %w", item.asset, item.hash.Hex(), err))
				return
			}
			if r.Status != 1 {
				fail(fmt.Errorf("submit %s price reverted: tx %s in block %d", item.asset, item.hash.Hex(), r.BlockNumber))
				return
			}
			meters.recordConfirmed(item.asset, time.Since(item.sentAt))
		}
	}
}
