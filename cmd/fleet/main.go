package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

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
	deployer := fleet.NewDeployer(root, os.Stdout)
	ctx := context.Background()
	arguments := os.Args[1:]
	if len(arguments) == 0 {
		return fmt.Errorf("usage:\n%s", usage(program))
	}

	switch arguments[0] {
	case "deploy":
		if len(arguments) != 2 {
			return fmt.Errorf("usage:\n%s", usage(program))
		}
		return deployer.Deploy(ctx, arguments[1])
	case "pchain":
		if len(arguments) != 2 {
			return fmt.Errorf("usage:\n%s", usage(program))
		}
		switch arguments[1] {
		case "archive":
			return deployer.ArchivePChain(ctx)
		case "follow":
			return deployer.FollowPChain(ctx)
		case "freeze":
			return deployer.FreezePChain(ctx)
		}
	case "status":
		if len(arguments) != 1 {
			return fmt.Errorf("status takes no selectors")
		}
		return deployer.Status(ctx)
	case "start":
		return deployer.Start(ctx, arguments[1:])
	case "stop":
		return deployer.Stop(ctx, arguments[1:])
	case "destroy":
		return deployer.Destroy(ctx, arguments[1:])
	case "place":
		if len(arguments) != 3 {
			return fmt.Errorf("usage:\n  %s place <identity-letter> <node>", program)
		}
		node, err := strconv.Atoi(arguments[2])
		if err != nil {
			return fmt.Errorf("place node must be an inventory node number, got %q", arguments[2])
		}
		return deployer.Place(ctx, arguments[1], node)
	case "apply-placement":
		if len(arguments) != 1 {
			return fmt.Errorf("apply-placement takes no arguments")
		}
		return deployer.ApplyPlacement(ctx)
	}
	return fmt.Errorf("usage:\n%s", usage(program))
}

func usage(program string) string {
	lines := []string{
		"deploy <frozen|follow>",
		"pchain <archive|follow|freeze>",
		"status",
		"start [<node>|dc=<tag> ...]",
		"stop [<node>|dc=<tag> ...]",
		"destroy [<node>|dc=<tag> ...]",
		"place <identity-letter> <node>",
		"apply-placement",
	}
	text := ""
	for _, line := range lines {
		text += fmt.Sprintf("  %s %s\n", program, line)
	}
	return text
}
