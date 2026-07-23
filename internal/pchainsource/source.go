package pchainsource

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type nodeConfig struct {
	NetworkID                 string  `json:"network-id"`
	DataDir                   string  `json:"data-dir"`
	HTTPHost                  string  `json:"http-host"`
	HTTPPort                  uint16  `json:"http-port"`
	StakingPort               uint16  `json:"staking-port"`
	PartialSyncPrimaryNetwork bool    `json:"partial-sync-primary-network"`
	PChainFollowOnly          bool    `json:"p-chain-follow-only"`
	BootstrapIPs              *string `json:"bootstrap-ips,omitempty"`
	BootstrapIDs              *string `json:"bootstrap-ids,omitempty"`
}

type Manager struct {
	root    string
	network string
	out     io.Writer
	exec    func(string, []string, []string) error
}

func New(root, network string, out io.Writer) *Manager {
	return &Manager{
		root:    root,
		network: network,
		out:     out,
		exec:    syscall.Exec,
	}
}

func (m *Manager) Start(mode string) error {
	if mode != "following" && mode != "frozen" {
		return fmt.Errorf("P-chain mode must be following or frozen, got %q", mode)
	}

	binaryPath := filepath.Join(m.root, "bin", "avalanchego")
	info, err := os.Stat(binaryPath)
	if err != nil {
		return fmt.Errorf("required packaged binary %s is unavailable: %w", binaryPath, err)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("required packaged binary %s is not executable", binaryPath)
	}

	sourceDir := filepath.Join(m.root, "data", "pchain-source")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		return fmt.Errorf("create P-chain source directory %s: %w", sourceDir, err)
	}
	cfg := nodeConfig{
		NetworkID:                 m.network,
		DataDir:                   sourceDir,
		HTTPHost:                  "127.0.0.1",
		HTTPPort:                  9650,
		StakingPort:               9651,
		PartialSyncPrimaryNetwork: true,
		PChainFollowOnly:          true,
	}
	if mode == "frozen" {
		empty := ""
		cfg.BootstrapIPs = &empty
		cfg.BootstrapIDs = &empty
	}

	configPath := filepath.Join(sourceDir, "config.json")
	if err := writeJSON(configPath, cfg); err != nil {
		return err
	}

	fmt.Fprintf(m.out, "starting P-chain source in %s mode\n", mode)
	return m.exec(
		binaryPath,
		[]string{binaryPath, "--config-file=" + configPath},
		os.Environ(),
	)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".config-")
	if err != nil {
		return fmt.Errorf("create temporary P-chain config: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install P-chain config %s: %w", path, err)
	}
	return nil
}
