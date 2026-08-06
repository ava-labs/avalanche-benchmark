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

	// Per-block line intentionally not printed; STATS lines carry the signal.
	if num > blockLog.lastNum {
		blockLog.lastNum = num
		blockLog.prevTime = tsMs
	}
}

func hexToUint64(hex string) uint64 {
	var val uint64
	fmt.Sscanf(hex, "0x%x", &val)
	return val
}

// watchBlocks polls a node for new blocks and marks each of our transactions
// mined as it appears. It OWNS its websocket connection and RECONNECTS when the
// connection drops, essential across a failover/restore, where the watched node
// restarts. The old version dialed once and, on a dead connection, spun on the
// dead socket forever (and died outright if the very first call failed); bombard
// was then left detecting mines only via a surviving watcher, which after a
// restore is a cross-region tracker lagging the tip, inflating observed
// mine-latency and throttling throughput via the in-flight cap until a manual
// restart. Now any RPC error redials a fresh socket and retries; resubmission
// keeps txs alive in the meantime. lastBlock is preserved across reconnects, so
// blocks produced during the gap are still marked once the node catches back up.
func watchBlocks(ctx context.Context, wsURL string, pollInterval time.Duration) {
	// dial (re)establishes the websocket, retrying until connected or ctx ends.
	dial := func() *rpc.Client {
		for {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			if c, err := rpc.DialWebsocket(ctx, wsURL, ""); err == nil {
				return c
			}
			time.Sleep(time.Second)
		}
	}
	// getBlock fetches a block under a bounded per-call timeout, so a half-open
	// socket can't hang the watcher (it errors out and triggers a redial).
	getBlock := func(c *rpc.Client, out *blockInfo, tag string) error {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return c.CallContext(cctx, out, "eth_getBlockByNumber", tag, false)
	}

	client := dial()
	if client == nil {
		return
	}
	defer func() {
		// client is reassigned on redial and can be nil if a redial fails
		// (e.g. ctx cancelled at shutdown), so guard before closing.
		if client != nil {
			client.Close()
		}
	}()

	var lastBlock uint64
	haveLast := false

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var block blockInfo
		err := getBlock(client, &block, "latest")
		observedAt := time.Now()
		if err != nil {
			client.Close() // socket likely dropped (node restart), redial, don't spin on it
			if client = dial(); client == nil {
				return
			}
			continue
		}

		num := hexToUint64(block.Number)
		switch {
		case !haveLast:
			lastBlock, haveLast = num, true
			time.Sleep(pollInterval)
			continue
		case num <= lastBlock:
			time.Sleep(pollInterval)
			continue
		}

		for n := lastBlock + 1; n <= num; n++ {
			var b blockInfo
			if n < num {
				if err := getBlock(client, &b, fmt.Sprintf("0x%x", n)); err != nil {
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
