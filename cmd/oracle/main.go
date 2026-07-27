package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/oraclerelay"
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
		return feed(root, os.Args[2])
	case len(os.Args) == 5 && os.Args[1] == "relay":
		return relay(root, os.Args[2], os.Args[3], os.Args[4])
	default:
		return fmt.Errorf("usage:\n%s", usage(filepath.Base(os.Args[0])))
	}
}

func usage(program string) string {
	return fmt.Sprintf(
		"  %s feed <oracle-node-url>                        foreground mock price feeder\n  %s relay <oracle-node-url> <main-node-url> <staking-ip:port,...>\n                                                   foreground Warp price relayer; collects each\n                                                   signature from the oracle validators over\n                                                   ACP-118 on their staking ports",
		program,
		program,
	)
}

func feed(root, oracleNodeURL string) error {
	environment, err := config.LoadNetworkEnvironment(filepath.Join(root, ".env"))
	if err != nil {
		return err
	}
	deploymentDirectory := filepath.Join(root, "deployment")
	deployment, err := oraclerelay.LoadDeployment(filepath.Join(deploymentDirectory, "network.env"), environment.Network)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return oraclerelay.Feed(ctx, deployment, deploymentDirectory, oracleNodeURL, os.Stdout)
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
