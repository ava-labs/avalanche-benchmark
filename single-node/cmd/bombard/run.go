package main

import (
	"context"
	"fmt"
)

// Run is the main entry point for bombardment.
func Run(ctx context.Context, cfg Config) error {
	fmt.Println("Bombard v3 - Pull-Based Worker Model")
	fmt.Printf("Active workers: %d\n", cfg.Workers)
	fmt.Println()

	// Connect to primary RPC
	client, err := dialRPC(cfg.RPCURLs[0])
	if err != nil {
		return fmt.Errorf("connect to RPC %s: %w", cfg.RPCURLs[0], err)
	}
	defer client.Close()

	fmt.Printf("Connected to %s\n", cfg.RPCURLs[0])

	// Get chain info
	chainID, err := client.NetworkID(ctx)
	if err != nil {
		return fmt.Errorf("get chain ID: %w", err)
	}
	fmt.Printf("Chain ID: %s\n", chainID.String())
	fmt.Println()

	// Generate key pool
	fmt.Printf("Generating %d keys...\n", KeyPoolSize)
	keyPool := NewKeyPool(KeyPoolSize)
	fmt.Println("Key pool ready")
	fmt.Println()

	// Create funder
	funder, err := NewFunder(client)
	if err != nil {
		return fmt.Errorf("create funder: %w", err)
	}

	// Fund initial workers
	fmt.Printf("Funding first %d keys...\n", cfg.Workers)
	keys := make([]*Key, cfg.Workers)
	for i := 0; i < cfg.Workers; i++ {
		keys[i] = keyPool.Get(i)
	}
	if err := funder.FundKeys(ctx, keys); err != nil {
		return fmt.Errorf("fund keys: %w", err)
	}
	fmt.Println("Funding complete")
	fmt.Println()

	// Start analytics and listener
	analytics := NewAnalytics()
	listener := NewBlockListener(cfg.RPCURLs[0])
	listener.SetAnalytics(analytics)
	go listener.Run(ctx)

	// Create and start worker pool (using block signal from listener)
	workerPool := NewWorkerPool(client, chainID, keyPool, listener.blockSignal)
	if err := workerPool.Start(cfg.Workers); err != nil {
		return fmt.Errorf("start workers: %w", err)
	}

	// Start UI
	ui := NewUI(workerPool, analytics)
	ui.Run(ctx)

	// Shutdown
	workerPool.Stop()
	return nil
}
