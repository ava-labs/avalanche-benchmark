package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ava-labs/libevm/rpc"
)

// blockInfo holds the fields we care about from the block response
type blockInfo struct {
	Number                string   `json:"number"`
	GasUsed               string   `json:"gasUsed"`
	GasLimit              string   `json:"gasLimit"`
	Transactions          []string `json:"transactions"`
	TimestampMilliseconds string   `json:"timestampMilliseconds"`
}

func hexToUint64(hex string) uint64 {
	var val uint64
	fmt.Sscanf(hex, "0x%x", &val)
	return val
}

const windowSize = 10

func watchBlocks(ctx context.Context, rpcClient *rpc.Client) {
	// Start from the latest block
	var block blockInfo
	err := rpcClient.CallContext(ctx, &block, "eth_getBlockByNumber", "latest", false)
	if err != nil {
		fmt.Printf("Watcher: failed to get latest block: %v\n", err)
		return
	}
	lastBlock := hexToUint64(block.Number)
	lastTimestampMs := hexToUint64(block.TimestampMilliseconds)
	fmt.Printf("Watcher starting at block %d\n", lastBlock)

	// Rolling window for TPS calculation
	deltaMs := make([]uint64, 0, windowSize)
	txCounts := make([]int, 0, windowSize)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var block blockInfo
		err := rpcClient.CallContext(ctx, &block, "eth_getBlockByNumber", "latest", false)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		num := hexToUint64(block.Number)
		if num <= lastBlock {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Print any missed blocks
		for n := lastBlock + 1; n <= num; n++ {
			var b blockInfo
			if n < num {
				err = rpcClient.CallContext(ctx, &b, "eth_getBlockByNumber", fmt.Sprintf("0x%x", n), false)
				if err != nil {
					continue
				}
			} else {
				b = block
			}
			gasUsed := float64(hexToUint64(b.GasUsed)) / 1_000_000
			gasLimit := float64(hexToUint64(b.GasLimit)) / 1_000_000
			txCount := len(b.Transactions)
			timestampMs := hexToUint64(b.TimestampMilliseconds)
			delta := timestampMs - lastTimestampMs
			lastTimestampMs = timestampMs

			// Update rolling window
			if len(deltaMs) >= windowSize {
				deltaMs = deltaMs[1:]
				txCounts = txCounts[1:]
			}
			deltaMs = append(deltaMs, delta)
			txCounts = append(txCounts, txCount)

			// Calculate average TPS over window
			var totalMs uint64
			var totalTx int
			for i := range deltaMs {
				totalMs += deltaMs[i]
				totalTx += txCounts[i]
			}
			avgTps := float64(0)
			if totalMs > 0 {
				avgTps = float64(totalTx) / (float64(totalMs) / 1000)
			}

			fmt.Printf("Block %d: %d txs, %.2fM / %.2fM gas, %dms, avg TPS: %.0f\n", n, txCount, gasUsed, gasLimit, delta, avgTps)
		}

		lastBlock = num
		time.Sleep(100 * time.Millisecond)
	}
}
