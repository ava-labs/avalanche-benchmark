package network

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	stateFileName = "benchmark-state.json"

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

// State holds the state of a running network
type State struct {
	DataDir       string   `json:"dataDir"`
	NodeURIs      []string `json:"nodeUris"`      // All L1 node URIs (validators + RPC)
	ValidatorURIs []string `json:"validatorUris"` // L1 validator node URIs
	RPCNodeURIs   []string `json:"rpcNodeUris"`   // L1 RPC-only node URIs (used for load balancing)
	NodeURI       string   `json:"nodeUri"`       // Primary RPC node URI (for backwards compat)
	ChainID       string   `json:"chainId"`
	SubnetID      string   `json:"subnetId"`
	RPCURL        string   `json:"rpcUrl"`    // Single RPC URL (for backwards compat)
	RPCURLs       []string `json:"rpcUrls"`   // All RPC URLs for load balancing
	NetworkID     uint32   `json:"networkId"`
	PIDs          []int    `json:"pids"` // Process IDs for cleanup
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
}

// NodeInfo holds information about a running node
type NodeInfo struct {
	NodeID          string
	URI             string
	StakingAddress  string
	PID             int
	BLSPublicKey    string
	BLSProofOfPoss  string
}

// Start starts a new benchmark network with an L1
func Start(ctx context.Context, cfg Config) (*Result, error) {
	// Determine data directory
	dataDir := cfg.DataDir
	if dataDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		dataDir = filepath.Join(homeDir, ".avalanche-benchmark")
	}

	// Check if a network is already running
	if existingState, err := LoadState(dataDir); err == nil {
		// Check if processes are still running
		for _, pid := range existingState.PIDs {
			if isProcessRunning(pid) {
				return nil, fmt.Errorf("network already running (PID %d). Use 'benchmark shutdown' first", pid)
			}
		}
		// PIDs not running, clean up stale state file
		os.Remove(filepath.Join(dataDir, stateFileName))
	}

	// Create unique network directory
	networkDir := filepath.Join(dataDir, fmt.Sprintf("network-%d", time.Now().Unix()))
	if err := os.MkdirAll(networkDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create network directory: %w", err)
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

	// Save state
	state := &State{
		DataDir:       networkDir,
		NodeURI:       primaryRPCNodeURI,
		NodeURIs:      allL1NodeURIs,
		ValidatorURIs: validatorURIs,
		RPCNodeURIs:   rpcNodeURIs,
		ChainID:       chainID.String(),
		SubnetID:      subnetID.String(),
		RPCURL:        rpcURL,
		RPCURLs:       rpcURLs,
		NetworkID:     12345, // Local network ID
		PIDs:          allPIDs,
	}

	if err := saveState(dataDir, state); err != nil {
		return nil, fmt.Errorf("failed to save state: %w", err)
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
	}, nil
}

// startNode starts a primary network validator node
func startNode(ctx context.Context, avalanchego, networkDir string, nodeIndex int, pluginDir, bootstrapNodeID string, extraFlags map[string]string) (*NodeInfo, error) {
	httpPort := baseHTTPPort + nodeIndex*portIncrement
	stakingPort := httpPort + 1
	stakerNum := nodeIndex + 1

	nodeDir := filepath.Join(networkDir, fmt.Sprintf("node-%d", nodeIndex))
	if err := os.MkdirAll(filepath.Join(nodeDir, "db"), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(nodeDir, "logs"), 0755); err != nil {
		return nil, err
	}

	args := buildNodeArgs(httpPort, stakingPort, nodeDir, pluginDir, extraFlags)

	// Add staking keys for validators (up to 5 pre-configured)
	// Look for keys relative to the executable location
	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("failed to get executable path: %w", err)
	}
	execDir := filepath.Dir(execPath)
	stakingKeysDir := filepath.Join(execDir, "staking", "local")

	if stakerNum >= 1 && stakerNum <= 5 {
		args = append(args,
			fmt.Sprintf("--staking-tls-cert-file=%s", filepath.Join(stakingKeysDir, fmt.Sprintf("staker%d.crt", stakerNum))),
			fmt.Sprintf("--staking-tls-key-file=%s", filepath.Join(stakingKeysDir, fmt.Sprintf("staker%d.key", stakerNum))),
			fmt.Sprintf("--staking-signer-key-file=%s", filepath.Join(stakingKeysDir, fmt.Sprintf("signer%d.key", stakerNum))),
		)
	} else {
		args = append(args,
			"--staking-ephemeral-cert-enabled=true",
			"--staking-ephemeral-signer-enabled=true",
		)
	}

	// Bootstrap configuration
	if bootstrapNodeID != "" {
		args = append(args,
			fmt.Sprintf("--bootstrap-ips=127.0.0.1:%d", baseHTTPPort+1), // Bootstrap staking port
			fmt.Sprintf("--bootstrap-ids=%s", bootstrapNodeID),
		)
	} else {
		args = append(args, "--bootstrap-ips=", "--bootstrap-ids=")
	}

	// Start the process
	cmd := exec.Command(avalanchego, args...)
	cmd.Dir = nodeDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // Create new process group so it survives CLI exit
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start avalanchego: %w", err)
	}

	// Wait for node to become healthy
	uri := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID, err := waitForNodeHealth(ctx, uri, nodeStartupTimeout)
	if err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("node failed to become healthy: %w", err)
	}

	return &NodeInfo{
		NodeID:         nodeID,
		URI:            uri,
		StakingAddress: fmt.Sprintf("127.0.0.1:%d", stakingPort),
		PID:            cmd.Process.Pid,
	}, nil
}

// startL1Node starts an L1 node with subnet tracking
// nodeType can be "validator" or "rpc" to control directory naming
func startL1Node(ctx context.Context, avalanchego, networkDir string, nodeIndex int, pluginDir, bootstrapNodeID, subnetID, nodeType string, extraFlags map[string]string) (*NodeInfo, error) {
	httpPort := baseHTTPPort + nodeIndex*portIncrement
	stakingPort := httpPort + 1

	nodeDir := filepath.Join(networkDir, fmt.Sprintf("l1-%s-%d", nodeType, nodeIndex))
	if err := os.MkdirAll(filepath.Join(nodeDir, "db"), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(nodeDir, "logs"), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(nodeDir, "staking"), 0755); err != nil {
		return nil, err
	}

	args := buildNodeArgs(httpPort, stakingPort, nodeDir, pluginDir, extraFlags)

	// L1 nodes use ephemeral keys (avalanchego will generate and store them)
	args = append(args,
		"--staking-ephemeral-cert-enabled=true",
		"--staking-ephemeral-signer-enabled=true",
	)

	// Track the subnet
	args = append(args, fmt.Sprintf("--track-subnets=%s", subnetID))

	// Bootstrap configuration
	args = append(args,
		fmt.Sprintf("--bootstrap-ips=127.0.0.1:%d", baseHTTPPort+1),
		fmt.Sprintf("--bootstrap-ids=%s", bootstrapNodeID),
	)

	// Start the process
	cmd := exec.Command(avalanchego, args...)
	cmd.Dir = nodeDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start avalanchego: %w", err)
	}

	// Wait for node to become healthy
	uri := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID, err := waitForNodeHealth(ctx, uri, nodeStartupTimeout)
	if err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("L1 node failed to become healthy: %w", err)
	}

	return &NodeInfo{
		NodeID:         nodeID,
		URI:            uri,
		StakingAddress: fmt.Sprintf("127.0.0.1:%d", stakingPort),
		PID:            cmd.Process.Pid,
	}, nil
}

// buildNodeArgs builds common avalanchego arguments
func buildNodeArgs(httpPort, stakingPort int, nodeDir, pluginDir string, extraFlags map[string]string) []string {
	args := []string{
		fmt.Sprintf("--http-port=%d", httpPort),
		fmt.Sprintf("--staking-port=%d", stakingPort),
		fmt.Sprintf("--db-dir=%s", filepath.Join(nodeDir, "db")),
		fmt.Sprintf("--log-dir=%s", filepath.Join(nodeDir, "logs")),
		fmt.Sprintf("--chain-data-dir=%s", filepath.Join(nodeDir, "chainData")),
		fmt.Sprintf("--data-dir=%s", nodeDir),
		"--network-id=local",
		"--http-host=127.0.0.1",
		"--sybil-protection-enabled=false",
		fmt.Sprintf("--plugin-dir=%s", pluginDir),
	}

	// Add extra flags
	for k, v := range extraFlags {
		args = append(args, fmt.Sprintf("--%s=%s", k, v))
	}

	return args
}

// waitForNodeHealth waits for a node to become healthy and returns its node ID
func waitForNodeHealth(ctx context.Context, uri string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		nodeID, err := checkNodeHealth(uri)
		if err == nil && nodeID != "" {
			return nodeID, nil
		}

		time.Sleep(healthCheckInterval)
	}

	return "", fmt.Errorf("timeout waiting for node to become healthy")
}

// checkNodeHealth checks if a node is healthy and returns its node ID
func checkNodeHealth(uri string) (string, error) {
	client := &http.Client{Timeout: nodeHealthTimeout}

	// Check info endpoint for node ID
	req, err := http.NewRequest("POST", uri+"/ext/info", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}`))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Result struct {
			NodeID string `json:"nodeID"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if result.Result.NodeID == "" {
		return "", fmt.Errorf("no node ID returned")
	}

	return result.Result.NodeID, nil
}

// killProcess kills a process by PID
func killProcess(pid int) {
	if pid <= 0 {
		return
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}

	// Try SIGTERM first
	proc.Signal(syscall.SIGTERM)

	// Wait briefly then force kill if needed
	time.Sleep(500 * time.Millisecond)
	proc.Kill()
}

// isProcessRunning checks if a process is still running
func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// Send signal 0 to check if process exists
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// Stop stops the network
func Stop(dataDir string) error {
	state, err := LoadState(dataDir)
	if err != nil {
		return err
	}

	fmt.Printf("Stopping %d processes...\n", len(state.PIDs))
	for _, pid := range state.PIDs {
		killProcess(pid)
	}

	fmt.Println("Network stopped.")
	return nil
}

// LoadState loads the network state from disk
func LoadState(dataDir string) (*State, error) {
	if dataDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		dataDir = filepath.Join(homeDir, ".avalanche-benchmark")
	}

	statePath := filepath.Join(dataDir, stateFileName)
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to parse state file: %w", err)
	}

	return &state, nil
}

func saveState(dataDir string, state *State) error {
	statePath := filepath.Join(dataDir, stateFileName)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath, data, 0644)
}

func findAvalanchego() (string, error) {
	homeDir, _ := os.UserHomeDir()

	// Check common locations
	locations := []string{
		// Local build
		"./bin/avalanchego",
		"../avalanchego/build/avalanchego",
		// User's code directory
		filepath.Join(homeDir, "code", "avalanchego", "build", "avalanchego"),
		// User's go bin
		filepath.Join(os.Getenv("GOPATH"), "bin", "avalanchego"),
		filepath.Join(homeDir, "go", "bin", "avalanchego"),
		// System path
		"/usr/local/bin/avalanchego",
	}

	// Also check AVALANCHEGO_PATH env var
	if envPath := os.Getenv("AVALANCHEGO_PATH"); envPath != "" {
		locations = append([]string{envPath}, locations...)
	}

	for _, loc := range locations {
		absPath, err := filepath.Abs(loc)
		if err != nil {
			continue
		}
		if _, err := os.Stat(absPath); err == nil {
			return absPath, nil
		}
	}

	return "", fmt.Errorf("avalanchego binary not found. Set AVALANCHEGO_PATH or place binary in ./bin/avalanchego")
}

func findPluginDir() (string, error) {
	homeDir, _ := os.UserHomeDir()

	// Check common locations
	locations := []string{
		// Local plugins
		"./plugins",
		"../avalanchego/build/plugins",
		// User's avalanchego plugins
		filepath.Join(homeDir, ".avalanchego", "plugins"),
	}

	// Also check AVALANCHEGO_PLUGIN_DIR env var
	if envPath := os.Getenv("AVALANCHEGO_PLUGIN_DIR"); envPath != "" {
		locations = append([]string{envPath}, locations...)
	}

	for _, loc := range locations {
		absPath, err := filepath.Abs(loc)
		if err != nil {
			continue
		}
		if info, err := os.Stat(absPath); err == nil && info.IsDir() {
			// Check if subnet-evm plugin exists
			subnetEvmPath := filepath.Join(absPath, "srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy")
			if _, err := os.Stat(subnetEvmPath); err == nil {
				return absPath, nil
			}
		}
	}

	return "", fmt.Errorf("subnet-evm plugin not found. Set AVALANCHEGO_PLUGIN_DIR or place plugin in ./plugins/")
}

func loadGenesis(genesisPath string) ([]byte, error) {
	if genesisPath != "" {
		return os.ReadFile(genesisPath)
	}

	// Use default genesis
	return []byte(defaultGenesis), nil
}

func loadChainConfig(chainConfigPath string) ([]byte, error) {
	if chainConfigPath != "" {
		return os.ReadFile(chainConfigPath)
	}

	// Auto-detect RAM and generate optimized config
	totalRAM, err := DetectRAM()
	if err != nil {
		fmt.Printf("Warning: Could not detect RAM (%v), using conservative defaults\n", err)
		totalRAM = 16 * 1024 * 1024 * 1024 // 16GB fallback
	}

	ramCfg := CalculateRAMConfig(totalRAM)
	fmt.Printf("\n%s\n\n", ramCfg.String())

	return GenerateChainConfig(ramCfg)
}

// Default subnet-evm genesis optimized for maximum throughput benchmarking
// Based on verified source code analysis of subnet-evm
//
// Key optimizations:
// - gasLimit: 500M (500,000,000) - ~23,809 simple transfers per block
// - targetBlockRate: 1 second blocks
// - targetGas: MaxUint64 - ensures base fee can NEVER increase
// - baseFeeChangeDenominator: MaxInt64 - fees essentially frozen at minBaseFee
// - minBaseFee: 1 wei - lowest possible
// - All block gas costs zeroed - no block production overhead
//
// Theoretical max TPS: ~16,000 (limited by 1.8MB tx size per block in miner/worker.go:64)
//
// EWOQ key pre-funded with max balance for benchmarking
var defaultGenesis = `{
  "config": {
    "chainId": 99999,
    "feeConfig": {
      "gasLimit": 500000000,
      "targetBlockRate": 1,
      "minBaseFee": 1,
      "targetGas": 18446744073709551615,
      "baseFeeChangeDenominator": 9223372036854775807,
      "minBlockGasCost": 0,
      "maxBlockGasCost": 0,
      "blockGasCostStep": 0
    }
  },
  "alloc": {
    "8db97C7cEcE249c2b98bDC0226Cc4C2A57BF52FC": {
      "balance": "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"
    }
  },
  "nonce": "0x0",
  "timestamp": "0x0",
  "extraData": "0x00",
  "gasLimit": "0x1DCD6500",
  "difficulty": "0x0",
  "mixHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
  "coinbase": "0x0000000000000000000000000000000000000000",
  "number": "0x0",
  "gasUsed": "0x0",
  "parentHash": "0x0000000000000000000000000000000000000000000000000000000000000000"
}`

// Default chain config optimized for maximum throughput benchmarking
// Based on verified source code analysis of subnet-evm
//
// Key optimizations:
// - min-delay-target: 0 - removes ACP-226 minimum block delay
// - Massive caches for high-memory systems (100GB+ trie caches)
// - Large tx pool for sustained high throughput
// - Aggressive gossip settings for fast tx propagation
// - Disabled tx indexing for benchmark speed
var defaultChainConfig = `{
  "min-delay-target": 200000000,

  "pruning-enabled": false,
  "commit-interval": 16384,
  "accepted-queue-limit": 256,

  "trie-clean-cache": 102400,
  "trie-dirty-cache": 102400,
  "trie-dirty-commit-target": 2048,
  "trie-prefetcher-parallelism": 64,
  "snapshot-cache": 51200,
  "accepted-cache-size": 256,
  "state-sync-server-trie-cache": 10240,

  "tx-pool-price-limit": 1,
  "tx-pool-price-bump": 1,
  "tx-pool-account-slots": 256,
  "tx-pool-global-slots": 262144,
  "tx-pool-account-queue": 1024,
  "tx-pool-global-queue": 131072,
  "tx-pool-lifetime": "1h",

  "push-gossip-frequency": "50ms",
  "pull-gossip-frequency": "500ms",
  "regossip-frequency": "10s",
  "push-gossip-num-validators": 200,
  "push-regossip-num-validators": 50,
  "push-gossip-percent-stake": 0.5,

  "rpc-gas-cap": 500000000,
  "rpc-tx-fee-cap": 1000000,
  "batch-request-limit": 10000,
  "batch-response-max-size": 104857600,

  "max-outbound-active-requests": 64,

  "skip-tx-indexing": true,
  "transaction-history": 0,

  "metrics-expensive-enabled": false,
  "allow-unfinalized-queries": true,
  "allow-unprotected-txs": true,
  "local-txs-enabled": true,

  "snapshot-wait": false,
  "snapshot-verification-enabled": false,

  "populate-missing-tries": null,
  "populate-missing-tries-parallelism": 4096,

  "state-sync-enabled": false,

  "log-level": "warn"
}`

// NodeHealthStatus contains health information about a node
type NodeHealthStatus struct {
	URI            string
	Healthy        bool
	Reachable      bool
	L1Healthy      bool
	L1Height       uint64
	DiskPercent    int
	ConnectedPeers int
}

// CheckNodeHealth checks the health of a specific node
func CheckNodeHealth(nodeURI, chainID string) NodeHealthStatus {
	status := NodeHealthStatus{
		URI:       nodeURI,
		Reachable: true,
	}

	client := &http.Client{Timeout: 2 * time.Second}

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "health.health",
		"params":  map[string]interface{}{},
		"id":      1,
	}

	body, _ := json.Marshal(req)
	resp, err := client.Post(nodeURI+"/ext/health", "application/json", strings.NewReader(string(body)))
	if err != nil {
		status.Reachable = false
		return status
	}
	defer resp.Body.Close()

	data, err := readAllBytes(resp.Body)
	if err != nil {
		status.Reachable = false
		return status
	}

	var result struct {
		Result struct {
			Healthy bool                   `json:"healthy"`
			Checks  map[string]interface{} `json:"checks"`
		} `json:"result"`
	}

	if err := json.Unmarshal(data, &result); err != nil {
		status.Reachable = false
		return status
	}

	status.Healthy = result.Result.Healthy

	// Parse individual checks
	for name, checkData := range result.Result.Checks {
		checkMap, ok := checkData.(map[string]interface{})
		if !ok {
			continue
		}

		switch name {
		case "diskspace":
			if msg, ok := checkMap["message"].(map[string]interface{}); ok {
				if pct, ok := msg["availableDiskPercentage"].(float64); ok {
					status.DiskPercent = int(pct)
				}
			}

		case "network":
			if msg, ok := checkMap["message"].(map[string]interface{}); ok {
				if peers, ok := msg["connectedPeers"].(float64); ok {
					status.ConnectedPeers = int(peers)
				}
			}

		default:
			// Check if this is the L1 chain
			if name == chainID {
				status.L1Healthy = true
				if msg, ok := checkMap["message"].(map[string]interface{}); ok {
					if engine, ok := msg["engine"].(map[string]interface{}); ok {
						if consensus, ok := engine["consensus"].(map[string]interface{}); ok {
							if height, ok := consensus["lastAcceptedHeight"].(float64); ok {
								status.L1Height = uint64(height)
							}
						}
					}
				}
				// Check if L1 has errors
				if _, hasError := checkMap["error"]; hasError {
					status.L1Healthy = false
				}
			}
		}
	}

	return status
}

func readAllBytes(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return buf, err
		}
	}
	return buf, nil
}

// writeChainConfig writes the chain config to a node's config directory
func writeChainConfig(nodeDir, chainID string, customConfig []byte) error {
	chainConfigDir := filepath.Join(nodeDir, "configs", "chains", chainID)
	if err := os.MkdirAll(chainConfigDir, 0755); err != nil {
		return fmt.Errorf("failed to create chain config dir: %w", err)
	}

	configPath := filepath.Join(chainConfigDir, "config.json")

	configData := customConfig
	if configData == nil {
		configData = []byte(defaultChainConfig)
	}

	if err := os.WriteFile(configPath, configData, 0644); err != nil {
		return fmt.Errorf("failed to write chain config: %w", err)
	}

	return nil
}
