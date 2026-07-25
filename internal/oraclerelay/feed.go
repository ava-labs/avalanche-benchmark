package oraclerelay

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"io"
	"math/big"
	"math/rand"
	"path/filepath"
	"time"

	ethcommon "github.com/ava-labs/libevm/common"
	ethtypes "github.com/ava-labs/libevm/core/types"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	feedGasLimit = 200000
	// Submit cadence. Both assets are submitted every tick: 10 ticks/s means 10
	// updates/s per asset (20 tx/s total). The contract assigns each a monotonic
	// per-asset seq, so sub-second updates are ordered without timestamp clashes.
	// Batched relay delivery removed the old per-message ceiling, so the feed runs
	// at full rate; a relay backlog now drains by packing more messages per tx.
	feedInterval = 100 * time.Millisecond
	// FeedMetricsListenAddress is the fixed, unconfigurable /metrics bind address.
	FeedMetricsListenAddress = "0.0.0.0:9701"
	feedMetricsNamespace     = "oracle_feed"
)

// Mock feed parameters in 8-decimal fixed point: a slow bounded random walk so
// the demo shows moving prices without needing a real data source.
const (
	btcBase  int64 = 60000 * 1e8
	btcStep  int64 = 20 * 1e8 // +/- up to $20 per second
	btcBand  int64 = 2000 * 1e8
	avaxBase int64 = 25 * 1e8
	avaxStep int64 = 2e6 // +/- up to $0.02 per second
	avaxBand int64 = 2 * 1e8
)

type priceWalk struct {
	rng  *rand.Rand
	btc  int64
	avax int64
}

func newPriceWalk() *priceWalk {
	// Fixed seed: the walk only needs to look plausible, not be unpredictable.
	return &priceWalk{rng: rand.New(rand.NewSource(1)), btc: btcBase, avax: avaxBase}
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
		w.avax = step(w.rng, w.avax, avaxBase, avaxStep, avaxBand)
		return w.avax
	}
}

func formatPrice(price int64) string {
	return fmt.Sprintf("%d.%08d", price/1e8, price%1e8)
}

// feedMetrics holds the feeder's Prometheus collectors on a dedicated registry.
type feedMetrics struct {
	registry  *prometheus.Registry
	submitted *prometheus.CounterVec
	confirmed *prometheus.CounterVec
	inflight  prometheus.Gauge
	price     *prometheus.GaugeVec
}

func newFeedMetrics() *feedMetrics {
	registry := prometheus.NewRegistry()
	m := &feedMetrics{
		registry: registry,
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
			Help:      "Latest submitted mock price scaled to whole units (raw / 1e8), by asset.",
		}, []string{"asset"}),
	}
	registry.MustRegister(m.submitted, m.confirmed, m.inflight, m.price)
	return m
}

func (m *feedMetrics) serve(address string) error {
	return serveMetrics(m.registry, address)
}

func (m *feedMetrics) recordSubmit(asset string, price int64) {
	m.submitted.WithLabelValues(asset).Inc()
	m.price.WithLabelValues(asset).Set(scaledPrice(big.NewInt(price)))
}

func (m *feedMetrics) recordEnqueued() {
	m.inflight.Inc()
}

func (m *feedMetrics) recordConfirmed(asset string) {
	m.confirmed.WithLabelValues(asset).Inc()
	m.inflight.Dec()
}

// feedPending is a sent-but-unconfirmed submission handed to the confirmer.
type feedPending struct {
	asset string
	hash  ethcommon.Hash
}

// Feed submits mock prices for both assets to the aggregator on the oracle L1
// every tick, until the context is cancelled. Submissions pipeline: each is
// signed and sent while a background goroutine confirms receipts. Any RPC error,
// reverted receipt, or unconfirmed delivery is fatal; the operator restarts.
func Feed(ctx context.Context, deployment Deployment, deploymentDirectory, oracleNodeURL string, output io.Writer) error {
	feederKey, feederAddress, err := loadFeederKey(filepath.Join(deploymentDirectory, "oracle-feeder.key"), deployment.FeederAddress)
	if err != nil {
		return err
	}
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
			hash, err := submitPrice(ctx, oracle, signer, feederKey, aggregator, &nonce, asset.id, price, gasPrice)
			if err != nil {
				return err
			}
			meters.recordSubmit(asset.name, price)
			fmt.Fprintf(output, "submitted %s price %s tx %s\n", asset.name, formatPrice(price), hash.Hex())
			select {
			case pending <- feedPending{asset: asset.name, hash: hash}:
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

// confirmSubmissions confirms sent submissions in order. A reverted or
// unconfirmed receipt is fatal for the whole feeder via fail.
func confirmSubmissions(ctx context.Context, oracle *evmClient, pending <-chan feedPending, fail func(error), meters *feedMetrics) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-pending:
			receiptCtx, cancel := context.WithTimeout(ctx, confirmTimeout)
			r, err := oracle.WaitReceipt(receiptCtx, item.hash)
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
			meters.recordConfirmed(item.asset)
		}
	}
}
