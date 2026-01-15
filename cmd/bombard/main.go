// Command bombard is the transaction flooding tool for benchmarking.
// This is bundled with the benchmark CLI for air-gapped deployments.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

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

	flag.StringVar(&rpcURLs, "rpc", "", "Comma-separated RPC URLs")
	flag.IntVar(&batchSize, "batch", 50, "Transactions per batch")
	flag.IntVar(&keyCount, "keys", 600, "Number of parallel sender keys")
	flag.IntVar(&timeout, "timeout", 10, "Transaction confirmation timeout (seconds)")
	flag.BoolVar(&erc20, "erc20", false, "Send ERC20 token transfers instead of native transfers")
	flag.BoolVar(&both, "both", false, "Send both native and ERC20 transfers (alternating batches)")
	flag.Parse()

	if rpcURLs == "" {
		fmt.Fprintln(os.Stderr, "Error: -rpc flag is required")
		flag.Usage()
		os.Exit(1)
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

	if err := bombard.Run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
