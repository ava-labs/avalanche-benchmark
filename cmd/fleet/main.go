package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fleet"
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
	case len(os.Args) == 3 && os.Args[1] == "deploy":
		return fleet.NewDeployer(root, os.Stdout).Deploy(context.Background(), os.Args[2])
	case len(os.Args) == 3 && os.Args[1] == "pchain" && os.Args[2] == "archive":
		return fleet.NewDeployer(root, os.Stdout).ArchivePChain(context.Background())
	default:
		return fmt.Errorf("usage:\n%s", usage(program))
	}
}

func usage(program string) string {
	return fmt.Sprintf(
		"  %s deploy <frozen|follow>\n  %s pchain archive",
		program,
		program,
	)
}
