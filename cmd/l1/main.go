package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/funding"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/topup"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/weights"
	"github.com/ava-labs/avalanchego/utils/units"
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
	case len(os.Args) == 2 && os.Args[1] == "create":
		return create(root)
	case len(os.Args) == 2 && os.Args[1] == "address":
		return showAddress(root)
	case len(os.Args) == 2 && os.Args[1] == "keygen":
		return generateKey(root)
	case len(os.Args) == 2 && os.Args[1] == "weights":
		return showWeights(root)
	case len(os.Args) == 3 && os.Args[1] == "topup":
		return topUp(root, os.Args[2])
	default:
		return fmt.Errorf("usage:\n  go run ./cmd/l1 create\n  go run ./cmd/l1 address\n  go run ./cmd/l1 keygen\n  go run ./cmd/l1 weights\n  go run ./cmd/l1 topup <days>")
	}
}

func create(root string) error {
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

func showAddress(root string) error {
	envPath := filepath.Join(root, ".env")
	environment, err := config.LoadEnvironment(envPath)
	if err != nil {
		return err
	}
	info, err := funding.Inspect(context.Background(), environment)
	if err != nil {
		return err
	}
	fmt.Printf("loaded %s\n", envPath)
	fmt.Printf("P-chain funding address: %s\n", info.Addresses.PChain)
	fmt.Printf("EVM genesis address: %s\n", info.Addresses.EVM)
	fmt.Printf("P-chain balance: %d.%09d AVAX\n", info.Balance/units.Avax, info.Balance%units.Avax)
	return nil
}

func generateKey(root string) error {
	envPath := filepath.Join(root, ".env")
	if err := funding.GenerateIntoEnvironment(envPath); err != nil {
		return err
	}
	fmt.Printf("generated FUNDING_PRIVATE_KEY in %s\n", envPath)
	return showAddress(root)
}

func showWeights(root string) error {
	envPath := filepath.Join(root, ".env")
	environment, err := config.LoadEnvironment(envPath)
	if err != nil {
		return err
	}
	statePath := filepath.Join(root, "deployment", "network.env")
	deployment, err := weights.LoadDeployment(statePath, environment.Network)
	if err != nil {
		return err
	}
	report, err := weights.Fetch(context.Background(), environment.PChainAPI, deployment)
	if err != nil {
		return err
	}
	fmt.Printf("management chain ID: %s\n", report.ManagementChainID)
	fmt.Printf("main chain ID: %s\n", report.MainChainID)
	fmt.Printf("validator fee price: %d nAVAX/second\n", report.FeePrice)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "L1\tNODE ID\tWEIGHT\tDAYS LEFT")
	for _, validator := range report.Validators {
		fmt.Fprintf(w, "%s\t%s\t%d\t%.2f\n", validator.L1, validator.NodeID, validator.Weight, validator.DaysLeft)
	}
	return w.Flush()
}

func topUp(root, rawDays string) error {
	days, err := strconv.ParseUint(rawDays, 10, 64)
	if err != nil || days == 0 {
		return fmt.Errorf("topup days must be a positive integer, got %q", rawDays)
	}
	environment, err := config.LoadEnvironment(filepath.Join(root, ".env"))
	if err != nil {
		return err
	}
	deployment, err := weights.LoadDeployment(
		filepath.Join(root, "deployment", "network.env"),
		environment.Network,
	)
	if err != nil {
		return err
	}
	return topup.Run(context.Background(), environment, deployment, days, os.Stdout)
}
