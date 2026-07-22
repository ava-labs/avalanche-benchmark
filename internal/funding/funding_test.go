package funding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateCreatesFreshRawKeys(t *testing.T) {
	first, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 || first == second {
		t.Fatalf("expected two fresh raw private keys")
	}
}

func TestGenerateIntoEnvironmentWritesOnceAndProtectsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := "NETWORK=fuji\nFUNDING_PRIVATE_KEY=\nMANAGER_COMMITTEE=1\n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := GenerateIntoEnvironment(path); err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(generated), "\n") {
		if strings.HasPrefix(line, "FUNDING_PRIVATE_KEY=") && len(strings.TrimPrefix(line, "FUNDING_PRIVATE_KEY=")) != 64 {
			t.Fatalf("expected generated raw key, got line length %d", len(line))
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %o", info.Mode().Perm())
	}
	if err := GenerateIntoEnvironment(path); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected existing key error, got %v", err)
	}
}

func TestGenerateIntoEnvironmentRequiresField(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("NETWORK=fuji\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := GenerateIntoEnvironment(path); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("expected missing field error, got %v", err)
	}
}

func TestDeriveAddressesUsesExplicitNetwork(t *testing.T) {
	key, err := ParsePrivateKey(strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	fuji, err := DeriveAddresses("fuji", key)
	if err != nil {
		t.Fatal(err)
	}
	mainnet, err := DeriveAddresses("mainnet", key)
	if err != nil {
		t.Fatal(err)
	}
	if fuji.PChain == mainnet.PChain || fuji.EVM != mainnet.EVM {
		t.Fatalf("unexpected network address derivation: fuji=%+v mainnet=%+v", fuji, mainnet)
	}
	if _, err := DeriveAddresses("testnet", key); err == nil {
		t.Fatal("unsupported network must fail")
	}
}
