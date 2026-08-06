package oraclerelay

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/rpc"
	pblock "github.com/ava-labs/avalanchego/vms/platformvm/block"
	proposerblock "github.com/ava-labs/avalanchego/vms/proposervm/block"
)

var jst = time.FixedZone("JST", 9*60*60)

// epochClient is the subset of proposervm.JSONRPCClient the gate needs.
type epochClient interface {
	GetCurrentEpoch(context.Context, ...rpc.Option) (proposerblock.Epoch, error)
}

// blockByHeightClient is the subset of platformvm.Client used to locate the
// oracle conversion block.
type blockByHeightClient interface {
	GetBlockByHeight(context.Context, uint64, ...rpc.Option) ([]byte, error)
}

// conversionTime derives the oracle conversion timestamp from the oracle
// validators' shared StartTime, mirroring setweight.managerConversionTime.
func conversionTime(startTimes []uint64) (time.Time, error) {
	if len(startTimes) == 0 {
		return time.Time{}, fmt.Errorf("oracle L1 has no validators")
	}
	startTime := startTimes[0]
	if startTime == 0 {
		return time.Time{}, fmt.Errorf("oracle validator has no start time")
	}
	for _, other := range startTimes[1:] {
		if other != startTime {
			return time.Time{}, fmt.Errorf("oracle validators have different conversion times: %d and %d", startTime, other)
		}
	}
	return time.Unix(int64(startTime), 0), nil
}

// findConversionHeight locates the P-chain block that included the oracle
// ConvertSubnetToL1Tx. Lifted from setweight.findConversionHeight: binary search
// by monotonic block timestamp to the conversion second, then scan that second's
// blocks for the transaction ID. Derives a public-chain fact and stores nothing.
func findConversionHeight(
	ctx context.Context,
	blocks blockByHeightClient,
	tipHeight uint64,
	convertedAt time.Time,
	conversionTxID ids.ID,
) (uint64, error) {
	low, high := uint64(0), tipHeight
	for low < high {
		mid := low + (high-low)/2
		timestamp, _, err := readBlock(ctx, blocks, mid)
		if err != nil {
			return 0, err
		}
		if timestamp.IsZero() || timestamp.Before(convertedAt) {
			low = mid + 1
		} else {
			high = mid
		}
	}
	for height := low; ; height++ {
		timestamp, txIDs, err := readBlock(ctx, blocks, height)
		if err != nil {
			return 0, err
		}
		if !timestamp.Equal(convertedAt) {
			break
		}
		for _, txID := range txIDs {
			if txID == conversionTxID {
				return height, nil
			}
		}
		if height == tipHeight {
			break
		}
	}
	return 0, fmt.Errorf("oracle conversion transaction %s was not found in P-chain blocks stamped %s", conversionTxID, convertedAt.In(jst).Format("2006-01-02 15:04:05 MST"))
}

func readBlock(ctx context.Context, blocks blockByHeightClient, height uint64) (time.Time, []ids.ID, error) {
	blockBytes, err := blocks.GetBlockByHeight(ctx, height)
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("read P-chain block %d: %w", height, err)
	}
	parsed, err := pblock.Parse(pblock.Codec, blockBytes)
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("parse P-chain block %d: %w", height, err)
	}
	banff, ok := parsed.(pblock.BanffBlock)
	if !ok {
		return time.Time{}, nil, nil
	}
	txs := parsed.Txs()
	txIDs := make([]ids.ID, len(txs))
	for i, tx := range txs {
		txIDs[i] = tx.ID()
	}
	return banff.Timestamp(), txIDs, nil
}

// gateOracleVisibility blocks until the MAIN chain's Warp verifier sees the
// oracle validator set. ACP-181 freezes the verifier's validator view at the
// current epoch's PChainHeight; if that height predates the oracle conversion,
// the main chain rejects our signatures no matter how much weight we hold. Unlike
// setweight's quiet P-chain, the main chain is under benchmark load and advances
// epochs on its own, so we only sleep to the seal boundary and poll; no nudge.
func gateOracleVisibility(
	ctx context.Context,
	epochs epochClient,
	conversionHeight uint64,
	epochDuration time.Duration,
	now time.Time,
	output io.Writer,
) error {
	epoch, err := epochs.GetCurrentEpoch(ctx)
	if err != nil {
		return fmt.Errorf("read current main-chain Warp epoch: %w", err)
	}
	if epoch.PChainHeight >= conversionHeight {
		return nil
	}

	sealTime := time.Unix(epoch.StartTime, 0).Add(epochDuration)
	if now.Before(sealTime) {
		remaining := sealTime.Sub(now)
		displayRemaining := ((remaining + time.Second - 1) / time.Second) * time.Second
		fmt.Fprintf(
			output,
			"oracle conversion is not visible to the main chain's Warp epoch yet; sleeping for %s until %s\n",
			displayRemaining,
			sealTime.In(jst).Format("2006-01-02 15:04:05 MST"),
		)
		if err := sleep(ctx, remaining); err != nil {
			return fmt.Errorf("wait for main-chain Warp epoch %d to seal: %w", epoch.Number, err)
		}
	}

	// After the boundary the load-driven main chain seals the epoch within a few
	// blocks; poll until the pinned height covers the conversion.
	deadline := time.Now().Add(30 * time.Second)
	for {
		epoch, err = epochs.GetCurrentEpoch(ctx)
		if err != nil {
			return fmt.Errorf("read current main-chain Warp epoch: %w", err)
		}
		if epoch.PChainHeight >= conversionHeight {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"oracle conversion at P-chain height %d is still not visible to main-chain Warp epoch %d pinned at height %d",
				conversionHeight,
				epoch.Number,
				epoch.PChainHeight,
			)
		}
		if err := sleep(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
