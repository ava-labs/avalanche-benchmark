package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

// config is the resolved runtime configuration for the failover lab.
// It merges values from .env (host inventory + ssh user) and
// staking/node-ids.env (hardcoded NodeIDs derived from committed certs).
type config struct {
	repoRoot string

	sshUser   string
	sshKey    string // optional; empty means use ssh defaults
	controlIP string
	dc1IPs    []string
	dc2IPs    []string

	control1NodeID string
	control2NodeID string
	dc1NodeIDs     []string

	// Local paths under repoRoot.
	binDir     string
	stakingDir string
	configDir  string

	subnetEvmID string

	// Validator placement chosen for this run. Set by the CLI after
	// loadConfig. dc1+dc2 must equal 5 (the registered validator-set size).
	arch archSpec
}

const subnetEvmID = "srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"

func loadConfig() (*config, error) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return nil, err
	}

	envPath := filepath.Join(repoRoot, ".env")
	if err := godotenv.Load(envPath); err != nil {
		return nil, fmt.Errorf("load %s: %w", envPath, err)
	}

	nodeIDsPath := filepath.Join(repoRoot, "staking", "node-ids.env")
	if err := godotenv.Load(nodeIDsPath); err != nil {
		return nil, fmt.Errorf("load %s: %w", nodeIDsPath, err)
	}

	c := &config{
		repoRoot:       repoRoot,
		sshUser:        env("SSH_USER"),
		sshKey:         os.Getenv("SSH_KEY"), // optional
		controlIP:      env("CONTROL_IP"),
		dc1IPs:         splitList("DC1_NODE_IPS"),
		dc2IPs:         splitList("DC2_NODE_IPS"),
		control1NodeID: env("CONTROL_1_NODE_ID"),
		control2NodeID: env("CONTROL_2_NODE_ID"),
		binDir:         filepath.Join(repoRoot, "bin"),
		stakingDir:     filepath.Join(repoRoot, "staking"),
		configDir:      filepath.Join(repoRoot, "config"),
		subnetEvmID:    subnetEvmID,
	}

	for i := 1; i <= 5; i++ {
		c.dc1NodeIDs = append(c.dc1NodeIDs, env(fmt.Sprintf("DC1_%d_NODE_ID", i)))
	}

	if len(c.dc1IPs) != 5 {
		return nil, fmt.Errorf("DC1_NODE_IPS must have 5 entries, got %d", len(c.dc1IPs))
	}
	if len(c.dc2IPs) != 5 {
		return nil, fmt.Errorf("DC2_NODE_IPS must have 5 entries, got %d", len(c.dc2IPs))
	}
	return c, nil
}

func env(k string) string {
	v := os.Getenv(k)
	if v == "" {
		panic(fmt.Sprintf("required env var %s is empty", k))
	}
	return v
}

func splitList(k string) []string {
	raw := env(k)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// findRepoRoot walks up from the executable to find the dir containing
// .env / staking/node-ids.env. We don't require .env to exist (it's
// gitignored); we anchor on staking/node-ids.env which is committed.
func findRepoRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "staking", "node-ids.env")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Fall back to cwd.
	cwd, _ := os.Getwd()
	if _, err := os.Stat(filepath.Join(cwd, "staking", "node-ids.env")); err == nil {
		return cwd, nil
	}
	return "", fmt.Errorf("could not locate repo root (looked for staking/node-ids.env)")
}
