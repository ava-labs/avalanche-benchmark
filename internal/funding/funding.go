package funding

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/utils/formatting/address"
	"github.com/ava-labs/avalanchego/vms/platformvm"
)

type Addresses struct {
	PChain string
	EVM    string
}

type Info struct {
	Addresses Addresses
	Balance   uint64
}

func Generate() (string, error) {
	key, err := secp256k1.NewPrivateKey()
	if err != nil {
		return "", fmt.Errorf("generate funding private key: %w", err)
	}
	return hex.EncodeToString(key.Bytes()), nil
}

func GenerateIntoEnvironment(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read required configuration %s: %w", path, err)
	}
	lines := strings.Split(string(contents), "\n")
	found := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, "FUNDING_PRIVATE_KEY=") {
			continue
		}
		if found {
			return fmt.Errorf("%s: duplicate FUNDING_PRIVATE_KEY field", path)
		}
		found = true
		_, value, _ := strings.Cut(trimmed, "=")
		if strings.TrimSpace(value) != "" {
			return fmt.Errorf("%s: FUNDING_PRIVATE_KEY already exists", path)
		}
		privateKey, err := Generate()
		if err != nil {
			return err
		}
		lines[i] = "FUNDING_PRIVATE_KEY=" + privateKey
	}
	if !found {
		return fmt.Errorf("%s: required field FUNDING_PRIVATE_KEY is not present", path)
	}

	temp, err := os.CreateTemp(filepath.Dir(path), ".env-keygen-*")
	if err != nil {
		return fmt.Errorf("create temporary configuration beside %s: %w", path, err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("protect temporary configuration %s: %w", tempPath, err)
	}
	if _, err := temp.WriteString(strings.Join(lines, "\n")); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary configuration %s: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary configuration %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish generated funding key to %s: %w", path, err)
	}
	return nil
}

func Inspect(ctx context.Context, environment config.Environment) (Info, error) {
	key, err := ParsePrivateKey(environment.FundingPrivateKey)
	if err != nil {
		return Info{}, err
	}
	addresses, err := DeriveAddresses(environment.Network, key)
	if err != nil {
		return Info{}, err
	}
	balance, err := platformvm.NewClient(environment.PChainAPI).GetBalance(
		ctx,
		[]ids.ShortID{key.Address()},
	)
	if err != nil {
		return Info{}, fmt.Errorf("read P-chain balance from %s: %w", environment.PChainAPI, err)
	}
	return Info{
		Addresses: addresses,
		Balance:   uint64(balance.Unlocked),
	}, nil
}

func ParsePrivateKey(raw string) (*secp256k1.PrivateKey, error) {
	keyBytes, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode FUNDING_PRIVATE_KEY: %w", err)
	}
	key, err := secp256k1.ToPrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("load FUNDING_PRIVATE_KEY: %w", err)
	}
	return key, nil
}

func DeriveAddresses(network string, key *secp256k1.PrivateKey) (Addresses, error) {
	if network != "fuji" && network != "mainnet" {
		return Addresses{}, fmt.Errorf("network must be fuji or mainnet, got %q", network)
	}
	networkID, err := constants.NetworkID(network)
	if err != nil {
		return Addresses{}, fmt.Errorf("resolve network %q: %w", network, err)
	}
	pChain, err := address.Format("P", constants.GetHRP(networkID), key.Address().Bytes())
	if err != nil {
		return Addresses{}, fmt.Errorf("format P-chain address: %w", err)
	}
	return Addresses{
		PChain: pChain,
		EVM:    key.EthAddress().Hex(),
	}, nil
}
