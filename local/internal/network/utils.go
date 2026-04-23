package network

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

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
	if genesisPath == "" {
		genesisPath = "./genesis.json"
	}
	return os.ReadFile(genesisPath)
}

func loadChainConfig(chainConfigPath string) ([]byte, error) {
	if chainConfigPath == "" {
		chainConfigPath = "./chain-config.json"
	}
	return os.ReadFile(chainConfigPath)
}

func setupNodeLogging(cmd *exec.Cmd, nodeDir, name string) {
	logDir := filepath.Join(nodeDir, "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		fmt.Printf("log setup failed for %s: %v\n", name, err)
		return
	}

	logPath := filepath.Join(logDir, "process.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("log setup failed for %s: %v\n", name, err)
		return
	}

	// Leave file open so the process can keep writing.
	cmd.Stdout = f
	cmd.Stderr = f
}

func ensureStakingKeys(networkDir string) error {
	srcDir := filepath.Join(".", "staking", "local")
	if _, err := os.Stat(srcDir); err != nil {
		return fmt.Errorf("staking keys not found at %s", srcDir)
	}

	dstDir := filepath.Join(networkDir, "staking", "local")
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("failed to create staking dir: %w", err)
	}

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("failed to read staking dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		srcPath := filepath.Join(srcDir, entry.Name())
		dstPath := filepath.Join(dstDir, entry.Name())

		data, err := os.ReadFile(srcPath)
		if err != nil {
			return fmt.Errorf("failed to read staking key %s: %w", srcPath, err)
		}
		if err := os.WriteFile(dstPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write staking key %s: %w", dstPath, err)
		}
	}

	return nil
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
		return fmt.Errorf("chain config data is nil")
	}

	if err := os.WriteFile(configPath, configData, 0644); err != nil {
		return fmt.Errorf("failed to write chain config: %w", err)
	}

	return nil
}
