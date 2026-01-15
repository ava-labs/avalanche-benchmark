package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ava-labs/avalanche-benchmark/internal/network"
	"github.com/spf13/cobra"
)

var (
	genesisPath     string
	chainConfigPath string
	dataDir         string
	primaryNodes    int
	l1Validators    int
	l1RPCs          int
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "benchmark",
		Short: "Avalanche L1 Benchmark",
		Long:  "Start a local Avalanche network and benchmark it.",
		RunE:  runStart,
	}

	rootCmd.Flags().StringVar(&genesisPath, "genesis", "./genesis.json", "Genesis file")
	rootCmd.Flags().StringVar(&chainConfigPath, "chain-config", "./chain-config.json", "Chain config file")
	rootCmd.Flags().StringVar(&dataDir, "data-dir", "./network_data", "Data directory")
	rootCmd.Flags().IntVar(&primaryNodes, "primary-nodes", 2, "Primary network nodes")
	rootCmd.Flags().IntVar(&l1Validators, "l1-validators", 2, "L1 validator nodes")
	rootCmd.Flags().IntVar(&l1RPCs, "l1-rpcs", 1, "L1 RPC nodes")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runStart(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg := network.Config{
		DataDir:              dataDir,
		GenesisPath:          genesisPath,
		ChainConfigPath:      chainConfigPath,
		PrimaryNodeCount:     primaryNodes,
		L1ValidatorNodeCount: l1Validators,
		L1RPCNodeCount:       l1RPCs,
	}

	return network.StartAndMonitor(ctx, cfg)
}
