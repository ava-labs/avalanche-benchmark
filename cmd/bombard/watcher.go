package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/rpc"
)

// blockInfo holds the fields we care about from the block response.
type blockInfo struct {
	Number                string   `json:"number"`
	Transactions          []string `json:"transactions"`
	TimestampMilliseconds string   `json:"timestampMilliseconds"`
}

func hexToUint64(hex string) uint64 {
	var val uint64
	fmt.Sscanf(hex, "0x%x", &val)
	return val
}

// watchBlocks polls for new blocks and marks each of our transactions mined as
// it appears. It is intentionally tolerant: any RPC error just sleeps and
// retries, so a node hiccup never crashes the watcher (resubmission keeps the
// txs alive in the meantime).
func watchBlocks(ctx context.Context, rpcClient *rpc.Client, pollInterval time.Duration) {
	var block blockInfo
	if err := rpcClient.CallContext(ctx, &block, "eth_getBlockByNumber", "latest", false); err != nil {
		fmt.Printf("Watcher: failed to get latest block: %v\n", err)
		return
	}
	lastBlock := hexToUint64(block.Number)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var block blockInfo
		err := rpcClient.CallContext(ctx, &block, "eth_getBlockByNumber", "latest", false)
		observedAt := time.Now()
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}

		num := hexToUint64(block.Number)
		if num <= lastBlock {
			time.Sleep(pollInterval)
			continue
		}

		for n := lastBlock + 1; n <= num; n++ {
			var b blockInfo
			if n < num {
				if err := rpcClient.CallContext(ctx, &b, "eth_getBlockByNumber", fmt.Sprintf("0x%x", n), false); err != nil {
					continue
				}
				observedAt = time.Now()
			} else {
				b = block
			}

			blockTime := time.UnixMilli(int64(hexToUint64(b.TimestampMilliseconds)))
			for _, hs := range b.Transactions {
				track.onMined(common.HexToHash(hs), blockTime, observedAt)
			}
		}

		lastBlock = num
		time.Sleep(pollInterval)
	}
}
