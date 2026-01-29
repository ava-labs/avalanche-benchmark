package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethclient"
)

// BlockListener subscribes to new blocks and feeds them to Analytics.
type BlockListener struct {
	wsURL          string
	analytics      *Analytics
	silent         bool
	blockSignal    *atomic.Uint64 // Incremented on each block for worker coordination
	lastBlockTime  uint64         // Timestamp of last block (for interval calculation)
}

// NewBlockListener creates a listener with its own Analytics.
func NewBlockListener(httpURL string) *BlockListener {
	wsURL := strings.Replace(httpURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "/rpc", "/ws", 1)

	return &BlockListener{
		wsURL:       wsURL,
		analytics:   NewAnalytics(),
		blockSignal: &atomic.Uint64{},
	}
}

// Run starts listening for blocks. Reconnects on failure.
func (l *BlockListener) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := l.subscribe(ctx)
		if err != nil && ctx.Err() == nil {
			fmt.Printf("Block listener error: %v, reconnecting...\n", err)
			time.Sleep(2 * time.Second)
		}
	}
}

func (l *BlockListener) subscribe(ctx context.Context) error {
	fmt.Printf("Connecting to WebSocket: %s\n", l.wsURL)
	client, err := ethclient.Dial(l.wsURL)
	if err != nil {
		return fmt.Errorf("dial websocket: %w", err)
	}
	defer client.Close()

	fmt.Println("Subscribing to new blocks...")
	headers := make(chan *types.Header, 10)
	sub, err := client.SubscribeNewHead(ctx, headers)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Unsubscribe()

	fmt.Println("Block listener ready, waiting for blocks...")

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-sub.Err():
			return err
		case header := <-headers:
			l.processBlock(ctx, client, header)
		}
	}
}

func (l *BlockListener) processBlock(ctx context.Context, client *ethclient.Client, header *types.Header) {
	// Fetch block data to get timestampMilliseconds (Avalanche-specific)
	var timestampMs, gasUsed, gasLimit uint64

	var raw json.RawMessage
	err := client.Client().CallContext(ctx, &raw, "eth_getBlockByNumber", fmt.Sprintf("0x%x", header.Number.Uint64()), false)
	if err == nil && len(raw) > 0 {
		var blockData map[string]interface{}
		if json.Unmarshal(raw, &blockData) == nil {
			if tsMsHex, ok := blockData["timestampMilliseconds"].(string); ok {
				timestampMs, _ = parseHexUint64(tsMsHex)
			}
			if gasUsedHex, ok := blockData["gasUsed"].(string); ok {
				gasUsed, _ = parseHexUint64(gasUsedHex)
			}
			if gasLimitHex, ok := blockData["gasLimit"].(string); ok {
				gasLimit, _ = parseHexUint64(gasLimitHex)
			}
		}
	}

	// Fallback to header values
	if gasUsed == 0 {
		gasUsed = header.GasUsed
	}
	if gasLimit == 0 {
		gasLimit = header.GasLimit
	}

	// Calculate tx count from gas (native tx = 21000 gas)
	txCount := gasUsed / NativeGasLimit

	// Record in analytics
	l.analytics.RecordBlock(timestampMs, txCount, gasUsed, gasLimit)

	// Increment block signal for worker coordination
	l.blockSignal.Add(1)

	// Calculate interval from last block
	intervalStr := "n/a"
	if l.lastBlockTime > 0 && timestampMs > l.lastBlockTime {
		intervalMs := timestampMs - l.lastBlockTime
		intervalStr = fmt.Sprintf("%dms", intervalMs)
	}
	l.lastBlockTime = timestampMs

	// Always log blocks for visibility
	fmt.Printf("[Block %d] ~%d txs, gas: %.1fM (%.0f%%) interval=%s\n",
		header.Number.Uint64(),
		txCount,
		float64(gasUsed)/1e6,
		float64(gasUsed)/float64(gasLimit)*100,
		intervalStr,
	)
}

func parseHexUint64(s string) (uint64, error) {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	return strconv.ParseUint(s, 16, 64)
}

// SetSilent enables/disables printing (for TUI mode).
func (l *BlockListener) SetSilent(silent bool) {
	l.silent = silent
}

// SetAnalytics sets the analytics instance.
func (l *BlockListener) SetAnalytics(a *Analytics) {
	l.analytics = a
}

// Analytics returns the analytics instance for UI access.
func (l *BlockListener) Analytics() *Analytics {
	return l.analytics
}

// BlockSignal returns the current block signal counter.
func (l *BlockListener) BlockSignal() uint64 {
	return l.blockSignal.Load()
}
