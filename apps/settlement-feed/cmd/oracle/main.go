package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ava-labs/avalanche-benchmark/apps/settlement-feed/internal/oraclerelay"
	"github.com/ava-labs/avalanche-benchmark/internal/config"
	ethcommon "github.com/ava-labs/libevm/common"
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
	switch {
	case len(os.Args) == 3 && os.Args[1] == "feed":
		return feed(root, os.Args[2], "")
	case len(os.Args) == 4 && os.Args[1] == "feed":
		return feed(root, os.Args[2], os.Args[3])
	case len(os.Args) == 5 && os.Args[1] == "relay":
		return relay(root, os.Args[2], os.Args[3], os.Args[4])
	case len(os.Args) == 2 && os.Args[1] == "upgrade":
		return upgradeCommand(root, "")
	case len(os.Args) == 3 && os.Args[1] == "upgrade":
		return upgradeCommand(root, os.Args[2])
	default:
		return fmt.Errorf("usage:\n%s", usage(filepath.Base(os.Args[0])))
	}
}

func usage(program string) string {
	return fmt.Sprintf(
		"  %s feed <node-url> [settlement-contract]         foreground mock price feeder. With an oracle\n                                                   L1 it submits to the aggregator on the oracle\n                                                   chain (pass an oracle node URL); without one it\n                                                   publishes rounds to the price aggregator on the\n                                                   main chain with type-2 priority-fee transactions\n                                                   (pass a main-chain RPC node URL). An optional\n                                                   deployed Settlement example address adds its\n                                                   canSettle gate to the exported metrics\n  %s relay <oracle-node-url> <main-node-url> <staking-ip:port,...>\n                                                   foreground Warp price relayer; collects each\n                                                   signature from the oracle validators over\n                                                   ACP-118 on their staking ports\n  %s upgrade [activation-minutes]                  write ./upgrade.json: the direct feed's\n                                                   accounts as a stateUpgrades entry (plus the\n                                                   main chain's Warp receiver on an oracle\n                                                   deployment), for adding the app to a RUNNING\n                                                   chain without recreating it. Apply with:\n                                                   fleet upgrade upgrade.json",
		program,
		program,
		program,
	)
}

func feed(root, nodeURL, settlementHex string) error {
	environment, err := config.LoadNetworkEnvironment(filepath.Join(root, ".env"))
	if err != nil {
		return err
	}
	var settlement ethcommon.Address
	if settlementHex != "" {
		if !ethcommon.IsHexAddress(settlementHex) {
			return fmt.Errorf("settlement contract must be a hex address, got %q", settlementHex)
		}
		settlement = ethcommon.HexToAddress(settlementHex)
	}
	deploymentDirectory := filepath.Join(root, "deployment")
	deployment, err := oraclerelay.LoadDeployment(filepath.Join(deploymentDirectory, "network.env"), environment.Network)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return oraclerelay.Feed(ctx, deployment, deploymentDirectory, nodeURL, settlement, os.Stdout)
}

func relay(root, oracleNodeURL, mainNodeURL, stakingList string) error {
	environment, err := config.LoadNetworkEnvironment(filepath.Join(root, ".env"))
	if err != nil {
		return err
	}
	deploymentDirectory := filepath.Join(root, "deployment")
	deployment, err := oraclerelay.LoadDeployment(filepath.Join(deploymentDirectory, "network.env"), environment.Network)
	if err != nil {
		return err
	}
	stakingAddresses, err := oraclerelay.ParseStakingAddresses(stakingList)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return oraclerelay.Relay(ctx, environment.PChainAPI, deployment, deploymentDirectory, oracleNodeURL, mainNodeURL, stakingAddresses, os.Stdout)
}
