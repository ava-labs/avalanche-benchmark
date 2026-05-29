package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/rpc"
)

// blockInfo holds the fields we care about from the block response.
type blockInfo struct {
	Number                string   `json:"number"`
	Transactions          []string `json:"transactions"`
	TimestampMilliseconds string   `json:"timestampMilliseconds"`
	GasUsed               string   `json:"gasUsed"`
	GasLimit              string   `json:"gasLimit"`
}

// blockLog dedupes per-block prints across the racing watchers (one per
// endpoint) so each block is logged exactly once, and tracks the previous
// block's millisecond timestamp to report inter-block time.
var blockLog = struct {
	mu       sync.Mutex
	seen     map[uint64]bool
	lastNum  uint64
	prevTime uint64 // timestampMilliseconds of the last printed block
}{seen: make(map[uint64]bool)}

func logBlock(num uint64, txCount int, tsMs, gasUsed, gasLimit uint64) {
	blockLog.mu.Lock()
	defer blockLog.mu.Unlock()
	if blockLog.seen[num] {
		return
	}
	blockLog.seen[num] = true

	var dtMs int64
	if num > blockLog.lastNum {
		if blockLog.prevTime != 0 {
			dtMs = int64(tsMs) - int64(blockLog.prevTime)
		}
		blockLog.lastNum = num
		blockLog.prevTime = tsMs
	}

	fmt.Printf("block %d  txs=%d  dt=%dms  gas=%.1fm/%.1fm\n",
		num, txCount, dtMs, float64(gasUsed)/1e6, float64(gasLimit)/1e6)
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

			tsMs := hexToUint64(b.TimestampMilliseconds)
			logBlock(n, len(b.Transactions), tsMs, hexToUint64(b.GasUsed), hexToUint64(b.GasLimit))
			for _, hs := range b.Transactions {
				track.onMined(common.HexToHash(hs), observedAt)
			}
		}

		lastBlock = num
		time.Sleep(pollInterval)
	}
}
