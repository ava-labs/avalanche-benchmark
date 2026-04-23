// Command blockwatch monitors blocks in real-time
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethclient"
)

func main() {
	var rpcURL string
	flag.StringVar(&rpcURL, "rpc", "", "RPC URL (required)")
	flag.Parse()

	if rpcURL == "" {
		// Try to read from default location
		data, err := os.ReadFile("./network_data/rpcs.txt")
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error: -rpc flag required or ./network_data/rpcs.txt must exist")
			flag.Usage()
			os.Exit(1)
		}
		rpcURL = strings.TrimSpace(strings.Split(string(data), ",")[0])
	}

	// Convert to WebSocket
	wsURL := strings.Replace(rpcURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "/rpc", "/ws", 1)

	fmt.Printf("Connecting to %s\n\n", wsURL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl+C
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	if err := watch(ctx, wsURL); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func watch(ctx context.Context, wsURL string) error {
	client, err := ethclient.Dial(wsURL)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer client.Close()

	headers := make(chan *types.Header, 10)
	sub, err := client.SubscribeNewHead(ctx, headers)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Unsubscribe()

	var lastTimestampMs uint64

	fmt.Println("Watching blocks...")

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-sub.Err():
			return err
		case header := <-headers:
			printBlock(ctx, client, header, &lastTimestampMs)
		}
	}
}

func printBlock(ctx context.Context, client *ethclient.Client, header *types.Header, lastTimestampMs *uint64) {
	// Fetch full block data
	var timestampMs, gasUsed, gasLimit uint64
	var txCount int

	var raw json.RawMessage
	err := client.Client().CallContext(ctx, &raw, "eth_getBlockByNumber", fmt.Sprintf("0x%x", header.Number.Uint64()), true)
	if err == nil && len(raw) > 0 {
		var blockData map[string]interface{}
		if json.Unmarshal(raw, &blockData) == nil {
			if tsMsHex, ok := blockData["timestampMilliseconds"].(string); ok {
				timestampMs, _ = parseHex(tsMsHex)
			}
			if gasUsedHex, ok := blockData["gasUsed"].(string); ok {
				gasUsed, _ = parseHex(gasUsedHex)
			}
			if gasLimitHex, ok := blockData["gasLimit"].(string); ok {
				gasLimit, _ = parseHex(gasLimitHex)
			}
			if txs, ok := blockData["transactions"].([]interface{}); ok {
				txCount = len(txs)
			}
		}
	}

	// Fallback
	if gasUsed == 0 {
		gasUsed = header.GasUsed
	}
	if gasLimit == 0 {
		gasLimit = header.GasLimit
	}

	// Calculate fill percentage
	fillPct := float64(0)
	if gasLimit > 0 {
		fillPct = float64(gasUsed) / float64(gasLimit) * 100
	}

	// Calculate interval
	intervalStr := "n/a"
	if *lastTimestampMs > 0 && timestampMs > *lastTimestampMs {
		interval := timestampMs - *lastTimestampMs
		intervalStr = fmt.Sprintf("%dms", interval)
	}
	*lastTimestampMs = timestampMs

	fmt.Printf("block=%d txs=%d gas=%.2fM fill=%.1f%% interval=%s\n",
		header.Number.Uint64(),
		txCount,
		float64(gasUsed)/1e6,
		fillPct,
		intervalStr,
	)
}

func parseHex(s string) (uint64, error) {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	return strconv.ParseUint(s, 16, 64)
}
