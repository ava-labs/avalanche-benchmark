package network

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/api/info"
	"github.com/ava-labs/avalanchego/genesis"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
)

const (
	// Port allocation
	baseHTTPPort  = 9650
	portIncrement = 100

	// Timeouts
	nodeHealthTimeout   = 2 * time.Second
	nodeStartupTimeout  = 60 * time.Second
	healthCheckInterval = 200 * time.Millisecond
)

// Config holds the configuration for starting a network
type Config struct {
	DataDir              string            // Directory for network data
	GenesisPath          string            // Path to subnet-evm genesis file (optional)
	ChainConfigPath      string            // Path to subnet-evm chain config file (optional)
	PrimaryNodeCount     int               // Number of primary network nodes (default: 2)
	L1ValidatorNodeCount int               // Number of L1 validator nodes (default: 2)
	L1RPCNodeCount       int               // Number of L1 RPC-only nodes (default: 1, not validators)
	NodeFlags            map[string]string // Additional flags to pass to nodes
}

// Result holds the result of starting a network
type Result struct {
	DataDir       string
	NodeURI       string
	NodeURIs      []string // All L1 node URIs
	ValidatorURIs []string // L1 validator node URIs
	RPCNodeURIs   []string // L1 RPC-only node URIs
	ChainID       string
	SubnetID      string
	RPCURL        string   // Single RPC URL (first RPC node)
	RPCURLs       []string // All RPC URLs for load balancing
	PIDs          []int    // Process IDs for cleanup
}

// NodeInfo holds information about a running node
type NodeInfo struct {
	NodeID         string
	URI            string
	StakingAddress string
	PID            int
	BLSPublicKey   string
	BLSProofOfPoss string
}

// Start starts a new benchmark network with an L1
func Start(ctx context.Context, cfg Config) (*Result, error) {
	// Determine data directory
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = "./network_data"
	}
	dataDirAbs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve data directory: %w", err)
	}

	// Always use the same directory and clean it on each run.
	if err := os.RemoveAll(dataDirAbs); err != nil {
		return nil, fmt.Errorf("failed to clean data directory: %w", err)
	}
	if err := os.MkdirAll(dataDirAbs, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}
	networkDir := dataDirAbs
	if err := ensureStakingKeys(networkDir); err != nil {
		return nil, err
	}

	// Find avalanchego binary
	avalanchegoPath, err := findAvalanchego()
	if err != nil {
		return nil, fmt.Errorf("failed to find avalanchego: %w", err)
	}

	// Find subnet-evm plugin
	pluginDir, err := findPluginDir()
	if err != nil {
		return nil, fmt.Errorf("failed to find plugin directory: %w", err)
	}

	fmt.Printf("Using avalanchego: %s\n", avalanchegoPath)
	fmt.Printf("Using plugin dir: %s\n", pluginDir)
	fmt.Printf("Network directory: %s\n", networkDir)
	fmt.Printf("Logs directory: %s (per-node logs in node-*/logs)\n", networkDir)

	// Determine node counts
	primaryNodeCount := cfg.PrimaryNodeCount
	if primaryNodeCount < 2 {
		primaryNodeCount = 2 // Minimum 2 for connectivity
	}
	l1ValidatorCount := cfg.L1ValidatorNodeCount
	if l1ValidatorCount < 1 {
		l1ValidatorCount = 1
	}
	l1RPCCount := cfg.L1RPCNodeCount
	if l1RPCCount < 0 {
		l1RPCCount = 0
	}

	fmt.Printf("Primary network nodes: %d\n", primaryNodeCount)
	fmt.Printf("L1 validator nodes: %d\n", l1ValidatorCount)
	fmt.Printf("L1 RPC nodes: %d\n", l1RPCCount)

	// Track all PIDs for cleanup
	var allPIDs []int

	// Start primary network nodes
	fmt.Println("Starting primary network nodes...")
	primaryNodes := make([]*NodeInfo, 0, primaryNodeCount)

	// Start bootstrap node first (node 0)
	fmt.Println("  Starting bootstrap node...")
	bootstrapNode, err := startNode(ctx, avalanchegoPath, networkDir, 0, pluginDir, "", cfg.NodeFlags)
	if err != nil {
		return nil, fmt.Errorf("failed to start bootstrap node: %w", err)
	}
	primaryNodes = append(primaryNodes, bootstrapNode)
	allPIDs = append(allPIDs, bootstrapNode.PID)
	fmt.Printf("  Bootstrap node started: %s (port %d)\n", bootstrapNode.NodeID, baseHTTPPort)

	// Start remaining primary nodes
	for i := 1; i < primaryNodeCount; i++ {
		fmt.Printf("  Starting primary node %d...\n", i+1)
		node, err := startNode(ctx, avalanchegoPath, networkDir, i, pluginDir, bootstrapNode.NodeID, cfg.NodeFlags)
		if err != nil {
			// Kill already started nodes
			for _, n := range primaryNodes {
				killProcess(n.PID)
			}
			return nil, fmt.Errorf("failed to start primary node %d: %w", i, err)
		}
		primaryNodes = append(primaryNodes, node)
		allPIDs = append(allPIDs, node.PID)
		fmt.Printf("  Primary node %d started: %s (port %d)\n", i+1, node.NodeID, baseHTTPPort+i*portIncrement)
	}

	// Use bootstrap node for wallet operations
	primaryNodeURI := primaryNodes[0].URI
	fmt.Printf("Primary network ready at: %s\n", primaryNodeURI)

	// Load subnet-evm genesis
	genesisBytes, err := loadGenesis(cfg.GenesisPath)
	if err != nil {
		for _, pid := range allPIDs {
			killProcess(pid)
		}
		return nil, fmt.Errorf("failed to load genesis: %w", err)
	}

	fmt.Println("Creating subnet and chain on P-Chain...")

	// Create keychain with EWOQ key
	kc := secp256k1fx.NewKeychain(genesis.EWOQKey)

	// Create P-Chain wallet
	wallet, err := primary.MakePWallet(ctx, primaryNodeURI, kc, primary.WalletConfig{})
	if err != nil {
		for _, pid := range allPIDs {
			killProcess(pid)
		}
		return nil, fmt.Errorf("failed to create wallet: %w", err)
	}

	// Create subnet
	fmt.Println("  Creating subnet...")
	owner := &secp256k1fx.OutputOwners{
		Threshold: 1,
		Addrs:     []ids.ShortID{genesis.EWOQKey.Address()},
	}

	subnetTx, err := wallet.IssueCreateSubnetTx(owner)
	if err != nil {
		for _, pid := range allPIDs {
			killProcess(pid)
		}
		return nil, fmt.Errorf("failed to create subnet: %w", err)
	}
	subnetID := subnetTx.ID()
	fmt.Printf("  Subnet created: %s\n", subnetID)

	// Re-sync wallet with subnet
	wallet, err = primary.MakePWallet(ctx, primaryNodeURI, kc, primary.WalletConfig{
		SubnetIDs: []ids.ID{subnetID},
	})
	if err != nil {
		for _, pid := range allPIDs {
			killProcess(pid)
		}
		return nil, fmt.Errorf("failed to re-sync wallet: %w", err)
	}

	// Create chain
	fmt.Println("  Creating chain...")
	chainTx, err := wallet.IssueCreateChainTx(
		subnetID,
		genesisBytes,
		constants.SubnetEVMID,
		nil,
		"benchmarkchain",
	)
	if err != nil {
		for _, pid := range allPIDs {
			killProcess(pid)
		}
		return nil, fmt.Errorf("failed to create chain: %w", err)
	}
	chainID := chainTx.ID()
	fmt.Printf("  Chain created: %s\n", chainID)

	// Load and write chain config to all primary nodes
	chainConfigBytes, err := loadChainConfig(cfg.ChainConfigPath)
	if err != nil {
		for _, pid := range allPIDs {
			killProcess(pid)
		}
		return nil, fmt.Errorf("failed to load chain config: %w", err)
	}

	fmt.Println("Writing chain config to nodes...")
	for i := 0; i < primaryNodeCount; i++ {
		nodeDir := filepath.Join(networkDir, fmt.Sprintf("node-%d", i))
		if err := writeChainConfig(nodeDir, chainID.String(), chainConfigBytes); err != nil {
			fmt.Printf("  Warning: failed to write chain config to node-%d: %v\n", i, err)
		}
	}

	// Now add L1 validator nodes that track the subnet
	fmt.Printf("Adding %d L1 validator node(s)...\n", l1ValidatorCount)
	l1ValidatorNodes := make([]*NodeInfo, 0, l1ValidatorCount)

	for i := 0; i < l1ValidatorCount; i++ {
		nodeIndex := primaryNodeCount + i // Continue port numbering after primary nodes
		fmt.Printf("  Starting L1 validator node %d...\n", i+1)

		// Pre-create L1 node directory and write chain config
		l1NodeDir := filepath.Join(networkDir, fmt.Sprintf("l1-validator-%d", nodeIndex))
		if err := os.MkdirAll(l1NodeDir, 0755); err != nil {
			for _, pid := range allPIDs {
				killProcess(pid)
			}
			return nil, fmt.Errorf("failed to create L1 validator node dir: %w", err)
		}
		if err := writeChainConfig(l1NodeDir, chainID.String(), chainConfigBytes); err != nil {
			fmt.Printf("  Warning: failed to write chain config to l1-validator-%d: %v\n", nodeIndex, err)
		}

		l1Node, err := startL1Node(ctx, avalanchegoPath, networkDir, nodeIndex, pluginDir,
			bootstrapNode.NodeID, subnetID.String(), "validator", cfg.NodeFlags)
		if err != nil {
			for _, pid := range allPIDs {
				killProcess(pid)
			}
			return nil, fmt.Errorf("failed to start L1 validator node %d: %w", i, err)
		}
		l1ValidatorNodes = append(l1ValidatorNodes, l1Node)
		allPIDs = append(allPIDs, l1Node.PID)
		fmt.Printf("  L1 Validator %d started: %s (port %d)\n", i+1, l1Node.NodeID, baseHTTPPort+nodeIndex*portIncrement)
	}

	// Gather validator info from L1 validator nodes for ConvertSubnetToL1
	fmt.Println("Gathering L1 validator info...")
	validators := make([]*txs.ConvertSubnetToL1Validator, 0, len(l1ValidatorNodes))

	for i, node := range l1ValidatorNodes {
		infoClient := info.NewClient(node.URI)
		nodeID, nodePoP, err := infoClient.GetNodeID(ctx)
		if err != nil {
			for _, pid := range allPIDs {
				killProcess(pid)
			}
			return nil, fmt.Errorf("failed to get node ID for L1 validator %d: %w", i, err)
		}
		fmt.Printf("  Validator %d: %s\n", i+1, nodeID)

		validators = append(validators, &txs.ConvertSubnetToL1Validator{
			NodeID:  nodeID.Bytes(),
			Weight:  units.Schmeckle,
			Balance: units.Avax,
			Signer:  *nodePoP,
		})
	}

	// Convert subnet to L1
	fmt.Println("Converting subnet to L1...")
	_, err = wallet.IssueConvertSubnetToL1Tx(
		subnetID,
		chainID,
		[]byte{}, // Empty manager address for simple benchmark
		validators,
	)
	if err != nil {
		for _, pid := range allPIDs {
			killProcess(pid)
		}
		return nil, fmt.Errorf("failed to convert subnet to L1: %w", err)
	}
	fmt.Printf("L1 conversion complete with %d validator(s)!\n", len(validators))

	// Wait for chain to be ready before starting RPC nodes
	fmt.Println("Waiting for L1 chain to be ready...")
	time.Sleep(5 * time.Second)

	// Now add L1 RPC-only nodes (they track the subnet but don't validate)
	l1RPCNodes := make([]*NodeInfo, 0, l1RPCCount)
	if l1RPCCount > 0 {
		fmt.Printf("Adding %d L1 RPC-only node(s)...\n", l1RPCCount)

		for i := 0; i < l1RPCCount; i++ {
			nodeIndex := primaryNodeCount + l1ValidatorCount + i // Continue port numbering
			fmt.Printf("  Starting L1 RPC node %d...\n", i+1)

			// Pre-create RPC node directory and write chain config
			rpcNodeDir := filepath.Join(networkDir, fmt.Sprintf("l1-rpc-%d", nodeIndex))
			if err := os.MkdirAll(rpcNodeDir, 0755); err != nil {
				for _, pid := range allPIDs {
					killProcess(pid)
				}
				return nil, fmt.Errorf("failed to create L1 RPC node dir: %w", err)
			}
			if err := writeChainConfig(rpcNodeDir, chainID.String(), chainConfigBytes); err != nil {
				fmt.Printf("  Warning: failed to write chain config to l1-rpc-%d: %v\n", nodeIndex, err)
			}

			rpcNode, err := startL1Node(ctx, avalanchegoPath, networkDir, nodeIndex, pluginDir,
				bootstrapNode.NodeID, subnetID.String(), "rpc", cfg.NodeFlags)
			if err != nil {
				for _, pid := range allPIDs {
					killProcess(pid)
				}
				return nil, fmt.Errorf("failed to start L1 RPC node %d: %w", i, err)
			}
			l1RPCNodes = append(l1RPCNodes, rpcNode)
			allPIDs = append(allPIDs, rpcNode.PID)
			fmt.Printf("  L1 RPC Node %d started: %s (port %d)\n", i+1, rpcNode.NodeID, baseHTTPPort+nodeIndex*portIncrement)
		}
	}

	// Collect all node URIs
	allL1NodeURIs := make([]string, 0, len(l1ValidatorNodes)+len(l1RPCNodes))
	validatorURIs := make([]string, 0, len(l1ValidatorNodes))
	rpcNodeURIs := make([]string, 0, len(l1RPCNodes))

	for _, node := range l1ValidatorNodes {
		allL1NodeURIs = append(allL1NodeURIs, node.URI)
		validatorURIs = append(validatorURIs, node.URI)
	}
	for _, node := range l1RPCNodes {
		allL1NodeURIs = append(allL1NodeURIs, node.URI)
		rpcNodeURIs = append(rpcNodeURIs, node.URI)
	}

	// Determine RPC URLs for load balancing
	// If we have dedicated RPC nodes, use them; otherwise fall back to validators
	var rpcURLs []string
	var primaryRPCNodeURI string
	if len(l1RPCNodes) > 0 {
		primaryRPCNodeURI = l1RPCNodes[0].URI
		for _, node := range l1RPCNodes {
			rpcURLs = append(rpcURLs, fmt.Sprintf("%s/ext/bc/%s/rpc", node.URI, chainID))
		}
	} else {
		// Fall back to validator nodes for RPC
		primaryRPCNodeURI = l1ValidatorNodes[0].URI
		for _, node := range l1ValidatorNodes {
			rpcURLs = append(rpcURLs, fmt.Sprintf("%s/ext/bc/%s/rpc", node.URI, chainID))
		}
	}
	rpcURL := fmt.Sprintf("%s/ext/bc/%s/rpc", primaryRPCNodeURI, chainID)

	// Write all L1 RPC URLs to rpcs.txt for bombard
	allL1RPCURLs := make([]string, 0, len(allL1NodeURIs))
	for _, uri := range allL1NodeURIs {
		allL1RPCURLs = append(allL1RPCURLs, fmt.Sprintf("%s/ext/bc/%s/rpc", uri, chainID))
	}
	rpcsFile := filepath.Join(networkDir, "rpcs.txt")
	if err := os.WriteFile(rpcsFile, []byte(strings.Join(allL1RPCURLs, ",")), 0644); err != nil {
		fmt.Printf("Warning: failed to write rpcs.txt: %v\n", err)
	}

	return &Result{
		DataDir:       networkDir,
		NodeURI:       primaryRPCNodeURI,
		NodeURIs:      allL1NodeURIs,
		ValidatorURIs: validatorURIs,
		RPCNodeURIs:   rpcNodeURIs,
		ChainID:       chainID.String(),
		SubnetID:      subnetID.String(),
		RPCURL:        rpcURL,
		RPCURLs:       rpcURLs,
		PIDs:          allPIDs,
	}, nil
}

// StartAndMonitor starts the network, prints metrics, and shuts down on Ctrl+C.
func StartAndMonitor(ctx context.Context, cfg Config) error {
	// Kill any existing avalanchego processes (best-effort).
	_ = exec.Command("pkill", "-f", "avalanchego").Run()
	time.Sleep(1 * time.Second)

	result, err := Start(ctx, cfg)
	if err != nil {
		return err
	}

	if len(result.RPCURLs) == 0 {
		return fmt.Errorf("no RPC URLs available for monitoring")
	}

	fmt.Printf("RPC endpoint: %s\n", result.RPCURLs[0])
	fmt.Println("Metrics (Ctrl+C to stop):")

	metricsCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go printMetrics(metricsCtx, result.RPCURLs[0])

	<-ctx.Done()
	fmt.Println("\nShutting down...")

	for _, pid := range result.PIDs {
		killProcess(pid)
	}

	return nil
}
