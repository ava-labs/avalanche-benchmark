package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ava-labs/avalanche-benchmark/local/internal/network"
	"github.com/spf13/cobra"
)

var (
	genesisPath     string
	chainConfigPath string
	dataDir         string
	exitOnSuccess   bool
)

type fileConfig struct {
	PrimaryNodes int `json:"primaryNodes"`
	L1Validators int `json:"l1Validators"`
	L1RPCs       int `json:"l1Rpcs"`
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "startnetwork",
		Short: "Start local Avalanche L1 network",
		Long:  "Start a local Avalanche L1 network for benchmarking.",
		RunE:  runStart,
	}

	rootCmd.Flags().StringVar(&genesisPath, "genesis", "./genesis.json", "Genesis file")
	rootCmd.Flags().StringVar(&chainConfigPath, "chain-config", "./chain-config.json", "Chain config file")
	rootCmd.Flags().StringVar(&dataDir, "data-dir", "./network_data", "Data directory")
	rootCmd.Flags().BoolVar(&exitOnSuccess, "exit-on-success", false, "Exit after the network is ready without shutting down nodes")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runStart(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fileCfg, configPath, err := loadConfig()
	if err != nil {
		return err
	}

	cfg := network.Config{
		DataDir:              "./network_data",
		GenesisPath:          "./genesis.json",
		ChainConfigPath:      "./chain-config.json",
		PrimaryNodeCount:     0,
		L1ValidatorNodeCount: 0,
		L1RPCNodeCount:       0,
	}

	if fileCfg.PrimaryNodes > 0 {
		cfg.PrimaryNodeCount = fileCfg.PrimaryNodes
	}
	if fileCfg.L1Validators > 0 {
		cfg.L1ValidatorNodeCount = fileCfg.L1Validators
	}
	if fileCfg.L1RPCs >= 0 {
		cfg.L1RPCNodeCount = fileCfg.L1RPCs
	}

	if cmd.Flags().Changed("data-dir") {
		cfg.DataDir = dataDir
	}
	if cmd.Flags().Changed("genesis") {
		cfg.GenesisPath = genesisPath
	}
	if cmd.Flags().Changed("chain-config") {
		cfg.ChainConfigPath = chainConfigPath
	}
	if cfg.PrimaryNodeCount <= 0 {
		return fmt.Errorf("benchmark-config.json: primaryNodes must be > 0")
	}
	if cfg.L1ValidatorNodeCount <= 0 {
		return fmt.Errorf("benchmark-config.json: l1Validators must be > 0")
	}

	fmt.Printf("Config file: %s\n", configPath)
	fmt.Printf("Data dir: %s\n", cfg.DataDir)
	fmt.Printf("Genesis: %s\n", cfg.GenesisPath)
	fmt.Printf("Chain config: %s\n", cfg.ChainConfigPath)
	fmt.Printf("Primary nodes: %d\n", cfg.PrimaryNodeCount)
	fmt.Printf("L1 validators: %d\n", cfg.L1ValidatorNodeCount)
	fmt.Printf("L1 RPCs: %d\n", cfg.L1RPCNodeCount)

	return network.StartAndMonitor(ctx, cfg, exitOnSuccess)
}

func loadConfig() (fileConfig, string, error) {
	paths := []string{
		"./benchmark-config.json",
		"./behcmark-config.json",
	}

	var path string
	for _, candidate := range paths {
		if _, err := os.Stat(candidate); err == nil {
			path = candidate
			break
		}
	}
	if path == "" {
		return fileConfig{}, "", fmt.Errorf("config file not found: %s", strings.Join(paths, ", "))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fileConfig{}, "", fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg fileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fileConfig{}, "", fmt.Errorf("failed to parse config file: %w", err)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return cfg, path, nil
	}
	return cfg, absPath, nil
}
