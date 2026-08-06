package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
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
	if err := config.InstallAPITokenFromRoot(root); err != nil {
		return err
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
		// The mode is optional and defaults to frozen, the isolated shape.
		// "follow" keeps the P-chain connected to the public network; see
		// playbooks/07-connected-pchain.md. A node number is never a mode,
		// so the forms stay unambiguous.
		mode := "frozen"
		rest := arguments[1:]
		if len(rest) > 0 && (rest[0] == "frozen" || rest[0] == "follow") {
			mode = rest[0]
			rest = rest[1:]
		}
		dryRun := false
		selectors := make([]string, 0, len(rest))
		for _, argument := range rest {
			if argument == "--dry-run" {
				dryRun = true
				continue
			}
			selectors = append(selectors, argument)
		}
		return deployer.Deploy(ctx, mode, selectors, dryRun)
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
		case "start":
			return deployer.StartPChain(ctx)
		case "stop":
			return deployer.StopPChain(ctx)
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
	case "upgrade":
		// The chain is optional and defaults to main, matching the single
		// chain every older inventory declares.
		chain := "main"
		rest := arguments[1:]
		if len(rest) >= 2 && rest[0] == "--chain" {
			chain = rest[1]
			rest = rest[2:]
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage:\n  %s upgrade [--chain <name>] <upgrade.json>", program)
		}
		return deployer.UpgradeChain(ctx, chain, rest[0])
	case "targets":
		if len(arguments) != 1 {
			return fmt.Errorf("targets takes no arguments; pipe it: %s targets > monitoring/targets.json", program)
		}
		return deployer.Targets()
	case "app":
		rest := arguments[1:]
		appUsage := fmt.Errorf("usage:\n  %s app install <name> [--chain <name>]\n  %s app list", program, program)
		if len(rest) == 0 {
			return appUsage
		}
		switch rest[0] {
		case "list":
			if len(rest) != 1 {
				return appUsage
			}
			return deployer.ListApps()
		case "install":
			// The target chain resolves as: the --chain flag, else the
			// manifest's chain, else main. One chain per install.
			name := ""
			chain := ""
			install := rest[1:]
			for index := 0; index < len(install); index++ {
				if install[index] == "--chain" {
					if index+1 >= len(install) {
						return appUsage
					}
					chain = install[index+1]
					index++
					continue
				}
				if name != "" {
					return appUsage
				}
				name = install[index]
			}
			if name == "" {
				return appUsage
			}
			return deployer.InstallApp(ctx, name, chain)
		}
		return appUsage
	case "place":
		if len(arguments) != 3 {
			return fmt.Errorf("usage:\n  %s place <identity-letter> <node>", program)
		}
		node, err := strconv.Atoi(arguments[2])
		if err != nil {
			return fmt.Errorf("place node must be an inventory node number, got %q", arguments[2])
		}
		return deployer.Place(ctx, arguments[1], node)
	}
	return fmt.Errorf("usage:\n%s", usage(program))
}

func usage(program string) string {
	lines := []string{
		"deploy [frozen|follow] [--dry-run] [<node> ...]   (default: frozen)",
		"pchain <archive|follow|freeze|start|stop>",
		"status",
		"start [<node> ...]",
		"stop [<node> ...]",
		"destroy <node> [<node> ...]",
		"upgrade [--chain <name>] <upgrade.json>",
		"app install <name> [--chain <name>]",
		"app list",
		"targets   (print Prometheus scrape targets; pipe to monitoring/targets.json)",
		"place <identity-letter> <node>",
	}
	text := ""
	for _, line := range lines {
		text += fmt.Sprintf("  %s %s\n", program, line)
	}
	return text
}
