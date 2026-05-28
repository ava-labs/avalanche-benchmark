package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/genesis"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm/signer"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
)

const (
	envFile                     = ".env"
	nodeIDsEnvFile              = "staking/node-ids.env"
	genesisSource               = "config/genesis.json"
	runtimeDataDir              = "runtime-data"
	l1EnvPath                   = runtimeDataDir + "/l1.env"
	l1ValidatorCountEnvKey      = "L1_VALIDATOR_COUNT"
	l1ValidatorStartIndexEnvKey = "L1_VALIDATOR_START_INDEX"
)

type l1Result struct {
	SubnetID            ids.ID
	ChainID             ids.ID
	ValidatorStartIndex int
	ValidatorCount      int
}

type l1ValidatorIdentity struct {
	Index     int
	NodeID    string
	SignerKey []byte
}

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: create-l1")
		os.Exit(2)
	}

	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	env, err := loadEnv(envFile)
	if err != nil {
		return err
	}
	nodeIDs, err := loadEnv(nodeIDsEnvFile)
	if err != nil {
		return err
	}

	validatorCount, err := requireEnvInt(env, l1ValidatorCountEnvKey)
	if err != nil {
		return err
	}
	if validatorCount < 1 {
		return fmt.Errorf("%s must set %s to a positive integer, got %d", envFile, l1ValidatorCountEnvKey, validatorCount)
	}
	validatorStartIndex, err := l1ValidatorStartIndex(env)
	if err != nil {
		return err
	}
	identityPoolSize, err := committedL1IdentityPoolSize(nodeIDs)
	if err != nil {
		return err
	}
	validatorEndIndex := validatorStartIndex + validatorCount - 1
	if validatorEndIndex > identityPoolSize {
		return fmt.Errorf("%s must set %s + %s - 1 to at most the committed L1 identity pool size %d, got %d + %d - 1 = %d",
			envFile,
			l1ValidatorStartIndexEnvKey,
			l1ValidatorCountEnvKey,
			identityPoolSize,
			validatorStartIndex,
			validatorCount,
			validatorEndIndex,
		)
	}
	validators, err := buildL1Validators(validatorStartIndex, validatorCount, nodeIDs)
	if err != nil {
		return err
	}
	if err := requireFiles(genesisSource); err != nil {
		return err
	}

	pchainAPI, err := pchainAPI(env)
	if err != nil {
		return err
	}
	genesisBytes, err := os.ReadFile(genesisSource)
	if err != nil {
		return fmt.Errorf("read %s: %w", genesisSource, err)
	}

	fmt.Println("[create-l1] connecting wallet to", pchainAPI)
	kc := secp256k1fx.NewKeychain(genesis.EWOQKey)
	wallet, err := primary.MakePWallet(ctx, pchainAPI, kc, primary.WalletConfig{})
	if err != nil {
		return fmt.Errorf("connect wallet: %w", err)
	}

	owner := &secp256k1fx.OutputOwners{
		Threshold: 1,
		Addrs:     []ids.ShortID{genesis.EWOQKey.Address()},
	}

	fmt.Println("[create-l1] CreateSubnetTx ...")
	subnetTx, err := wallet.IssueCreateSubnetTx(owner)
	if err != nil {
		return fmt.Errorf("create subnet: %w", err)
	}
	subnetID := subnetTx.ID()
	fmt.Println("[create-l1]   subnet:", subnetID)

	wallet, err = primary.MakePWallet(ctx, pchainAPI, kc, primary.WalletConfig{
		SubnetIDs: []ids.ID{subnetID},
	})
	if err != nil {
		return fmt.Errorf("re-sync wallet: %w", err)
	}

	fmt.Println("[create-l1] CreateChainTx ...")
	chainTx, err := wallet.IssueCreateChainTx(
		subnetID,
		genesisBytes,
		constants.SubnetEVMID,
		nil,
		"benchmarkchain",
	)
	if err != nil {
		return fmt.Errorf("create chain: %w", err)
	}
	chainID := chainTx.ID()
	fmt.Println("[create-l1]   chain:", chainID)

	fmt.Println("[create-l1] ConvertSubnetToL1Tx ...")
	_, err = wallet.IssueConvertSubnetToL1Tx(
		subnetID,
		chainID,
		[]byte{},
		validators,
	)
	if err != nil {
		return fmt.Errorf("convert subnet to L1: %w", err)
	}

	time.Sleep(5 * time.Second)

	result := l1Result{
		SubnetID:            subnetID,
		ChainID:             chainID,
		ValidatorStartIndex: validatorStartIndex,
		ValidatorCount:      validatorCount,
	}
	if err := writeL1Env(result); err != nil {
		return err
	}

	fmt.Println("L1 created")
	fmt.Println("  subnet:", result.SubnetID)
	fmt.Println("  chain: ", result.ChainID)
	fmt.Printf("  validators: l1/%d..%d (%d total)\n",
		result.ValidatorStartIndex,
		result.ValidatorStartIndex+result.ValidatorCount-1,
		result.ValidatorCount,
	)
	fmt.Println("  env:", l1EnvPath)
	return nil
}

func buildL1Validators(startIndex, count int, nodeIDs map[string]string) ([]*txs.ConvertSubnetToL1Validator, error) {
	identities, err := loadL1ValidatorIdentities(startIndex, count, nodeIDs)
	if err != nil {
		return nil, err
	}

	fmt.Printf("[create-l1] computing BLS PoPs for L1 validator identities %d..%d ...\n", startIndex, startIndex+count-1)
	validators := make([]*txs.ConvertSubnetToL1Validator, 0, count)
	for _, identity := range identities {
		nodeID, err := ids.NodeIDFromString(identity.NodeID)
		if err != nil {
			return nil, fmt.Errorf("parse L1 node ID %d (%q): %w", identity.Index, identity.NodeID, err)
		}
		sk, err := localsigner.FromBytes(identity.SignerKey)
		if err != nil {
			return nil, fmt.Errorf("load L1 signer key %d: %w", identity.Index, err)
		}
		pop, err := signer.NewProofOfPossession(sk)
		if err != nil {
			return nil, fmt.Errorf("build PoP for L1 validator %d: %w", identity.Index, err)
		}

		validators = append(validators, &txs.ConvertSubnetToL1Validator{
			NodeID:  nodeID.Bytes(),
			Weight:  units.Schmeckle,
			Balance: units.Avax,
			Signer:  *pop,
		})
		fmt.Printf("[create-l1]   l1-%d: %s\n", identity.Index, nodeID)
	}
	return validators, nil
}

func committedL1IdentityPoolSize(nodeIDs map[string]string) (int, error) {
	for i := 1; ; i++ {
		if strings.TrimSpace(nodeIDs[fmt.Sprintf("L1_%d_NODE_ID", i)]) == "" {
			return i - 1, nil
		}

		signerPath := filepath.Join("staking", "l1", strconv.Itoa(i), "signer.key")
		info, err := os.Stat(signerPath)
		if err == nil && !info.IsDir() {
			continue
		}
		if err == nil {
			return i - 1, nil
		}
		if os.IsNotExist(err) {
			return i - 1, nil
		}
		return 0, fmt.Errorf("stat %s: %w", signerPath, err)
	}
}

func loadL1ValidatorIdentities(startIndex, count int, nodeIDs map[string]string) ([]l1ValidatorIdentity, error) {
	identities := make([]l1ValidatorIdentity, 0, count)
	for i := startIndex; i < startIndex+count; i++ {
		nodeID, err := requireEnv(nodeIDs, fmt.Sprintf("L1_%d_NODE_ID", i))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", nodeIDsEnvFile, err)
		}
		signerPath := filepath.Join("staking", "l1", strconv.Itoa(i), "signer.key")
		signerKey, err := os.ReadFile(signerPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", signerPath, err)
		}
		identities = append(identities, l1ValidatorIdentity{
			Index:     i,
			NodeID:    nodeID,
			SignerKey: signerKey,
		})
	}
	return identities, nil
}

func l1ValidatorStartIndex(env map[string]string) (int, error) {
	value := strings.TrimSpace(env[l1ValidatorStartIndexEnvKey])
	if value == "" {
		if truthy(env["SYBIL_ENABLED_LOCAL"]) {
			return 6, nil
		}
		return 1, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must set %s to an integer: %w", envFile, l1ValidatorStartIndexEnvKey, err)
	}
	if parsed < 1 {
		return 0, fmt.Errorf("%s must set %s to a positive integer, got %d", envFile, l1ValidatorStartIndexEnvKey, parsed)
	}
	return parsed, nil
}

func pchainAPI(env map[string]string) (string, error) {
	if explicit := strings.TrimSpace(env["PCHAIN_RPC_URL"]); explicit != "" {
		return explicit, nil
	}
	if truthy(env["SYBIL_ENABLED_LOCAL"]) {
		firstDC1, err := firstCSVValue(env, "DC1_NODE_IPS")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("http://%s:9650", firstDC1), nil
	}
	benchmarkHostIP, err := requireEnv(env, "BENCHMARK_HOST_IP")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:9650", benchmarkHostIP), nil
}

func firstCSVValue(env map[string]string, key string) (string, error) {
	value, err := requireEnv(env, key)
	if err != nil {
		return "", err
	}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			return part, nil
		}
	}
	return "", fmt.Errorf("must set %s to at least one non-empty value", key)
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func loadEnv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	env := make(map[string]string)
	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNo+1)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, lineNo+1)
		}
		env[key] = value
	}
	return env, nil
}

func requireEnv(env map[string]string, key string) (string, error) {
	value := strings.TrimSpace(env[key])
	if value == "" {
		return "", fmt.Errorf("must set %s", key)
	}
	return value, nil
}

func requireEnvInt(env map[string]string, key string) (int, error) {
	value, err := requireEnv(env, key)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", envFile, err)
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must set %s to an integer: %w", envFile, key, err)
	}
	return parsed, nil
}

func requireFiles(paths ...string) error {
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("missing %s -- run `make` first or restore the runtime package", path)
		}
		if info.IsDir() {
			return fmt.Errorf("%s is a directory, expected a file", path)
		}
	}
	return nil
}

func writeL1Env(result l1Result) error {
	if err := os.MkdirAll(filepath.Dir(l1EnvPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(l1EnvPath), err)
	}
	data := fmt.Sprintf(
		"# Generated by create-l1. Do not edit by hand; rerun ./scripts/02_create-l1.sh to replace.\nL1_SUBNET_ID=%s\nL1_CHAIN_ID=%s\nL1_VALIDATOR_START_INDEX=%d\nL1_VALIDATOR_COUNT=%d\n",
		result.SubnetID,
		result.ChainID,
		result.ValidatorStartIndex,
		result.ValidatorCount,
	)
	if err := os.WriteFile(l1EnvPath, []byte(data), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", l1EnvPath, err)
	}
	return nil
}
