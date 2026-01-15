package config

import (
	"encoding/json"
	"os"
)

// Config holds the benchmark configuration
type Config struct {
	// Network configuration
	PrimaryNodeCount     int `json:"primaryNodeCount"`     // Number of primary network nodes (default: 2, min: 2)
	L1ValidatorNodeCount int `json:"l1ValidatorNodeCount"` // Number of L1 validator nodes (default: 2)
	L1RPCNodeCount       int `json:"l1RpcNodeCount"`       // Number of L1 RPC-only nodes (default: 1, not validators)

	// Node flags (passed to avalanchego)
	NodeFlags map[string]string `json:"nodeFlags,omitempty"`

	// Chain configuration
	ChainConfig map[string]interface{} `json:"chainConfig,omitempty"`
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	return &Config{
		PrimaryNodeCount:     2, // Minimum 2 for connectivity
		L1ValidatorNodeCount: 2, // 2 validators for redundancy
		L1RPCNodeCount:       1, // 1 RPC-only node (separate from validators)
		NodeFlags: map[string]string{
			"log-level": "INFO",
		},
	}
}

// Load loads configuration from a JSON file
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// Ensure minimum values
	if cfg.PrimaryNodeCount < 2 {
		cfg.PrimaryNodeCount = 2 // Minimum 2 for connectivity
	}
	if cfg.L1ValidatorNodeCount < 1 {
		cfg.L1ValidatorNodeCount = 1 // At least 1 validator required
	}
	if cfg.L1RPCNodeCount < 0 {
		cfg.L1RPCNodeCount = 0 // RPC nodes are optional
	}

	return cfg, nil
}

// Save saves configuration to a JSON file
func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
