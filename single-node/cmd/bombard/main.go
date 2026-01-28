// Command bombard is the transaction flooding tool for benchmarking.
// This is bundled with the benchmark CLI for air-gapped deployments.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ava-labs/avalanche-benchmark/internal/bombard"
)

func main() {
	var (
		rpcURLs   string
		batchSize int
		keyCount  int
		timeout   int
		erc20     bool
		both      bool
	)

	flag.StringVar(&rpcURLs, "rpc", "", "Comma-separated RPC URLs (default: read from ./network_data/rpcs.txt)")
	flag.IntVar(&batchSize, "batch", 500, "Transactions per batch")
	flag.IntVar(&keyCount, "keys", 500, "Number of parallel sender keys")
	flag.IntVar(&timeout, "timeout", 65, "Transaction confirmation timeout (seconds)")
	flag.BoolVar(&erc20, "erc20", false, "Send ERC20 token transfers instead of native transfers")
	flag.BoolVar(&both, "both", false, "Send both native and ERC20 transfers (alternating batches)")
	flag.Parse()

	if rpcURLs == "" {
		// Try to read from default location
		data, err := os.ReadFile("./network_data/rpcs.txt")
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error: -rpc flag is required or ./network_data/rpcs.txt must exist")
			flag.Usage()
			os.Exit(1)
		}
		rpcURLs = strings.TrimSpace(string(data))
		if rpcURLs == "" {
			fmt.Fprintln(os.Stderr, "Error: ./network_data/rpcs.txt is empty")
			os.Exit(1)
		}
	}

	if erc20 && both {
		fmt.Fprintln(os.Stderr, "Error: cannot use both -erc20 and -both flags")
		os.Exit(1)
	}

	urls := strings.Split(rpcURLs, ",")
	for i := range urls {
		urls[i] = strings.TrimSpace(urls[i])
	}

	// Determine transaction mode
	mode := bombard.TxModeNative
	if erc20 {
		mode = bombard.TxModeERC20
	} else if both {
		mode = bombard.TxModeBoth
	}

	cfg := bombard.Config{
		RPCURLs:        urls,
		BatchSize:      batchSize,
		KeyCount:       keyCount,
		TimeoutSeconds: timeout,
		Mode:           mode,
	}

	// Handle Ctrl+C to exit immediately
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	errChan := make(chan error, 1)
	go func() {
		errChan <- bombard.Run(cfg)
	}()

	select {
	case sig := <-sigChan:
		_ = sig
		os.Exit(0)
	case err := <-errChan:
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}
}
