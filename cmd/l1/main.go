package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"text/tabwriter"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/destroy"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/funding"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/setweight"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/topup"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/weights"
	"github.com/ava-labs/avalanchego/ids"
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
	case len(os.Args) == 2 && os.Args[1] == "destroy":
		return destroyL1s(root)
	case len(os.Args) == 3 && os.Args[1] == "topup":
		return topUp(root, os.Args[2])
	case len(os.Args) == 4 && os.Args[1] == "set-weight":
		return setWeight(root, os.Args[2], os.Args[3])
	default:
		return fmt.Errorf("usage:\n%s", usage(filepath.Base(os.Args[0])))
	}
}

func usage(program string) string {
	return fmt.Sprintf(
		"  %s create\n  %s address\n  %s keygen\n  %s weights\n  %s topup <days>\n  %s set-weight <main-identity> <1|1000|100000>\n  %s destroy",
		program,
		program,
		program,
		program,
		program,
		program,
		program,
	)
}

func setWeight(root, rawIdentity, rawWeight string) error {
	identityNumber, err := strconv.Atoi(rawIdentity)
	if err != nil || identityNumber <= 0 {
		return fmt.Errorf("set-weight identity must be a positive node number, got %q", rawIdentity)
	}
	targetWeight, err := strconv.ParseUint(rawWeight, 10, 64)
	if err != nil {
		return fmt.Errorf("set-weight weight must be 1, 1000, or 100000, got %q", rawWeight)
	}
	if err := setweight.ValidateWeight(targetWeight); err != nil {
		return err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	deployment, err := weights.LoadDeployment(
		filepath.Join(root, "deployment", "network.env"),
		cfg.Environment.Network,
	)
	if err != nil {
		return err
	}
	return setweight.Run(
		context.Background(),
		cfg,
		deployment,
		filepath.Join(root, "deployment"),
		identityNumber,
		targetWeight,
		os.Stdout,
	)
}

func create(root string) error {
	deploymentPath := filepath.Join(root, "deployment")
	if _, err := os.Stat(deploymentPath); err == nil {
		return fmt.Errorf("chain already exists in ./deployment; delete ./deployment only if you want a new chain")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect ./deployment: %w", err)
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
		deploymentPath,
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
	if err := rejectDestroyedDeployment(context.Background(), root, environment); err != nil {
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

func rejectDestroyedDeployment(ctx context.Context, root string, environment config.Environment) error {
	statePath := filepath.Join(root, "deployment", "network.env")
	if _, err := os.Stat(statePath); errors.Is(err, os.ErrNotExist) {
		// Address must remain available before creation so an imported funding
		// key can be funded. Once creation state exists, zero active validators
		// means the benchmark lifecycle is over and commands must fail loudly.
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect creation state %s: %w", statePath, err)
	}
	deployment, err := weights.LoadDeployment(statePath, environment.Network)
	if err != nil {
		return err
	}
	// Do not mirror lifecycle state in a local flag. Height-consistent P-Chain
	// validator balances are the source of truth: zero active validators means
	// this chain is destroyed.
	_, err = weights.Fetch(ctx, environment.PChainAPI, deployment)
	return err
}

func generateKey(root string) error {
	statePath := filepath.Join(root, "deployment", "network.env")
	if _, err := os.Stat(statePath); err == nil {
		// Replacing the funding identity after creation would disconnect local
		// configuration from every on-chain owner. Key generation is therefore
		// strictly a pre-creation operation, regardless of validator activity.
		return fmt.Errorf("keygen is only valid before creation; deployment state already exists at %s", statePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect creation state %s: %w", statePath, err)
	}
	envPath := filepath.Join(root, ".env")
	if err := funding.GenerateIntoEnvironment(envPath); err != nil {
		return err
	}
	fmt.Printf("generated FUNDING_PRIVATE_KEY in %s\n", envPath)
	return showAddress(root)
}

func showWeights(root string) error {
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	statePath := filepath.Join(root, "deployment", "network.env")
	deployment, err := weights.LoadDeployment(statePath, cfg.Environment.Network)
	if err != nil {
		return err
	}
	identityNumbers, err := loadIdentityNumbers(filepath.Join(root, "deployment"), cfg)
	if err != nil {
		return err
	}
	report, err := weights.Fetch(context.Background(), cfg.Environment.PChainAPI, deployment)
	if err != nil {
		return err
	}
	for _, validator := range report.Validators {
		if _, ok := identityNumbers[identityKey{L1: validator.L1, NodeID: validator.NodeID}]; !ok {
			return fmt.Errorf("%s validator %s has no local identity", validator.L1, validator.NodeID)
		}
	}
	sort.Slice(report.Validators, func(i, j int) bool {
		left := identityNumbers[identityKey{L1: report.Validators[i].L1, NodeID: report.Validators[i].NodeID}]
		right := identityNumbers[identityKey{L1: report.Validators[j].L1, NodeID: report.Validators[j].NodeID}]
		if report.Validators[i].L1 != report.Validators[j].L1 {
			return report.Validators[i].L1 == "management"
		}
		return left < right
	})
	fmt.Printf("management chain ID: %s\n", report.ManagementChainID)
	fmt.Printf("main chain ID: %s\n", report.MainChainID)
	fmt.Printf("validator fee price: %d nAVAX/second\n", report.FeePrice)
	fmt.Printf("validator fee cost: %.6f AVAX/30 days per validator\n", float64(report.FeePrice)*30*24*60*60/float64(units.Avax))
	printTable := func(l1 string) error {
		fmt.Printf("\n%s validators:\n", l1)
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "IDENTITY\tNODE ID\tWEIGHT\tDAYS LEFT")
		for _, validator := range report.Validators {
			if validator.L1 != l1 {
				continue
			}
			identityNumber := identityNumbers[identityKey{L1: validator.L1, NodeID: validator.NodeID}]
			fmt.Fprintf(w, "%d\t%s\t%d\t%.2f\n", identityNumber, validator.NodeID, validator.Weight, validator.DaysLeft)
		}
		return w.Flush()
	}
	if err := printTable("management"); err != nil {
		return err
	}
	return printTable("main")
}

type identityKey struct {
	L1     string
	NodeID ids.NodeID
}

func loadIdentityNumbers(deploymentDirectory string, cfg config.Config) (map[identityKey]int, error) {
	numbers := make(map[identityKey]int, len(cfg.Nodes)+cfg.Environment.ManagerCommittee)
	load := func(l1 string, number int, path string) error {
		nodeID, err := identity.LoadNodeID(path)
		if err != nil {
			return err
		}
		numbers[identityKey{L1: l1, NodeID: nodeID}] = number
		return nil
	}
	for _, node := range cfg.Nodes {
		if node.Role != config.RoleValidator {
			continue
		}
		if err := load("main", node.Number, filepath.Join(deploymentDirectory, "nodes", strconv.Itoa(node.Number), "staker.crt")); err != nil {
			return nil, fmt.Errorf("load main identity %d: %w", node.Number, err)
		}
	}
	for number := 1; number <= cfg.Environment.ManagerCommittee; number++ {
		if err := load("management", number, filepath.Join(deploymentDirectory, "manager", strconv.Itoa(number), "staker.crt")); err != nil {
			return nil, fmt.Errorf("load management identity %d: %w", number, err)
		}
	}
	return numbers, nil
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

func destroyL1s(root string) error {
	environment, err := config.LoadEnvironment(filepath.Join(root, ".env"))
	if err != nil {
		return err
	}
	statePath := filepath.Join(root, "deployment", "network.env")
	deployment, err := weights.LoadDeployment(
		statePath,
		environment.Network,
	)
	if err != nil {
		return err
	}
	return destroy.Run(context.Background(), environment, deployment, os.Stdout)
}
