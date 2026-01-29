// Command bombard is a stable EVM transaction flooding tool using pull-based workers.
// Each worker maintains exactly 1 transaction in the mempool at all times.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	var (
		rpcURLs string
		workers int
	)

	flag.StringVar(&rpcURLs, "rpc", "", "Comma-separated RPC URLs (required, or reads from ./network_data/rpcs.txt)")
	flag.IntVar(&workers, "workers", 1000, "Number of concurrent workers")
	flag.Parse()

	// Load RPC URLs
	if rpcURLs == "" {
		data, err := os.ReadFile("./network_data/rpcs.txt")
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error: -rpc flag required or ./network_data/rpcs.txt must exist")
			flag.Usage()
			os.Exit(1)
		}
		rpcURLs = strings.TrimSpace(string(data))
	}

	if rpcURLs == "" {
		fmt.Fprintln(os.Stderr, "Error: no RPC URLs provided")
		os.Exit(1)
	}

	urls := parseURLs(rpcURLs)
	if len(urls) == 0 {
		fmt.Fprintln(os.Stderr, "Error: no valid RPC URLs")
		os.Exit(1)
	}

	cfg := Config{
		RPCURLs: urls,
		Workers: workers,
	}

	// Setup immediate exit on Ctrl+C
	ctx := context.Background()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nExiting...")
		os.Exit(0)
	}()

	// Run bombardment
	if err := Run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func parseURLs(s string) []string {
	parts := strings.Split(s, ",")
	urls := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			urls = append(urls, p)
		}
	}
	return urls
}
