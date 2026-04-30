package network

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// startNode starts a primary network validator node
func startNode(ctx context.Context, avalanchego, networkDir string, nodeIndex int, pluginDir, bootstrapNodeID string) (*NodeInfo, error) {
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

	configPath, err := ensureSharedNodeConfig()
	if err != nil {
		return nil, err
	}

	args := buildNodeArgs(httpPort, stakingPort, nodeDir, pluginDir, configPath)

	// Add staking keys for validators (up to 5 pre-configured)
	stakingKeysDir := filepath.Join(networkDir, "staking", "local")

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
	setupNodeLogging(cmd, nodeDir, fmt.Sprintf("node-%d", nodeIndex))

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start avalanchego: %w", err)
	}

	// Wait for node to become healthy
	uri := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID, err := waitForNodeHealth(ctx, uri, cmd.Process.Pid, nodeStartupTimeout)
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
func startL1Node(ctx context.Context, avalanchego, networkDir string, nodeIndex int, pluginDir, bootstrapNodeID, subnetID, nodeType string) (*NodeInfo, error) {
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

	configPath, err := ensureSharedNodeConfig()
	if err != nil {
		return nil, err
	}

	args := buildNodeArgs(httpPort, stakingPort, nodeDir, pluginDir, configPath)

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
	setupNodeLogging(cmd, nodeDir, fmt.Sprintf("l1-%s-%d", nodeType, nodeIndex))

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start avalanchego: %w", err)
	}

	// Wait for node to become healthy
	uri := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID, err := waitForNodeHealth(ctx, uri, cmd.Process.Pid, nodeStartupTimeout)
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
func buildNodeArgs(httpPort, stakingPort int, nodeDir, pluginDir, configPath string) []string {
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
		fmt.Sprintf("--config-file=%s", configPath),
	}

	// Allow local benchmark runs to lower AvalancheGo's disk-space guard
	// without changing the default repo behavior for everyone.
	if required := os.Getenv("BENCHMARK_DISK_REQUIRED_PERCENT"); required != "" {
		args = append(args, fmt.Sprintf("--system-tracker-disk-required-available-space-percentage=%s", required))
	}
	if warning := os.Getenv("BENCHMARK_DISK_WARNING_PERCENT"); warning != "" {
		args = append(args, fmt.Sprintf("--system-tracker-disk-warning-available-space-percentage=%s", warning))
	}

	return args
}

func ensureSharedNodeConfig() (string, error) {
	configPath := "./node-config.json"
	if _, err := os.Stat(configPath); err != nil {
		return "", fmt.Errorf("node config not found: %s", configPath)
	}
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve node config path: %w", err)
	}
	return absPath, nil
}
