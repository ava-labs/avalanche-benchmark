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
	deliveryGasLimit = 600000
	// We attach exactly one warp predicate, so receivePrice always references
	// access-list index 0.
	warpPredicateIndex uint32 = 0
	// Deliveries are sent as messages arrive and confirmed asynchronously; this
	// bounds how many unconfirmed txs may be in flight before send blocks (that
	// back-pressure is intentional).
	maxInFlight = 256
	// A delivery that is not mined within this window is fatal.
	confirmTimeout = 30 * time.Second
	// Deliveries pay a premium over the suggested gas price so they out-bid
	// benchmark flood traffic: bombard pays the minimum fee, and the block builder
	// orders txs by effective gas price, so a 10x premium keeps oracle updates at
	// the front of the queue under load. The floor guards against a zero suggestion.
	deliveryGasPriceMultiplier = 10
	minDeliveryGasPrice        = 10 // wei
)

// priorityGasPrice applies the delivery premium and floor.
func priorityGasPrice(suggested *big.Int) *big.Int {
	price := new(big.Int).Mul(suggested, big.NewInt(deliveryGasPriceMultiplier))
	if floor := big.NewInt(minDeliveryGasPrice); price.Cmp(floor) < 0 {
		return floor
	}
	return price
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

// pendingDelivery is a sent-but-unconfirmed delivery handed to the confirmer.
// seenAt and updatedAt travel with it so latency histograms can be observed at
// confirmation time.
type pendingDelivery struct {
	asset     string
	hash      ethcommon.Hash
	seenAt    time.Time
	updatedAt uint64
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
			// The ws-event-seen clock starts the moment the log is dequeued.
			seenAt := time.Now()
			item, err := deliver(runCtx, pChain, mainEpochs, main, mainSigner, feederKey, signers, &nonce, &cache, fresh, deployment, entry, seenAt, meters, output)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, errStaleSkipped) {
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

// deliver signs a single aggregator broadcast and sends the delivery tx, bumping
// the feeder nonce. Confirmation happens asynchronously; the returned item is
// handed to the confirmer.
// errStaleSkipped marks a message dropped by the freshness gate; it is the one
// non-fatal deliver outcome.
var errStaleSkipped = errors.New("stale message skipped")

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

func deliver(
	ctx context.Context,
	pChain *platformvm.Client,
	mainEpochs epochClient,
	main *evmClient,
	mainSigner ethtypes.Signer,
	feederKey *ecdsa.PrivateKey,
	signers []bls.Signer,
	nonce *uint64,
	cache *canonicalSetCache,
	fresh *freshnessGate,
	deployment Deployment,
	log ethtypes.Log,
	seenAt time.Time,
	meters *metrics,
	output io.Writer,
) (pendingDelivery, error) {
	unsignedBytes, err := UnpackEventMessage(log.Data)
	if err != nil {
		return pendingDelivery{}, fmt.Errorf("oracle block %d: %w", log.BlockNumber, err)
	}
	submission, err := ParseSubmission(unsignedBytes)
	if err != nil {
		return pendingDelivery{}, fmt.Errorf("oracle block %d: %w", log.BlockNumber, err)
	}
	// Freshness is sequence-based: deliver only when seq exceeds the last seq
	// delivered for this asset, skipping duplicates/reorders (e.g. on ws
	// reconnect) instead of burning a guaranteed stale-update revert.
	seq := submission.Seq.Uint64()
	if !fresh.fresher(submission.AssetID, seq) {
		meters.recordSkipped(AssetName(submission.AssetID))
		fmt.Fprintf(output, "skipped %s oracle-block %d: seq %d not fresher\n", AssetName(submission.AssetID), log.BlockNumber, seq)
		return pendingDelivery{}, errStaleSkipped
	}
	unsigned, err := warp.ParseUnsignedMessage(unsignedBytes)
	if err != nil {
		return pendingDelivery{}, fmt.Errorf("oracle block %d: parse unsigned Warp message: %w", log.BlockNumber, err)
	}

	// The verifier uses the validator set pinned by the main chain's CURRENT
	// proposervm epoch, so the canonical bit positions must be built at exactly
	// that P-chain height. The set is cached until the pinned height changes.
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
	signed, err := signAndAggregate(unsigned, cache.set, signers)
	if err != nil {
		return pendingDelivery{}, err
	}

	suggestedGasPrice, err := main.GasPrice(ctx)
	if err != nil {
		return pendingDelivery{}, fmt.Errorf("read main gas price: %w", err)
	}
	gasPrice := priorityGasPrice(suggestedGasPrice)
	receiver := deployment.ReceiverAddress
	tx := ethtypes.NewTx(&ethtypes.AccessListTx{
		ChainID:  mainSigner.ChainID(),
		Nonce:    *nonce,
		GasPrice: gasPrice,
		Gas:      deliveryGasLimit,
		To:       &receiver,
		Value:    big.NewInt(0),
		Data:     packReceivePrice(warpPredicateIndex),
		// The signed Warp message rides as a predicate keyed by the Warp
		// precompile; the verifier reads it from this access-list entry.
		AccessList: ethtypes.AccessList{{
			Address:     WarpPrecompileAddress,
			StorageKeys: PackPredicate(signed.Bytes()),
		}},
	})
	signedTx, err := ethtypes.SignTx(tx, mainSigner, feederKey)
	if err != nil {
		return pendingDelivery{}, fmt.Errorf("sign delivery tx: %w", err)
	}
	rawTx, err := signedTx.MarshalBinary()
	if err != nil {
		return pendingDelivery{}, fmt.Errorf("encode delivery tx: %w", err)
	}
	asset := AssetName(submission.AssetID)
	hash, err := main.SendRawTransaction(ctx, rawTx)
	if err != nil {
		return pendingDelivery{}, fmt.Errorf("deliver price for %s: %w", asset, err)
	}
	*nonce++
	updatedAt := submission.UpdatedAt.Uint64()
	meters.recordDelivery(asset, submission.Price, updatedAt)
	meters.recordSeq(asset, seq)
	fmt.Fprintf(output, "delivered %s price %s seq %d oracle-block %d main-tx %s\n", asset, formatPrice(submission.Price.Int64()), seq, log.BlockNumber, hash.Hex())
	return pendingDelivery{asset: asset, hash: hash, seenAt: seenAt, updatedAt: updatedAt}, nil
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
				fail(fmt.Errorf("await delivery receipt for %s tx %s: %w", item.asset, item.hash.Hex(), err))
				return
			}
			if r.Status != 1 {
				fail(fmt.Errorf("delivery for %s reverted: tx %s in block %d", item.asset, item.hash.Hex(), r.BlockNumber))
				return
			}
			meters.recordConfirmation(item.asset, item.seenAt, item.updatedAt, time.Now())
			fmt.Fprintf(output, "confirmed %s tx %s block %d\n", item.asset, item.hash.Hex(), r.BlockNumber)
		}
	}
}

// pollMainPrices reads latestPrice from the receiver contract on the main chain
// every 2s for each known asset and exports it as the chain="main" price series.
// A read failure is fatal, consistent with the relay's fail-fast ethos.
func pollMainPrices(ctx context.Context, main *evmClient, deployment Deployment, meters *metrics, fail func(error)) {
	ticker := time.NewTicker(2 * time.Second)
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
