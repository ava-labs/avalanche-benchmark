package keygen

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
)

func TestGenerateWritesPrivateBundleAndPublicHandover(t *testing.T) {
	nodes := []config.Node{
		{Number: 1, Role: config.RoleValidator},
		{Number: 2, Role: config.RoleValidator},
		{Number: 3, Role: config.RoleValidator},
		{Number: 4, Role: config.RoleValidator},
		{Number: 5, Role: config.RoleRPC},
		{Number: 6, Role: config.RolePChain},
	}
	output := filepath.Join(t.TempDir(), "deployment")
	result, err := Generate(output, nodes, 1)
	if err != nil {
		t.Fatal(err)
	}

	publicPath := filepath.Join(output, "public.json")
	loaded, digest, err := creation.LoadPublic(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	if digest != result.Digest {
		t.Fatalf("digest changed across handover: generated %s, loaded %s", result.Digest, digest)
	}
	if len(loaded.Nodes) != 6 || len(loaded.Managers) != 1 {
		t.Fatalf("unexpected public identity counts: %+v", loaded)
	}
	if loaded.Nodes[4].Signer != nil || loaded.Nodes[4].Weight != 0 {
		t.Fatal("RPC must have neither signer nor weight")
	}
	if _, err := os.Stat(filepath.Join(output, "identities", "e", "signer.key")); !os.IsNotExist(err) {
		t.Fatalf("RPC signer key must not exist, got %v", err)
	}
	if loaded.Nodes[5].Signer != nil || loaded.Nodes[5].Weight != 0 {
		t.Fatal("P-chain node must have neither signer nor weight")
	}
	if _, err := os.Stat(filepath.Join(output, "identities", "f", "signer.key")); !os.IsNotExist(err) {
		t.Fatalf("P-chain node signer key must not exist, got %v", err)
	}

	genesisKeyPath := filepath.Join(output, "genesis-funds.key")
	keyHex, err := os.ReadFile(genesisKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := hex.DecodeString(strings.TrimSpace(string(keyHex)))
	if err != nil {
		t.Fatal(err)
	}
	key, err := secp256k1.ToPrivateKey(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if key.EthAddress().Hex() != loaded.GenesisAddress {
		t.Fatalf("Genesis address %s does not match private key %s", loaded.GenesisAddress, key.EthAddress())
	}
	info, err := os.Stat(genesisKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("Genesis key mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := Generate(output, nodes, 1); err == nil {
		t.Fatal("key generation must reject existing output")
	}
	if loaded.FeederAddress != "" {
		t.Fatalf("feeder address must be empty without oracle nodes, got %q", loaded.FeederAddress)
	}
	if _, err := os.Stat(filepath.Join(output, "oracle-feeder.key")); !os.IsNotExist(err) {
		t.Fatalf("oracle feeder key must not exist without oracle nodes, got %v", err)
	}
}

func TestGenerateOracleIdentitiesAndFeederKey(t *testing.T) {
	nodes := []config.Node{
		{Number: 1, Role: config.RoleValidator},
		{Number: 2, Role: config.RoleValidator},
		{Number: 3, Role: config.RoleValidator},
		{Number: 4, Role: config.RoleValidator},
		{Number: 5, Role: config.RoleRPC},
		{Number: 6, Role: config.RolePChain},
		{Number: 7, Role: config.RoleArchive},
		{Number: 8, Role: config.RoleArchive},
		{Number: 9, Role: config.RoleOracleValidator},
		{Number: 10, Role: config.RoleOracleValidator},
		{Number: 11, Role: config.RoleOracleRPC},
	}
	output := filepath.Join(t.TempDir(), "deployment")
	if _, err := Generate(output, nodes, 1); err != nil {
		t.Fatal(err)
	}

	loaded, _, err := creation.LoadPublic(filepath.Join(output, "public.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Identity letters follow inventory order: i and j are the oracle
	// validators, f the P-chain node, g and h the archives, k the oracle rpc.
	oracleValidator := loaded.Nodes[8]
	if oracleValidator.Role != config.RoleOracleValidator || oracleValidator.Signer == nil || oracleValidator.Weight != creation.OracleWeight {
		t.Fatalf("unexpected oracle validator: %+v", oracleValidator)
	}
	for _, index := range []int{5, 6, 7, 10} {
		if loaded.Nodes[index].Signer != nil || loaded.Nodes[index].Weight != 0 {
			t.Fatalf("node %s must have neither signer nor weight", loaded.Nodes[index].Identity)
		}
	}
	if _, err := os.Stat(filepath.Join(output, "identities", "i", "signer.key")); err != nil {
		t.Fatalf("oracle validator BLS key must exist: %v", err)
	}

	feederKeyPath := filepath.Join(output, "oracle-feeder.key")
	keyHex, err := os.ReadFile(feederKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := hex.DecodeString(strings.TrimSpace(string(keyHex)))
	if err != nil {
		t.Fatal(err)
	}
	key, err := secp256k1.ToPrivateKey(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if key.EthAddress().Hex() != loaded.FeederAddress {
		t.Fatalf("feeder address %s does not match private key %s", loaded.FeederAddress, key.EthAddress())
	}
	info, err := os.Stat(feederKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("feeder key mode = %o, want 600", info.Mode().Perm())
	}
}
