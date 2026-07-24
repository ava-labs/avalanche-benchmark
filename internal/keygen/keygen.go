package keygen

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	ethcommon "github.com/ava-labs/libevm/common"
)

type Result struct {
	Public creation.Public
	Digest string
}

func Generate(outputDirectory string, nodes []config.Node, managerCount int) (Result, error) {
	if err := creation.ValidateManagerCommittee(managerCount); err != nil {
		return Result{}, err
	}
	if _, err := os.Stat(outputDirectory); err == nil {
		return Result{}, fmt.Errorf("key generation output %s already exists; remove it explicitly before generating fresh keys", outputDirectory)
	} else if !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("inspect key generation output %s: %w", outputDirectory, err)
	}
	if err := os.Mkdir(outputDirectory, 0o700); err != nil {
		return Result{}, fmt.Errorf("create key generation output %s: %w", outputDirectory, err)
	}

	generated, err := identity.Generate(outputDirectory, nodes, managerCount)
	if err != nil {
		return Result{}, err
	}
	genesisKey, err := secp256k1.NewPrivateKey()
	if err != nil {
		return Result{}, fmt.Errorf("generate Genesis funds key: %w", err)
	}
	genesisKeyPath := filepath.Join(outputDirectory, "genesis-funds.key")
	if err := os.WriteFile(genesisKeyPath, []byte(hex.EncodeToString(genesisKey.Bytes())+"\n"), 0o600); err != nil {
		return Result{}, fmt.Errorf("write Genesis funds key %s: %w", genesisKeyPath, err)
	}

	// The feeder key exists exactly when the inventory declares an oracle L1.
	// It signs the oracle chain's feed transactions and the main chain's
	// delivery transactions, so both chains fund it at Genesis.
	var feederAddress *ethcommon.Address
	for _, node := range nodes {
		if node.Role != config.RoleOracleValidator {
			continue
		}
		feederKey, err := secp256k1.NewPrivateKey()
		if err != nil {
			return Result{}, fmt.Errorf("generate oracle feeder key: %w", err)
		}
		feederKeyPath := filepath.Join(outputDirectory, "oracle-feeder.key")
		if err := os.WriteFile(feederKeyPath, []byte(hex.EncodeToString(feederKey.Bytes())+"\n"), 0o600); err != nil {
			return Result{}, fmt.Errorf("write oracle feeder key %s: %w", feederKeyPath, err)
		}
		address := feederKey.EthAddress()
		feederAddress = &address
		break
	}

	public := creation.NewPublic(generated, genesisKey.EthAddress(), feederAddress)
	publicPath := filepath.Join(outputDirectory, "public.json")
	digest, err := creation.SavePublic(publicPath, public)
	if err != nil {
		return Result{}, err
	}
	return Result{Public: public, Digest: digest}, nil
}
