package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ava-labs/libevm/rpc"
)

// blockInfo holds the fields we read from an eth_getBlockByNumber response. We
// make the raw RPC call (not ethclient) specifically for timestampMilliseconds:
// the proposerVM millisecond block time, which the standard go-ethereum header
// type drops. Inter-block gap and TPS are derived from these block timestamps,
// not from scrape sampling — same source bombard's watcher reads.
type blockInfo struct {
	Number                string   `json:"number"`
	Transactions          []string `json:"transactions"`
	TimestampMilliseconds string   `json:"timestampMilliseconds"`
	GasUsed               string   `json:"gasUsed"`
	GasLimit              string   `json:"gasLimit"`
}

func hexToUint64(hex string) uint64 {
	var val uint64
	fmt.Sscanf(hex, "0x%x", &val)
	return val
}

// watchBlocks polls one RPC endpoint for new blocks and feeds every block it
// sees (backfilling any it skipped between polls) into the site aggregator,
// which dedupes across the per-site endpoints. It OWNS its client and RECONNECTS
// on any RPC error — essential across a failover/restore, where the watched node
// restarts: the old socket is dropped and a fresh one dialed rather than spinning
// on a dead connection. lastBlock is preserved across reconnects so blocks
// produced during the gap are still accounted once the node catches back up.
func watchBlocks(ctx context.Context, rpcURL string, agg *siteState, pollInterval time.Duration) {
	dial := func() *rpc.Client {
		for {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			if c, err := rpc.DialContext(ctx, rpcURL); err == nil {
				return c
			}
			time.Sleep(time.Second)
		}
	}
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
			client.Close() // socket likely dropped (node restart) — redial, don't spin on it
			if client = dial(); client == nil {
				return
			}
			continue
		}

		num := hexToUint64(block.Number)
		switch {
		case !haveLast:
			lastBlock, haveLast = num, true
			agg.observe(num, hexToUint64(block.TimestampMilliseconds), len(block.Transactions), observedAt)
			time.Sleep(pollInterval)
			continue
		case num <= lastBlock:
			time.Sleep(pollInterval)
			continue
		}

		// Backfill every block produced since our last poll so per-block tx
		// counts and gaps are exact, not amortized over the poll interval.
		for n := lastBlock + 1; n <= num; n++ {
			b := block
			if n < num {
				if err := getBlock(client, &b, fmt.Sprintf("0x%x", n)); err != nil {
					continue
				}
				observedAt = time.Now()
			}
			agg.observe(n, hexToUint64(b.TimestampMilliseconds), len(b.Transactions), observedAt)
		}

		lastBlock = num
		time.Sleep(pollInterval)
	}
}
