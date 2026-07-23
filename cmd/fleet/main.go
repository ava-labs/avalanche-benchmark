package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/pchainsource"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	program := filepath.Base(os.Args[0])
	switch {
	case len(os.Args) == 5 && os.Args[1] == "pchain" && os.Args[2] == "follow":
		environment, err := config.LoadNetworkEnvironment(filepath.Join(root, ".env"))
		if err != nil {
			return err
		}
		return pchainsource.New(root, environment.Network, environment.PChainAPI, os.Stdout).
			Follow(context.Background(), os.Args[3], os.Args[4])
	case len(os.Args) == 3 && os.Args[1] == "pchain" && os.Args[2] == "freeze":
		environment, err := config.LoadNetworkEnvironment(filepath.Join(root, ".env"))
		if err != nil {
			return err
		}
		return pchainsource.New(root, environment.Network, environment.PChainAPI, os.Stdout).
			Freeze(context.Background())
	case len(os.Args) == 3 && os.Args[1] == "pchain" && os.Args[2] == "status":
		environment, err := config.LoadNetworkEnvironment(filepath.Join(root, ".env"))
		if err != nil {
			return err
		}
		return pchainsource.New(root, environment.Network, environment.PChainAPI, os.Stdout).
			Status(context.Background())
	default:
		return fmt.Errorf("usage:\n%s", usage(program))
	}
}

func usage(program string) string {
	return fmt.Sprintf(
		"  %s pchain follow <upstream-ip:port> <upstream-node-id>\n  %s pchain freeze\n  %s pchain status",
		program,
		program,
		program,
	)
}
