package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/ava-labs/avalanche-benchmark/internal/config"
	"github.com/ava-labs/avalanche-benchmark/internal/flood"
	"github.com/ava-labs/avalanche-benchmark/internal/monitor"
	"github.com/ava-labs/avalanche-benchmark/internal/network"
	"github.com/spf13/cobra"
)

var (
	// Flags
	configPath           string
	genesisPath          string
	chainConfigPath      string
	keysCount            int
	batchSize            int
	dataDir              string
	primaryNodeCount     int
	l1ValidatorNodeCount int
	l1RPCNodeCount       int

	// Logs flags
	logsNodeType string
	logsNodeNum  int
	logsFollow   bool
	logsLines    int
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "benchmark",
		Short: "Avalanche L1 Benchmark CLI",
		Long:  `A minimal CLI tool for benchmarking Avalanche L1 (subnet-evm) performance.`,
	}

	// Global flags
	rootCmd.PersistentFlags().StringVar(&dataDir, "data-dir", "", "Directory for network data (default: ~/.avalanche-benchmark)")

	// Start command
	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start network and create L1",
		Long:  `Starts a local Avalanche network and creates an L1 with subnet-evm.`,
		RunE:  runStart,
	}
	startCmd.Flags().StringVar(&configPath, "config", "", "Path to config.json file")
	startCmd.Flags().StringVar(&genesisPath, "genesis", "", "Path to subnet-evm genesis file (uses optimized default)")
	startCmd.Flags().StringVar(&chainConfigPath, "chain-config", "", "Path to subnet-evm chain config file (uses optimized default)")
	startCmd.Flags().IntVar(&primaryNodeCount, "primary-nodes", 2, "Number of primary network nodes (min: 2)")
	startCmd.Flags().IntVar(&l1ValidatorNodeCount, "l1-validators", 2, "Number of L1 validator nodes (participate in consensus)")
	startCmd.Flags().IntVar(&l1RPCNodeCount, "l1-rpcs", 1, "Number of L1 RPC-only nodes (for load balancing, not validators)")
	rootCmd.AddCommand(startCmd)

	// Flood command
	floodCmd := &cobra.Command{
		Use:   "flood",
		Short: "Start flooding transactions",
		Long:  `Starts evmbombard to flood transactions to the L1.`,
		RunE:  runFlood,
	}
	floodCmd.Flags().IntVar(&keysCount, "keys", 600, "Number of keys to use for flooding")
	floodCmd.Flags().IntVar(&batchSize, "batch", 50, "Batch size for transactions")
	rootCmd.AddCommand(floodCmd)

	// Stop-flood command
	stopFloodCmd := &cobra.Command{
		Use:   "stop-flood",
		Short: "Stop flooding transactions",
		Long:  `Stops the evmbombard process.`,
		RunE:  runStopFlood,
	}
	rootCmd.AddCommand(stopFloodCmd)

	// Monitor command
	monitorCmd := &cobra.Command{
		Use:   "monitor",
		Short: "Show live metrics",
		Long:  `Displays live performance metrics from the network.`,
		RunE:  runMonitor,
	}
	rootCmd.AddCommand(monitorCmd)

	// Shutdown command
	shutdownCmd := &cobra.Command{
		Use:   "shutdown",
		Short: "Stop everything",
		Long:  `Stops flooding and shuts down the network.`,
		RunE:  runShutdown,
	}
	rootCmd.AddCommand(shutdownCmd)

	// Status command
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show node status and health",
		Long:  `Displays all node endpoints and their health status.`,
		RunE:  runStatus,
	}
	rootCmd.AddCommand(statusCmd)

	// Logs command
	logsCmd := &cobra.Command{
		Use:   "logs [node-type] [node-num]",
		Short: "View node logs",
		Long: `View logs for a specific node.

Node types: primary, validator, rpc
Example: benchmark logs validator 1`,
		RunE: runLogs,
	}
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Follow log output")
	logsCmd.Flags().IntVarP(&logsLines, "lines", "n", 50, "Number of lines to show")
	rootCmd.AddCommand(logsCmd)

	// Flood-status command
	floodStatusCmd := &cobra.Command{
		Use:   "flood-status",
		Short: "Show flooding status",
		Long:  `Displays the status of the transaction flooding process.`,
		RunE:  runFloodStatus,
	}
	rootCmd.AddCommand(floodStatusCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runStart(cmd *cobra.Command, args []string) error {
	fmt.Println("Starting Avalanche benchmark network...")

	// Load config from file if specified
	var benchConfig *config.Config
	if configPath != "" {
		var err error
		benchConfig, err = config.Load(configPath)
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		fmt.Printf("Loaded config from: %s\n", configPath)
	} else {
		benchConfig = config.DefaultConfig()
	}

	// Command line flags override config file
	if cmd.Flags().Changed("primary-nodes") {
		benchConfig.PrimaryNodeCount = primaryNodeCount
	}
	if cmd.Flags().Changed("l1-validators") {
		benchConfig.L1ValidatorNodeCount = l1ValidatorNodeCount
	}
	if cmd.Flags().Changed("l1-rpcs") {
		benchConfig.L1RPCNodeCount = l1RPCNodeCount
	}

	cfg := network.Config{
		DataDir:              dataDir,
		GenesisPath:          genesisPath,
		ChainConfigPath:      chainConfigPath,
		PrimaryNodeCount:     benchConfig.PrimaryNodeCount,
		L1ValidatorNodeCount: benchConfig.L1ValidatorNodeCount,
		L1RPCNodeCount:       benchConfig.L1RPCNodeCount,
		NodeFlags:            benchConfig.NodeFlags,
	}

	fmt.Printf("Starting %d primary network node(s) + %d L1 validator(s) + %d L1 RPC node(s)...\n",
		cfg.PrimaryNodeCount, cfg.L1ValidatorNodeCount, cfg.L1RPCNodeCount)

	net, err := network.Start(cmd.Context(), cfg)
	if err != nil {
		return fmt.Errorf("failed to start network: %w", err)
	}

	fmt.Printf("\nNetwork started successfully!\n")
	fmt.Printf("  Primary Nodes:    %d\n", cfg.PrimaryNodeCount)
	fmt.Printf("  L1 Validators:    %d\n", cfg.L1ValidatorNodeCount)
	fmt.Printf("  L1 RPC Nodes:     %d\n", cfg.L1RPCNodeCount)
	fmt.Printf("  Chain ID:         %s\n", net.ChainID)
	fmt.Printf("  RPC URLs:         %d endpoint(s)\n", len(net.RPCURLs))
	for i, url := range net.RPCURLs {
		fmt.Printf("    [%d] %s\n", i+1, url)
	}
	fmt.Printf("  Data Dir:         %s\n", net.DataDir)
	fmt.Printf("\nRun 'benchmark flood' to start flooding transactions.\n")

	return nil
}

func runFlood(cmd *cobra.Command, args []string) error {
	fmt.Println("Starting transaction flood...")

	state, err := network.LoadState(dataDir)
	if err != nil {
		return fmt.Errorf("network not running (run 'benchmark start' first): %w", err)
	}

	cfg := flood.Config{
		RPCURL:    state.RPCURL,
		RPCURLs:   state.RPCURLs,
		KeysCount: keysCount,
		BatchSize: batchSize,
		DataDir:   state.DataDir,
	}

	if err := flood.Start(cmd.Context(), cfg); err != nil {
		return fmt.Errorf("failed to start flooding: %w", err)
	}

	fmt.Printf("Flooding started with %d RPC endpoint(s). Run 'benchmark monitor' to see metrics.\n", len(cfg.RPCURLs))
	return nil
}

func runStopFlood(cmd *cobra.Command, args []string) error {
	fmt.Println("Stopping transaction flood...")

	state, err := network.LoadState(dataDir)
	if err != nil {
		return fmt.Errorf("network not running: %w", err)
	}

	if err := flood.Stop(state.DataDir); err != nil {
		return fmt.Errorf("failed to stop flooding: %w", err)
	}

	fmt.Println("Flooding stopped.")
	return nil
}

func runMonitor(cmd *cobra.Command, args []string) error {
	state, err := network.LoadState(dataDir)
	if err != nil {
		return fmt.Errorf("network not running: %w", err)
	}

	cfg := monitor.Config{
		NodeURI:       state.NodeURI,
		ChainID:       state.ChainID,
		ValidatorURIs: state.ValidatorURIs,
		RPCNodeURIs:   state.RPCNodeURIs,
	}

	return monitor.Run(cmd.Context(), cfg)
}

func runShutdown(cmd *cobra.Command, args []string) error {
	fmt.Println("Shutting down...")

	state, err := network.LoadState(dataDir)
	if err != nil {
		// Network might not be running, that's ok
		fmt.Println("No network state found, nothing to shut down.")
		return nil
	}

	// Stop flooding first
	_ = flood.Stop(state.DataDir)

	// Stop network - pass the state directory where state file lives, not network dir
	if err := network.Stop(dataDir); err != nil {
		return fmt.Errorf("failed to stop network: %w", err)
	}

	fmt.Println("Network shut down successfully.")
	return nil
}

func runStatus(cmd *cobra.Command, args []string) error {
	state, err := network.LoadState(dataDir)
	if err != nil {
		return fmt.Errorf("network not running: %w", err)
	}

	fmt.Println("Network Status")
	fmt.Println("══════════════════════════════════════════════════════════")
	fmt.Printf("Chain ID:   %s\n", state.ChainID)
	fmt.Printf("Subnet ID:  %s\n", state.SubnetID)
	fmt.Printf("Data Dir:   %s\n", state.DataDir)
	fmt.Println()

	// Check health for all nodes
	fmt.Println("L1 Validator Nodes:")
	for i, uri := range state.ValidatorURIs {
		health := network.CheckNodeHealth(uri, state.ChainID)
		rpcURL := fmt.Sprintf("%s/ext/bc/%s/rpc", uri, state.ChainID)
		statusIcon := "●"
		statusColor := "\033[32m" // green
		if !health.Reachable {
			statusIcon = "✗"
			statusColor = "\033[31m" // red
		} else if !health.L1Healthy {
			statusIcon = "○"
			statusColor = "\033[33m" // yellow
		}
		fmt.Printf("  %s%s\033[0m V%d  %s\n", statusColor, statusIcon, i+1, uri)
		fmt.Printf("      RPC: %s\n", rpcURL)
		if health.Reachable {
			fmt.Printf("      L1 Height: %d | Peers: %d | Disk: %d%%\n",
				health.L1Height, health.ConnectedPeers, health.DiskPercent)
		}
	}
	fmt.Println()

	fmt.Println("L1 RPC Nodes:")
	for i, uri := range state.RPCNodeURIs {
		health := network.CheckNodeHealth(uri, state.ChainID)
		rpcURL := fmt.Sprintf("%s/ext/bc/%s/rpc", uri, state.ChainID)
		statusIcon := "●"
		statusColor := "\033[32m"
		if !health.Reachable {
			statusIcon = "✗"
			statusColor = "\033[31m"
		} else if !health.L1Healthy {
			statusIcon = "○"
			statusColor = "\033[33m"
		}
		fmt.Printf("  %s%s\033[0m R%d  %s\n", statusColor, statusIcon, i+1, uri)
		fmt.Printf("      RPC: %s\n", rpcURL)
		if health.Reachable {
			fmt.Printf("      L1 Height: %d | Peers: %d | Disk: %d%%\n",
				health.L1Height, health.ConnectedPeers, health.DiskPercent)
		}
	}
	fmt.Println()

	fmt.Println("RPC Endpoints (for bombard):")
	for i, url := range state.RPCURLs {
		fmt.Printf("  [%d] %s\n", i+1, url)
	}

	return nil
}

func runLogs(cmd *cobra.Command, args []string) error {
	state, err := network.LoadState(dataDir)
	if err != nil {
		return fmt.Errorf("network not running: %w", err)
	}

	if len(args) < 2 {
		// Show available nodes
		fmt.Println("Available nodes:")
		fmt.Println("  primary 0, 1, ...   - Primary network nodes")
		fmt.Println("  validator 1, 2, ... - L1 validator nodes")
		fmt.Println("  rpc 1, 2, ...       - L1 RPC nodes")
		fmt.Println("  bombard             - Bombard flooding logs")
		fmt.Println()
		fmt.Println("Example: benchmark logs validator 1")
		fmt.Println("         benchmark logs rpc 1 -f")
		fmt.Println("         benchmark logs bombard")
		return nil
	}

	nodeType := args[0]
	var logPath string

	if nodeType == "bombard" {
		logPath = fmt.Sprintf("%s/evmbombard.log", state.DataDir)
	} else {
		nodeNum := 0
		if len(args) > 1 {
			fmt.Sscanf(args[1], "%d", &nodeNum)
		}

		switch nodeType {
		case "primary":
			logPath = fmt.Sprintf("%s/node-%d/logs/main.log", state.DataDir, nodeNum)
		case "validator":
			// Validators start at index primaryNodeCount (typically 2)
			// User says "validator 1" = first validator = l1-validator-2 (index 2)
			nodeIndex := 2 + nodeNum - 1 // Assuming 2 primary nodes
			logPath = fmt.Sprintf("%s/l1-validator-%d/logs/main.log", state.DataDir, nodeIndex)
		case "rpc":
			// RPC nodes start after validators
			// For a 5 validator setup, RPC nodes start at index 7
			nodeIndex := 2 + len(state.ValidatorURIs) + nodeNum - 1
			logPath = fmt.Sprintf("%s/l1-rpc-%d/logs/main.log", state.DataDir, nodeIndex)
		default:
			return fmt.Errorf("unknown node type: %s (use primary, validator, rpc, or bombard)", nodeType)
		}
	}

	// Check if file exists
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		return fmt.Errorf("log file not found: %s", logPath)
	}

	fmt.Printf("Log file: %s\n", logPath)
	fmt.Println("──────────────────────────────────────────────────────────")

	if logsFollow {
		// Use tail -f
		tailCmd := fmt.Sprintf("tail -f %s", logPath)
		c := exec.Command("sh", "-c", tailCmd)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}

	// Just show last N lines
	tailCmd := fmt.Sprintf("tail -n %d %s", logsLines, logPath)
	c := exec.Command("sh", "-c", tailCmd)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func runFloodStatus(cmd *cobra.Command, args []string) error {
	state, err := network.LoadState(dataDir)
	if err != nil {
		return fmt.Errorf("network not running: %w", err)
	}

	isRunning, pid := flood.Status(state.DataDir)

	fmt.Println("Flood Status")
	fmt.Println("══════════════════════════════════════════════════════════")

	if isRunning {
		fmt.Printf("\033[32m●\033[0m Flooding ACTIVE (PID: %d)\n", pid)
		fmt.Printf("Log file: %s/evmbombard.log\n", state.DataDir)
		fmt.Println()

		// Show last few lines of log
		logPath := fmt.Sprintf("%s/evmbombard.log", state.DataDir)
		fmt.Println("Recent output:")
		fmt.Println("──────────────────────────────────────────────────────────")
		tailCmd := fmt.Sprintf("tail -n 10 %s 2>/dev/null", logPath)
		c := exec.Command("sh", "-c", tailCmd)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		c.Run()
	} else {
		fmt.Println("\033[31m●\033[0m Flooding NOT RUNNING")
		fmt.Println("Run 'benchmark flood' to start flooding.")
	}

	return nil
}
