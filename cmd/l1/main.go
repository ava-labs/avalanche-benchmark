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
	"github.com/ava-labs/avalanche-benchmark/remote/internal/keygen"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/setweight"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/topup"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/weights"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/units"
)

// defaultManagerCommittee is one signer: the minimum authority. Four is the
// smallest committee that survives losing one signing key.
const defaultManagerCommittee = 1

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
	switch {
	case (len(os.Args) == 2 || len(os.Args) == 3) && os.Args[1] == "keygen":
		managerCommittee := defaultManagerCommittee
		if len(os.Args) == 3 {
			managerCommittee, err = strconv.Atoi(os.Args[2])
			if err != nil {
				return fmt.Errorf("keygen manager committee must be 1 or 4, got %q", os.Args[2])
			}
		}
		return generateKeys(root, managerCommittee)
	case len(os.Args) == 2 && os.Args[1] == "create":
		return create(root)
	case len(os.Args) == 2 && os.Args[1] == "address":
		return showAddress(root)
	case len(os.Args) == 2 && os.Args[1] == "keygen-funding":
		return generateFundingKey(root)
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
		"  %s keygen [1|4]\n  %s create\n  %s address\n  %s keygen-funding\n  %s weights\n  %s topup <days>\n  %s set-weight <identity-letter> <1|1000|100000>\n  %s destroy",
		program,
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
	if _, err := identity.Index(rawIdentity); err != nil {
		return fmt.Errorf("set-weight %w", err)
	}
	targetWeight, err := strconv.ParseUint(rawWeight, 10, 64)
	if err != nil {
		return fmt.Errorf("set-weight weight must be 1, 1000, or 100000, got %q", rawWeight)
	}
	if err := setweight.ValidateWeight(targetWeight); err != nil {
		return err
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
	return setweight.Run(
		context.Background(),
		environment,
		deployment,
		filepath.Join(root, "deployment"),
		rawIdentity,
		targetWeight,
		os.Stdout,
	)
}

func create(root string) error {
	deploymentPath := filepath.Join(root, "deployment")
	statePath := filepath.Join(deploymentPath, "network.env")
	if _, err := os.Stat(statePath); err == nil {
		return fmt.Errorf("chain already exists at ./deployment/network.env; run destroy only if you want to remove it")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect ./deployment/network.env: %w", err)
	}
	// An absent deployment directory means there is nothing to create from, so
	// generate the identities rather than making the operator run a second
	// command that has exactly one sensible outcome. An existing but incomplete
	// directory is still an error: it means a previous run left state behind.
	if _, err := os.Stat(deploymentPath); errors.Is(err, os.ErrNotExist) {
		fmt.Println("no ./deployment; generating fresh identities first")
		if err := generateKeys(root, defaultManagerCommittee); err != nil {
			return err
		}
		fmt.Println()
	} else if err != nil {
		return fmt.Errorf("inspect ./deployment: %w", err)
	}

	envPath := filepath.Join(root, ".env")
	environment, err := config.LoadEnvironment(envPath)
	if err != nil {
		return err
	}
	publicPath := filepath.Join(deploymentPath, "public.json")
	fmt.Printf("loaded %s and %s\n", envPath, publicPath)
	fmt.Printf("network %s, P-chain API %s\n", environment.Network, environment.PChainAPI)
	_, err = creation.Create(
		context.Background(),
		environment,
		deploymentPath,
		root,
	)
	return err
}

func generateKeys(root string, managerCommittee int) error {
	if err := creation.ValidateManagerCommittee(managerCommittee); err != nil {
		return err
	}
	nodesPath := filepath.Join(root, "nodes.ini")
	nodes, err := config.LoadNodes(nodesPath)
	if err != nil {
		return err
	}
	deploymentPath := filepath.Join(root, "deployment")
	result, err := keygen.Generate(deploymentPath, nodes, managerCommittee)
	if err != nil {
		return err
	}
	roleCounts := make(map[config.Role]int, 6)
	for _, node := range result.Public.Nodes {
		roleCounts[node.Role]++
	}
	fmt.Printf("loaded %s\n", nodesPath)
	fmt.Printf(
		"generated keys: validators=%d rpc=%d pchain=%d archive=%d oracle-validators=%d oracle-rpc=%d managers=%d root=%s\n",
		roleCounts[config.RoleValidator],
		roleCounts[config.RoleRPC],
		roleCounts[config.RolePChain],
		roleCounts[config.RoleArchive],
		roleCounts[config.RoleOracleValidator],
		roleCounts[config.RoleOracleRPC],
		len(result.Public.Managers),
		deploymentPath,
	)
	fmt.Printf("public chain inputs: %s sha256:%s\n", filepath.Join(deploymentPath, "public.json"), result.Digest)
	fmt.Printf("Genesis EVM address: %s\n", result.Public.GenesisAddress)
	return nil
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
	fmt.Printf("funding-key EVM address: %s\n", info.Addresses.EVM)
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

func generateFundingKey(root string) error {
	statePath := filepath.Join(root, "deployment", "network.env")
	if _, err := os.Stat(statePath); err == nil {
		// Replacing the funding identity after creation would disconnect local
		// configuration from every on-chain owner. Key generation is therefore
		// strictly a pre-creation operation, regardless of validator activity.
		return fmt.Errorf("keygen-funding is only valid before creation; deployment state already exists at %s", statePath)
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
	environment, err := config.LoadEnvironment(filepath.Join(root, ".env"))
	if err != nil {
		return err
	}
	statePath := filepath.Join(root, "deployment", "network.env")
	deployment, err := weights.LoadDeployment(statePath, environment.Network)
	if err != nil {
		return err
	}
	identityNames, err := loadIdentityNames(filepath.Join(root, "deployment"))
	if err != nil {
		return err
	}
	report, err := weights.Fetch(context.Background(), environment.PChainAPI, deployment)
	if err != nil {
		return err
	}
	for _, validator := range report.Validators {
		if _, ok := identityNames[identityKey{L1: validator.L1, NodeID: validator.NodeID}]; !ok {
			return fmt.Errorf("%s validator %s has no local identity", validator.L1, validator.NodeID)
		}
	}
	l1Rank := map[string]int{"management": 0, "main": 1, "oracle": 2}
	sort.Slice(report.Validators, func(i, j int) bool {
		left := identityNames[identityKey{L1: report.Validators[i].L1, NodeID: report.Validators[i].NodeID}]
		right := identityNames[identityKey{L1: report.Validators[j].L1, NodeID: report.Validators[j].NodeID}]
		if report.Validators[i].L1 != report.Validators[j].L1 {
			return l1Rank[report.Validators[i].L1] < l1Rank[report.Validators[j].L1]
		}
		return left.Index < right.Index
	})
	fmt.Printf("management chain ID: %s\n", report.ManagementChainID)
	fmt.Printf("main chain ID: %s\n", report.MainChainID)
	if report.OracleChainID != ids.Empty {
		fmt.Printf("oracle chain ID: %s\n", report.OracleChainID)
	}
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
			identityName := identityNames[identityKey{L1: validator.L1, NodeID: validator.NodeID}]
			fmt.Fprintf(w, "%s\t%s\t%d\t%.2f\n", identityName.Name, validator.NodeID, validator.Weight, validator.DaysLeft)
		}
		return w.Flush()
	}
	if err := printTable("management"); err != nil {
		return err
	}
	if err := printTable("main"); err != nil {
		return err
	}
	if report.OracleChainID != ids.Empty {
		return printTable("oracle")
	}
	return nil
}

type identityKey struct {
	L1     string
	NodeID ids.NodeID
}

type identityName struct {
	Name  string
	Index int
}

func loadIdentityNames(deploymentDirectory string) (map[identityKey]identityName, error) {
	public, _, err := creation.LoadPublic(filepath.Join(deploymentDirectory, "public.json"))
	if err != nil {
		return nil, err
	}
	names := make(map[identityKey]identityName, len(public.Nodes)+len(public.Managers))
	load := func(l1, name, rawNodeID string) error {
		nodeID, err := ids.NodeIDFromString(rawNodeID)
		if err != nil {
			return err
		}
		index, err := identity.Index(name)
		if err != nil {
			return err
		}
		names[identityKey{L1: l1, NodeID: nodeID}] = identityName{Name: name, Index: index}
		return nil
	}
	for _, node := range public.Nodes {
		switch node.Role {
		case config.RoleValidator:
			if err := load("main", node.Identity, node.NodeID); err != nil {
				return nil, fmt.Errorf("load main identity %s: %w", node.Identity, err)
			}
		case config.RoleOracleValidator:
			if err := load("oracle", node.Identity, node.NodeID); err != nil {
				return nil, fmt.Errorf("load oracle identity %s: %w", node.Identity, err)
			}
		}
	}
	for _, manager := range public.Managers {
		if err := load("management", manager.Identity, manager.NodeID); err != nil {
			return nil, fmt.Errorf("load management identity %s: %w", manager.Identity, err)
		}
	}
	return names, nil
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
	deploymentPath := filepath.Join(root, "deployment")
	statePath := filepath.Join(deploymentPath, "network.env")
	deployment, err := weights.LoadDeploymentForDestroy(
		statePath,
		environment.Network,
	)
	if err != nil {
		return err
	}
	if err := destroy.Run(context.Background(), environment, deployment, os.Stdout); err != nil {
		// Keep every local key and transaction ID needed to retry a partial
		// destruction. Local state is removed only after all balances return.
		return err
	}
	if err := os.RemoveAll(deploymentPath); err != nil {
		return fmt.Errorf("remove destroyed deployment ./deployment: %w", err)
	}
	fmt.Println("removed ./deployment")
	return nil
}
