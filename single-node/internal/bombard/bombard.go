// Package bombard provides EVM transaction flooding capabilities for benchmarking.
// Adapted from github.com/ava-labs/devrel-experiments/04_evmbombard
package bombard

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/rpc"
)

// Shared HTTP transport with aggressive connection pooling
var sharedTransport = &http.Transport{
	MaxIdleConns:        1000,
	MaxIdleConnsPerHost: 1000,
	MaxConnsPerHost:     1000,
	IdleConnTimeout:     90 * time.Second,
	DisableKeepAlives:   false,
}

var sharedHTTPClient = &http.Client{
	Transport: sharedTransport,
	Timeout:   30 * time.Second,
}

// dialWithSharedTransport creates an ethclient using the shared HTTP transport
func dialWithSharedTransport(url string) (*ethclient.Client, error) {
	rpcClient, err := rpc.DialHTTPWithClient(url, sharedHTTPClient)
	if err != nil {
		return nil, err
	}
	return ethclient.NewClient(rpcClient), nil
}

// TxMode specifies what type of transactions to send.
type TxMode string

const (
	// TxModeNative sends native token transfers (default)
	TxModeNative TxMode = "native"
	// TxModeERC20 sends ERC20 token transfers using the predeployed Benchmark Token
	TxModeERC20 TxMode = "erc20"
	// TxModeBoth sends both native and ERC20 transfers (alternating)
	TxModeBoth TxMode = "both"
)

// Config holds the configuration for the bombardment.
type Config struct {
	RPCURLs        []string
	BatchSize      int
	KeyCount       int
	TimeoutSeconds int
	Data           []byte // Optional contract call data
	ToAddress      string // Optional target address (empty = random)
	Mode           TxMode // Transaction mode: native, erc20, or both
}

// DefaultConfig returns a default configuration.
func DefaultConfig() Config {
	return Config{
		BatchSize:      50,
		KeyCount:       600,
		TimeoutSeconds: 10,
		Mode:           TxModeNative,
	}
}

// Run starts the bombardment with the given configuration.
// It blocks until interrupted or an error occurs.
func Run(cfg Config) error {
	if len(cfg.RPCURLs) == 0 {
		return fmt.Errorf("no RPC URLs provided")
	}

	// Set package-level variables
	batchSize = cfg.BatchSize
	keyCount = cfg.KeyCount
	timeoutSeconds = cfg.TimeoutSeconds

	// Default to native mode if not specified
	if cfg.Mode == "" {
		cfg.Mode = TxModeNative
	}

	fmt.Printf("Starting with batch size: %d, key count: %d, mode: %s\n", batchSize, keyCount, cfg.Mode)
	fmt.Printf("Using %d RPC URLs\n", len(cfg.RPCURLs))

	// Use first RPC URL for primary client
	rpcURL := cfg.RPCURLs[0]

	client, err := dialWithSharedTransport(rpcURL)
	if err != nil {
		return fmt.Errorf("failed to connect to RPC: %w", err)
	}
	defer client.Close()

	// Create WebSocket URL from HTTP URL
	wsURL := strings.Replace(rpcURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "/rpc", "/ws", 1)

	listener, err := NewTxListener(wsURL)
	if err != nil {
		return fmt.Errorf("failed to create listener: %w", err)
	}
	defer listener.Close()

	// Generate keys
	keys := generateKeys(keyCount)

	// Fund accounts with native tokens (needed for gas in all modes)
	fmt.Printf("funding %d accounts with native tokens\n", keyCount)
	if err := fundAccounts(client, listener, keys); err != nil {
		return fmt.Errorf("failed to fund accounts: %w", err)
	}
	fmt.Println("all accounts funded with native tokens")

	// Fund accounts with ERC20 tokens if needed
	if cfg.Mode == TxModeERC20 || cfg.Mode == TxModeBoth {
		fmt.Printf("funding %d accounts with ERC20 tokens\n", keyCount)
		if err := fundAccountsWithERC20(client, listener, keys); err != nil {
			return fmt.Errorf("failed to fund accounts with ERC20: %w", err)
		}
		fmt.Println("all accounts funded with ERC20 tokens")
	}

	// Create shared client pool (one per RPC URL, shared across workers)
	// ethclient is goroutine-safe for concurrent requests
	clientPool := make([]*ethclient.Client, len(cfg.RPCURLs))
	for i, url := range cfg.RPCURLs {
		c, err := dialWithSharedTransport(url)
		if err != nil {
			// Close already opened clients
			for j := 0; j < i; j++ {
				clientPool[j].Close()
			}
			return fmt.Errorf("failed to create client pool: %w", err)
		}
		clientPool[i] = c
	}
	defer func() {
		for _, c := range clientPool {
			c.Close()
		}
	}()

	// Determine target address
	var toAddress common.Address
	if cfg.ToAddress != "" {
		toAddress = common.HexToAddress(cfg.ToAddress)
	}

	// Start bombardment goroutines
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	for i, key := range keys {
		wg.Add(1)
		go func(idx int, k *Key, mode TxMode) {
			defer wg.Done()

			// Use shared client from pool (round-robin distribution)
			sharedClient := clientPool[idx%len(clientPool)]

			// Use random address if not specified
			target := toAddress
			if target == (common.Address{}) {
				target = common.HexToAddress(fmt.Sprintf("0x%040x", idx+1))
			}

			switch mode {
			case TxModeERC20:
				bombardWithERC20Transactions(ctx, sharedClient, k.PrivateKey, listener, target)
			case TxModeBoth:
				bombardWithBothTransactions(ctx, sharedClient, k.PrivateKey, listener, cfg.Data, target)
			default:
				bombardWithTransactions(ctx, sharedClient, k.PrivateKey, listener, cfg.Data, target)
			}
		}(i, key, cfg.Mode)
	}

	// Wait for interrupt
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\nShutting down...")
	cancel()
	wg.Wait()

	return nil
}

// RunAsync starts the bombardment in a goroutine and returns a stop function.
func RunAsync(cfg Config) (stop func(), errChan <-chan error) {
	ch := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		ch <- runWithContext(ctx, cfg)
		close(ch)
	}()

	return cancel, ch
}

func runWithContext(ctx context.Context, cfg Config) error {
	if len(cfg.RPCURLs) == 0 {
		return fmt.Errorf("no RPC URLs provided")
	}

	// Set package-level variables
	batchSize = cfg.BatchSize
	keyCount = cfg.KeyCount
	timeoutSeconds = cfg.TimeoutSeconds

	// Default to native mode if not specified
	if cfg.Mode == "" {
		cfg.Mode = TxModeNative
	}

	fmt.Printf("Starting with batch size: %d, key count: %d, mode: %s\n", batchSize, keyCount, cfg.Mode)
	fmt.Printf("Using %d RPC URLs\n", len(cfg.RPCURLs))

	rpcURL := cfg.RPCURLs[0]

	client, err := dialWithSharedTransport(rpcURL)
	if err != nil {
		return fmt.Errorf("failed to connect to RPC: %w", err)
	}
	defer client.Close()

	wsURL := strings.Replace(rpcURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "/rpc", "/ws", 1)

	listener, err := NewTxListener(wsURL)
	if err != nil {
		return fmt.Errorf("failed to create listener: %w", err)
	}
	defer listener.Close()

	keys := generateKeys(keyCount)

	// Fund accounts with native tokens (needed for gas in all modes)
	fmt.Printf("funding %d accounts with native tokens\n", keyCount)
	if err := fundAccounts(client, listener, keys); err != nil {
		return fmt.Errorf("failed to fund accounts: %w", err)
	}
	fmt.Println("all accounts funded with native tokens")

	// Fund accounts with ERC20 tokens if needed
	if cfg.Mode == TxModeERC20 || cfg.Mode == TxModeBoth {
		fmt.Printf("funding %d accounts with ERC20 tokens\n", keyCount)
		if err := fundAccountsWithERC20(client, listener, keys); err != nil {
			return fmt.Errorf("failed to fund accounts with ERC20: %w", err)
		}
		fmt.Println("all accounts funded with ERC20 tokens")
	}

	// Create shared client pool (one per RPC URL, shared across workers)
	// ethclient is goroutine-safe for concurrent requests
	clientPool := make([]*ethclient.Client, len(cfg.RPCURLs))
	for i, url := range cfg.RPCURLs {
		c, err := dialWithSharedTransport(url)
		if err != nil {
			// Close already opened clients
			for j := 0; j < i; j++ {
				clientPool[j].Close()
			}
			return fmt.Errorf("failed to create client pool: %w", err)
		}
		clientPool[i] = c
	}
	defer func() {
		for _, c := range clientPool {
			c.Close()
		}
	}()

	var toAddress common.Address
	if cfg.ToAddress != "" {
		toAddress = common.HexToAddress(cfg.ToAddress)
	}

	var wg sync.WaitGroup

	for i, key := range keys {
		wg.Add(1)
		go func(idx int, k *Key, mode TxMode) {
			defer wg.Done()

			// Use shared client from pool (round-robin distribution)
			sharedClient := clientPool[idx%len(clientPool)]

			target := toAddress
			if target == (common.Address{}) {
				target = common.HexToAddress(fmt.Sprintf("0x%040x", idx+1))
			}

			switch mode {
			case TxModeERC20:
				bombardWithERC20Transactions(ctx, sharedClient, k.PrivateKey, listener, target)
			case TxModeBoth:
				bombardWithBothTransactions(ctx, sharedClient, k.PrivateKey, listener, cfg.Data, target)
			default:
				bombardWithTransactions(ctx, sharedClient, k.PrivateKey, listener, cfg.Data, target)
			}
		}(i, key, cfg.Mode)
	}

	<-ctx.Done()
	wg.Wait()
	return nil
}
