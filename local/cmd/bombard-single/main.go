package main

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/rpc"
)

const (
	// EWOQ is the pre-funded test key for Avalanche local networks.
	ewoqPrivateKey = "56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027"

	defaultTps = 1000

	gasLimitNative = 21000
	gasPrice       = 25

	signQueueSize = 1024
)

type signJob struct {
	nonce uint64
}

func main() {
	rpcURL := flag.String("rpc", "", "RPC URL for submitting txs (auto-detected from network_data/rpcs.txt if omitted)")
	wsURL := flag.String("ws", "", "WebSocket URL for submitting txs (auto-derived from --rpc if omitted)")
	watchRPCURL := flag.String("watch-rpc", "", "RPC URL for the block watcher (defaults to --rpc)")
	watchWSURL := flag.String("watch-ws", "", "WebSocket URL for the block watcher (auto-derived from --watch-rpc if omitted)")
	defaultWSConns := runtime.NumCPU() * 10
	wsConns := flag.Int("ws-conns", defaultWSConns, "Number of WebSocket connections in the worker pool")
	watchInterval := flag.Duration("watch-interval", time.Millisecond, "How often the watcher polls for new blocks")
	confirmSource := flag.String("confirm-source", "block", "Confirmation source: block or accepted-sub")
	targetTps := flag.Int("tps", defaultTps, "Target transactions per second")
	targetTxs := flag.Uint64("txs", 0, "Stop after at least this many landed transactions; 0 means run until interrupted")
	runDuration := flag.Duration("duration", 0, "Stop after this duration; 0 means run until interrupted or --txs is reached")
	dataDir := flag.String("data-dir", "./network_data", "Network data directory (for auto-detecting RPC URL)")
	signWorkers := flag.Int("sign-workers", runtime.NumCPU(), "Number of goroutines that sign+submit txs in parallel")
	flag.Parse()

	if *confirmSource != "block" && *confirmSource != "accepted-sub" {
		fmt.Printf("Invalid --confirm-source=%q; expected block or accepted-sub\n", *confirmSource)
		os.Exit(1)
	}

	if *rpcURL == "" {
		rpcsFile := filepath.Join(*dataDir, "rpcs.txt")
		data, err := os.ReadFile(rpcsFile)
		if err != nil {
			fmt.Printf("No --rpc provided and failed to read %s: %v\n", rpcsFile, err)
			os.Exit(1)
		}
		urls := strings.Split(strings.TrimSpace(string(data)), ",")
		if len(urls) == 0 || urls[0] == "" {
			fmt.Printf("No RPC URLs found in %s\n", rpcsFile)
			os.Exit(1)
		}
		*rpcURL = urls[0]
		fmt.Printf("Auto-detected RPC URL from %s\n", rpcsFile)
	}
	if *wsURL == "" {
		*wsURL = httpRPCToWS(*rpcURL)
	}
	if *watchRPCURL == "" {
		*watchRPCURL = *rpcURL
	}
	if *watchWSURL == "" {
		*watchWSURL = httpRPCToWS(*watchRPCURL)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Printf("\nSignal %v received, stopping...\n", sig)
		cancel()
	}()

	if *runDuration > 0 {
		go func() {
			timer := time.NewTimer(*runDuration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
			case <-timer.C:
				fmt.Printf("Duration target reached: %s\n", runDuration.String())
				cancel()
			}
		}()
	}

	if *targetTxs > 0 {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}

				_, landed, _, _ := tracker.counts()
				if landed >= *targetTxs {
					fmt.Printf("Transaction target reached: landed=%d target=%d\n", landed, *targetTxs)
					cancel()
					return
				}
			}
		}()
	}

	pool, err := newWSPool(ctx, *wsURL, *wsConns)
	if err != nil {
		fmt.Printf("Failed to open WS pool: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()
	fmt.Printf("Opened %d WS connections to %s\n", *wsConns, *wsURL)

	watcherRPC, err := rpc.DialWebsocket(ctx, *watchWSURL, "")
	if err != nil {
		fmt.Printf("Failed to dial watcher WS: %v\n", err)
		os.Exit(1)
	}
	defer watcherRPC.Close()
	if *watchWSURL != *wsURL {
		fmt.Printf("Watcher WS: %s (separate from submission pool)\n", *watchWSURL)
	}

	setupRPC, err := rpc.DialWebsocket(ctx, *wsURL, "")
	if err != nil {
		fmt.Printf("Failed to dial setup WS: %v\n", err)
		os.Exit(1)
	}
	defer setupRPC.Close()
	client := ethclient.NewClient(setupRPC)

	chainID, err := client.NetworkID(ctx)
	if err != nil {
		fmt.Printf("Failed to get chain ID: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Chain ID: %s\n", chainID)

	privateKey, err := crypto.HexToECDSA(ewoqPrivateKey)
	if err != nil {
		fmt.Printf("Failed to load key: %v\n", err)
		os.Exit(1)
	}
	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	signer := types.NewEIP155Signer(chainID)
	fmt.Printf("Sender: %s\n", address.Hex())

	startNonce, err := client.PendingNonceAt(ctx, address)
	if err != nil {
		fmt.Printf("Failed to get starting nonce: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Starting nonce: %d\n", startNonce)

	go watchBlocks(ctx, watcherRPC, *watchInterval, *confirmSource == "block")
	if *confirmSource == "accepted-sub" {
		go watchAcceptedTransactions(ctx, watcherRPC)
	}
	go tracker.reportLoop(ctx)
	go tracker.printTableLoop(ctx)

	// Sign+submit worker pool. Each worker pulls a job (nonce only), builds the
	// tx, signs, and submits via the shared wsPool. Multiple workers parallelize
	// signing across cores; submission concurrency is capped by the wsPool size.
	signCh := make(chan signJob, signQueueSize)
	var wg sync.WaitGroup
	for i := 0; i < *signWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range signCh {
				tx := types.NewTransaction(
					job.nonce,
					address,
					big.NewInt(1),
					gasLimitNative,
					big.NewInt(gasPrice),
					nil,
				)
				signed, err := types.SignTx(tx, signer, privateKey)
				if err != nil {
					fmt.Printf("sign error nonce=%d: %v\n", job.nonce, err)
					continue
				}
				sendStart := time.Now()
				err = pool.Do(ctx, func(c *ethclient.Client) error {
					return c.SendTransaction(ctx, signed)
				})
				if err != nil {
					if ctx.Err() == nil {
						fmt.Printf("submit error nonce=%d: %v\n", job.nonce, err)
					}
					continue
				}
				sendEnd := time.Now()
				tracker.markSubmitted(signed.Hash(), 0, sendStart, sendEnd)
			}
		}()
	}

	fmt.Printf("\nSingle-sender catch-up loop: target %d tps, %d sign workers, 1ms tick\n\n", *targetTps, *signWorkers)

	// Catch-up controller: every ~1ms, compute target = elapsed_ms * tps / 1000
	// and enqueue (target - sent) signing jobs. Self-correcting: a stalled tick
	// is recovered on the next tick because elapsed time keeps moving. If
	// signers can't drain signCh fast enough, the channel send blocks here,
	// providing natural backpressure — actual rate becomes min(target, signer
	// throughput).
	start := time.Now()
	var sent uint64
	nonce := startNonce

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
		}

		elapsedMs := uint64(time.Since(start).Milliseconds())
		target := elapsedMs * uint64(*targetTps) / 1000
		for sent < target {
			select {
			case signCh <- signJob{nonce: nonce}:
				nonce++
				sent++
			case <-ctx.Done():
				break loop
			}
		}
	}

	close(signCh)
	wg.Wait()

	submitted, landed, timeouts, pending := tracker.counts()
	fmt.Printf("FINAL submitted=%d landed=%d timeouts=%d pending=%d\n", submitted, landed, timeouts, pending)
}
