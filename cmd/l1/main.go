package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 || os.Args[1] != "create" {
		return fmt.Errorf("usage: go run ./cmd/l1 create")
	}
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	fmt.Printf("loaded %s and %s\n", filepath.Join(root, ".env"), filepath.Join(root, "nodes.ini"))
	fmt.Printf("network %s, P-chain API %s, %d nodes, %d manager identities\n",
		cfg.Environment.Network,
		cfg.Environment.PChainAPI,
		len(cfg.Nodes),
		cfg.Environment.ManagerCommittee,
	)
	_, err = creation.Create(
		context.Background(),
		cfg,
		filepath.Join(root, "deployment"),
		filepath.Join(root, "genesis-template.json"),
	)
	return err
}
