package oraclerelay

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"math/big"
	"path/filepath"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/upgrade"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
	"github.com/ava-labs/avalanchego/vms/proposervm"
	ethereum "github.com/ava-labs/libevm"
	ethcommon "github.com/ava-labs/libevm/common"
	ethtypes "github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethclient"
)

const (
	// Deliveries are sent as messages arrive and confirmed asynchronously; this
	// bounds how many unconfirmed txs may be in flight before send blocks (that
	// back-pressure is intentional).
	maxInFlight = 256
	// maxBatchSize caps how many messages ride one delivery tx (one warp predicate
	// each). Batching only kicks in under backlog; there is no timer, so a single
	// pending message degrades to a batch of one with zero added latency.
	maxBatchSize = 16
	// A delivery that is not mined within this window is fatal.
	confirmTimeout = 30 * time.Second
	// Deliveries pay a premium over the suggested gas price so they out-bid
	// benchmark flood traffic: bombard pays the minimum fee, and the block builder
	// orders txs by effective gas price, so a 10x premium keeps oracle updates at
	// the front of the queue under load. The floor guards against a zero suggestion.
	deliveryGasPriceMultiplier = 10
	minDeliveryGasPrice        = 10 // wei
	// Gas scales with batch size: a fixed base plus a per-message allowance for
	// each message's warp signature verification and receiver storage write.
	baseDeliveryGas       = 400000
	perMessageDeliveryGas = 250000
)

// priorityGasPrice applies the delivery premium and floor.
func priorityGasPrice(suggested *big.Int) *big.Int {
	price := new(big.Int).Mul(suggested, big.NewInt(deliveryGasPriceMultiplier))
	if floor := big.NewInt(minDeliveryGasPrice); price.Cmp(floor) < 0 {
		return floor
	}
	return price
}

// batchGasLimit sizes a delivery tx's gas for n batched messages.
func batchGasLimit(n int) uint64 {
	return uint64(baseDeliveryGas + perMessageDeliveryGas*n)
}

// canonicalSetCache memoizes the canonical oracle validator set by the P-chain
// height its epoch pins. The epoch rolls only every few minutes, so this avoids
// a getValidatorsAt call on every message.
type canonicalSetCache struct {
	height uint64
	set    validators.WarpSet
	loaded bool
}

// needsRefetch reports whether the cached set is missing or was built at a
// different pinned height than the one now in effect.
func (c *canonicalSetCache) needsRefetch(height uint64) bool {
	return !c.loaded || c.height != height
}

func (c *canonicalSetCache) store(height uint64, set validators.WarpSet) {
	c.height = height
	c.set = set
	c.loaded = true
}

// confirmMessage carries one batched message's data through to confirmation so
// its own latency histograms can be observed with the batch's single receipt.
type confirmMessage struct {
	asset     string
	seenAt    time.Time
	updatedAt uint64
}

// pendingDelivery is a sent-but-unconfirmed delivery tx handed to the confirmer.
// One tx may carry several messages (a batch); each is confirmed against the
// tx's single receipt.
type pendingDelivery struct {
	hash     ethcommon.Hash
	messages []confirmMessage
}

// Relay watches the aggregator's SendWarpMessage broadcasts on the oracle L1 over
// a WebSocket log subscription, signs each with every oracle validator's BLS key,
// and delivers the aggregated Warp message to the receiver on the main L1.
// Deliveries pipeline: they are signed and sent as messages arrive while a
// background goroutine confirms receipts. Foreground until ctx cancels; a failed
// or unconfirmed delivery is fatal.
func Relay(ctx context.Context, pChainAPI string, deployment Deployment, deploymentDirectory, oracleNodeURL, mainNodeURL string, output io.Writer) error {
	feederKey, feederAddress, err := loadFeederKey(filepath.Join(deploymentDirectory, "oracle-feeder.key"), deployment.FeederAddress)
	if err != nil {
		return err
	}
	public, _, err := creation.LoadPublic(filepath.Join(deploymentDirectory, "public.json"))
	if err != nil {
		return err
	}
	signers, err := loadOracleSigners(deploymentDirectory, public)
	if err != nil {
		return err
	}
	networkID, err := constants.NetworkID(deployment.Network)
	if err != nil {
		return fmt.Errorf("resolve network %q: %w", deployment.Network, err)
	}

	main := newEVMClient(mainNodeURL, deployment.MainChainID)
	pChain := platformvm.NewClient(pChainAPI)
	mainEpochs := proposervm.NewJSONRPCClient(mainNodeURL, deployment.MainChainID.String())

	if err := gateOracleConversion(ctx, pChain, mainEpochs, deployment, networkID, output); err != nil {
		return err
	}

	mainChainID, err := main.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("read main chain ID: %w", err)
	}
	mainSigner := ethtypes.LatestSignerForChainID(mainChainID)
	nonce, err := main.PendingNonce(ctx, feederAddress)
	if err != nil {
		return fmt.Errorf("read feeder nonce on main chain: %w", err)
	}

	meters := newMetrics()
	if err := meters.serve(MetricsListenAddress); err != nil {
		return err
	}
	fmt.Fprintf(output, "serving Prometheus metrics at http://%s/metrics (fixed port)\n", MetricsListenAddress)

	wsURL, err := wsEndpoint(oracleNodeURL, deployment.OracleChainID)
	if err != nil {
		return err
	}
	oracleWS, err := ethclient.DialContext(ctx, wsURL)
	if err != nil {
		return fmt.Errorf("dial oracle log subscription at %s: %w", wsURL, err)
	}
	defer oracleWS.Close()

	logs := make(chan ethtypes.Log, 64)
	query := ethereum.FilterQuery{
		Addresses: []ethcommon.Address{WarpPrecompileAddress},
		Topics:    [][]ethcommon.Hash{{SendWarpMessageTopic}, {ethcommon.BytesToHash(deployment.AggregatorAddress.Bytes())}},
	}
	subscription, err := oracleWS.SubscribeFilterLogs(ctx, query, logs)
	if err != nil {
		return fmt.Errorf("subscribe to oracle warp logs at %s: %w", wsURL, err)
	}
	defer subscription.Unsubscribe()
	fmt.Fprintf(output, "relaying oracle chain %s -> main chain %s, subscribed at %s\n", deployment.OracleChainID, deployment.MainChainID, wsURL)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pending := make(chan pendingDelivery, maxInFlight)
	confirmErr := make(chan error, 1)
	fail := func(err error) {
		select {
		case confirmErr <- err:
		default:
		}
		cancel()
	}
	go confirmDeliveries(runCtx, main, pending, fail, meters, output)
	go pollMainPrices(runCtx, main, deployment, meters, fail)

	var cache canonicalSetCache
	fresh := newFreshnessGate()
	for {
		select {
		case <-runCtx.Done():
			return drainConfirmError(confirmErr)
		case err := <-confirmErr:
			return err
		case err := <-subscription.Err():
			if err == nil {
				return nil
			}
			return fmt.Errorf("oracle log subscription over %s failed: %w", wsURL, err)
		case entry := <-logs:
			// Drain everything currently buffered along with this message and pack
			// the survivors into one batch. No timer: batching emerges only under
			// backlog, so a lone message ships immediately as a batch of one.
			batch, err := collectBatch(entry, logs, fresh, meters, output)
			if err != nil {
				return err
			}
			if len(batch) == 0 {
				continue
			}
			item, err := deliverBatch(runCtx, pChain, mainEpochs, main, mainSigner, feederKey, signers, &nonce, &cache, deployment, batch, meters, output)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					continue
				}
				return err
			}
			select {
			case pending <- item:
				meters.recordEnqueued()
			case <-runCtx.Done():
				return drainConfirmError(confirmErr)
			}
		}
	}
}

// drainConfirmError distinguishes a fatal confirmer failure (error buffered)
// from an ordinary signal-driven shutdown (no error).
func drainConfirmError(confirmErr chan error) error {
	select {
	case err := <-confirmErr:
		return err
	default:
		return nil
	}
}

// gateOracleConversion enforces the ACP-181 visibility gate once at startup.
func gateOracleConversion(ctx context.Context, pChain *platformvm.Client, mainEpochs epochClient, deployment Deployment, networkID uint32, output io.Writer) error {
	tipHeight, err := pChain.GetHeight(ctx)
	if err != nil {
		return fmt.Errorf("read P-chain height: %w", err)
	}
	startTimes, err := oracleStartTimes(ctx, pChain, deployment.OracleSubnetID)
	if err != nil {
		return err
	}
	convertedAt, err := conversionTime(startTimes)
	if err != nil {
		return err
	}
	conversionHeight, err := findConversionHeight(ctx, pChain, tipHeight, convertedAt, deployment.OracleConvertTxID)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "oracle conversion %s committed at P-chain height %d\n", deployment.OracleConvertTxID, conversionHeight)
	return gateOracleVisibility(ctx, mainEpochs, conversionHeight, upgrade.GetConfig(networkID).GraniteEpochDuration, time.Now(), output)
}

// oracleStartTimes reads the immutable conversion StartTime of every oracle
// validator so the conversion block can be located.
func oracleStartTimes(ctx context.Context, pChain *platformvm.Client, subnetID ids.ID) ([]uint64, error) {
	members, err := pChain.GetCurrentValidators(ctx, subnetID, nil)
	if err != nil {
		return nil, fmt.Errorf("read oracle validators for %s: %w", subnetID, err)
	}
	startTimes := make([]uint64, 0, len(members))
	for _, member := range members {
		if member.ValidationID == nil {
			return nil, fmt.Errorf("oracle validator %s has no validation ID", member.NodeID)
		}
		state, _, err := pChain.GetL1Validator(ctx, *member.ValidationID)
		if err != nil {
			return nil, fmt.Errorf("read oracle validator %s: %w", *member.ValidationID, err)
		}
		startTimes = append(startTimes, state.StartTime)
	}
	return startTimes, nil
}

// freshnessGate remembers the highest seq delivered per asset. seq is a per-asset
// monotonic counter assigned by the aggregator, so it orders same-second updates
// that updatedAt (second resolution) cannot. Marking happens at send time: a
// later revert is fatal anyway, so premature marking cannot wedge the process.
type freshnessGate struct {
	latestSeq map[[32]byte]uint64
}

func newFreshnessGate() *freshnessGate {
	return &freshnessGate{latestSeq: make(map[[32]byte]uint64)}
}

func (g *freshnessGate) fresher(assetID [32]byte, seq uint64) bool {
	if seq <= g.latestSeq[assetID] {
		return false
	}
	g.latestSeq[assetID] = seq
	return true
}

// batchMessage is a parsed, freshness-passed message awaiting signing/packing.
type batchMessage struct {
	assetID     ethcommon.Hash
	asset       string
	seq         uint64
	updatedAt   uint64
	price       *big.Int
	oracleBlock uint64
	seenAt      time.Time
	unsigned    *warp.UnsignedMessage
}

// parseAndGate decodes one log and applies the sequence freshness gate. It
// returns ok=false (no error) for a stale/duplicate message, which is skipped;
// a decode error is fatal.
func parseAndGate(log ethtypes.Log, fresh *freshnessGate, meters *metrics, output io.Writer) (batchMessage, bool, error) {
	unsignedBytes, err := UnpackEventMessage(log.Data)
	if err != nil {
		return batchMessage{}, false, fmt.Errorf("oracle block %d: %w", log.BlockNumber, err)
	}
	submission, err := ParseSubmission(unsignedBytes)
	if err != nil {
		return batchMessage{}, false, fmt.Errorf("oracle block %d: %w", log.BlockNumber, err)
	}
	asset := AssetName(submission.AssetID)
	seq := submission.Seq.Uint64()
	// Freshness is sequence-based: deliver only when seq exceeds the last seq
	// delivered for this asset, skipping duplicates/reorders (e.g. on ws
	// reconnect) instead of burning a guaranteed stale-update revert.
	if !fresh.fresher(submission.AssetID, seq) {
		meters.recordSkipped(asset)
		fmt.Fprintf(output, "skipped %s oracle-block %d: seq %d not fresher\n", asset, log.BlockNumber, seq)
		return batchMessage{}, false, nil
	}
	unsigned, err := warp.ParseUnsignedMessage(unsignedBytes)
	if err != nil {
		return batchMessage{}, false, fmt.Errorf("oracle block %d: parse unsigned Warp message: %w", log.BlockNumber, err)
	}
	return batchMessage{
		assetID:     submission.AssetID,
		asset:       asset,
		seq:         seq,
		updatedAt:   submission.UpdatedAt.Uint64(),
		price:       submission.Price,
		oracleBlock: log.BlockNumber,
		// The ws-event-seen clock starts when the log is dequeued for this batch.
		seenAt:   time.Now(),
		unsigned: unsigned,
	}, true, nil
}

// collectBatch packs the triggering log plus everything already buffered in the
// ws channel (non-blocking) into up to maxBatchSize freshness-passed messages,
// preserving arrival order. It never waits for more messages.
func collectBatch(first ethtypes.Log, logs <-chan ethtypes.Log, fresh *freshnessGate, meters *metrics, output io.Writer) ([]batchMessage, error) {
	batch := make([]batchMessage, 0, maxBatchSize)
	add := func(log ethtypes.Log) (full bool, err error) {
		msg, ok, err := parseAndGate(log, fresh, meters, output)
		if err != nil {
			return false, err
		}
		if ok {
			batch = append(batch, msg)
		}
		return len(batch) >= maxBatchSize, nil
	}
	if full, err := add(first); err != nil || full {
		return batch, err
	}
	for {
		select {
		case log := <-logs:
			if full, err := add(log); err != nil || full {
				return batch, err
			}
		default:
			return batch, nil
		}
	}
}

// deliverBatch signs each message in the batch and packs them into ONE
// AccessListTx: one warp-precompile predicate per message in message order, with
// receivePrices(count). Confirmation happens asynchronously.
func deliverBatch(
	ctx context.Context,
	pChain *platformvm.Client,
	mainEpochs epochClient,
	main *evmClient,
	mainSigner ethtypes.Signer,
	feederKey *ecdsa.PrivateKey,
	signers []bls.Signer,
	nonce *uint64,
	cache *canonicalSetCache,
	deployment Deployment,
	batch []batchMessage,
	meters *metrics,
	output io.Writer,
) (pendingDelivery, error) {
	// The verifier uses the validator set pinned by the main chain's CURRENT
	// proposervm epoch, so canonical bit positions must be built at exactly that
	// P-chain height. One epoch read serves the whole batch; the set is cached
	// until the pinned height changes.
	epoch, err := mainEpochs.GetCurrentEpoch(ctx)
	if err != nil {
		return pendingDelivery{}, fmt.Errorf("read main-chain Warp epoch: %w", err)
	}
	if cache.needsRefetch(epoch.PChainHeight) {
		warpSet, err := canonicalSet(ctx, pChain, epoch.PChainHeight, deployment.OracleSubnetID)
		if err != nil {
			return pendingDelivery{}, fmt.Errorf("build oracle canonical set at height %d: %w", epoch.PChainHeight, err)
		}
		cache.store(epoch.PChainHeight, warpSet)
		meters.recordCanonicalRefresh()
	}

	// One access-list tuple per message; predicate order equals message order, so
	// receivePrices reads warp index i from the i-th tuple.
	tuples := make(ethtypes.AccessList, 0, len(batch))
	for _, msg := range batch {
		signed, err := signAndAggregate(msg.unsigned, cache.set, signers)
		if err != nil {
			return pendingDelivery{}, err
		}
		tuples = append(tuples, ethtypes.AccessTuple{
			Address:     WarpPrecompileAddress,
			StorageKeys: PackPredicate(signed.Bytes()),
		})
	}

	suggestedGasPrice, err := main.GasPrice(ctx)
	if err != nil {
		return pendingDelivery{}, fmt.Errorf("read main gas price: %w", err)
	}
	receiver := deployment.ReceiverAddress
	tx := ethtypes.NewTx(&ethtypes.AccessListTx{
		ChainID:    mainSigner.ChainID(),
		Nonce:      *nonce,
		GasPrice:   priorityGasPrice(suggestedGasPrice),
		Gas:        batchGasLimit(len(batch)),
		To:         &receiver,
		Value:      big.NewInt(0),
		Data:       packReceivePrices(uint32(len(batch))),
		AccessList: tuples,
	})
	signedTx, err := ethtypes.SignTx(tx, mainSigner, feederKey)
	if err != nil {
		return pendingDelivery{}, fmt.Errorf("sign delivery tx: %w", err)
	}
	rawTx, err := signedTx.MarshalBinary()
	if err != nil {
		return pendingDelivery{}, fmt.Errorf("encode delivery tx: %w", err)
	}
	hash, err := main.SendRawTransaction(ctx, rawTx)
	if err != nil {
		return pendingDelivery{}, fmt.Errorf("deliver batch of %d: %w", len(batch), err)
	}
	*nonce++

	meters.recordBatchSize(len(batch))
	messages := make([]confirmMessage, len(batch))
	for i, msg := range batch {
		meters.recordDelivery(msg.asset, msg.price, msg.updatedAt)
		meters.recordSeq(msg.asset, msg.seq)
		fmt.Fprintf(output, "delivered %s price %s seq %d oracle-block %d main-tx %s (batch %d/%d)\n",
			msg.asset, formatPrice(msg.price.Int64()), msg.seq, msg.oracleBlock, hash.Hex(), i+1, len(batch))
		messages[i] = confirmMessage{asset: msg.asset, seenAt: msg.seenAt, updatedAt: msg.updatedAt}
	}
	return pendingDelivery{hash: hash, messages: messages}, nil
}

// confirmDeliveries confirms sent deliveries in order. A reverted or unconfirmed
// receipt is fatal for the whole relay via fail.
func confirmDeliveries(ctx context.Context, main *evmClient, pending <-chan pendingDelivery, fail func(error), meters *metrics, output io.Writer) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-pending:
			receiptCtx, cancel := context.WithTimeout(ctx, confirmTimeout)
			r, err := main.WaitReceipt(receiptCtx, item.hash)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				fail(fmt.Errorf("await delivery receipt for batch tx %s: %w", item.hash.Hex(), err))
				return
			}
			if r.Status != 1 {
				fail(fmt.Errorf("delivery batch reverted: tx %s in block %d", item.hash.Hex(), r.BlockNumber))
				return
			}
			// One receipt covers the whole batch; observe each message's own
			// latency against it, then release the single tx from the queue.
			now := time.Now()
			for _, msg := range item.messages {
				meters.recordConfirmation(msg.asset, msg.seenAt, msg.updatedAt, now)
			}
			meters.recordDequeued()
			fmt.Fprintf(output, "confirmed batch of %d tx %s block %d\n", len(item.messages), item.hash.Hex(), r.BlockNumber)
		}
	}
}

// mainPricePollInterval samples the receiver's on-chain price for the main
// series. It must be well under the delivery latency (~150ms) or the metric,
// not the pipeline, becomes the reported staleness floor.
const mainPricePollInterval = 250 * time.Millisecond

// pollMainPrices reads latestPrice from the receiver contract on the main chain
// for each known asset and exports it as the chain="main" price series.
// A read failure is fatal, consistent with the relay's fail-fast ethos.
func pollMainPrices(ctx context.Context, main *evmClient, deployment Deployment, meters *metrics, fail func(error)) {
	ticker := time.NewTicker(mainPricePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, asset := range KnownAssets {
				output, err := main.CallContract(ctx, deployment.ReceiverAddress, packLatestPrice(asset.id))
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					fail(fmt.Errorf("poll main latestPrice for %s: %w", asset.name, err))
					return
				}
				price, updatedAt, err := decodeLatestPrice(output)
				if err != nil {
					fail(fmt.Errorf("decode main latestPrice for %s: %w", asset.name, err))
					return
				}
				meters.recordMainPrice(asset.name, price, updatedAt)
			}
		}
	}
}
