package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/libevm/crypto"
)

// Two logical nodes share host-a and three share host-b, so the positional rule
// of internal/fleet/deploy.go portsByNode is the only thing that gets the ports
// right. Node numbers deliberately do not match the port order.
func TestHTTPPortsByNode(t *testing.T) {
	nodes := []config.Node{
		{Number: 1, Host: "host-a", Role: config.RoleValidator},
		{Number: 2, Host: "host-b", Role: config.RoleValidator},
		{Number: 3, Host: "host-a", Role: config.RoleValidator},
		{Number: 4, Host: "host-b", Role: config.RoleRPC},
		{Number: 5, Host: "host-b", Role: config.RoleRPC},
		{Number: 6, Host: "host-c", Role: config.RolePChain},
	}
	want := map[int]int{1: 9650, 2: 9650, 3: 9652, 4: 9652, 5: 9654, 6: 9650}
	got := httpPortsByNode(nodes)
	if len(got) != len(want) {
		t.Fatalf("httpPortsByNode returned %d entries, want %d", len(got), len(want))
	}
	for number, port := range want {
		if got[number] != port {
			t.Errorf("node %d got HTTP port %d, want %d", number, got[number], port)
		}
	}
}

const testChainID = "2W1H4RVBBhVXTRE9BgSgQyudEJQZDkbSAJhBhbSpUyGWDQrfaN"

func writeInventory(t *testing.T, nodesINI, networkEnv string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, nodesFile), []byte(nodesINI), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "deployment"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, networkEnvFile), []byte(networkEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// Node 10 and node 11 share 10.0.0.5, so node 11 must land on 9652. Validators
// and the P-chain node must never appear: ingress goes to role=rpc only.
const testNodesINI = `1  host=10.0.0.1 role=validator dc=A
2  host=10.0.0.2 role=validator dc=A
3  host=10.0.0.3 role=validator dc=B
4  host=10.0.0.4 role=validator dc=B
10 host=10.0.0.5 role=rpc       dc=A
11 host=10.0.0.5 role=rpc       dc=A
12 host=10.0.0.6 role=rpc       dc=B
13 host=10.0.0.7 role=pchain
`

func TestDiscoverRPCEndpoints(t *testing.T) {
	root := writeInventory(t, testNodesINI, "NETWORK=fuji\nCHAIN_ID="+testChainID+"\n")

	got, err := discoverRPCEndpoints(root)
	if err != nil {
		t.Fatalf("discoverRPCEndpoints: %v", err)
	}
	want := []string{
		"http://10.0.0.5:9650/ext/bc/" + testChainID + "/rpc",
		"http://10.0.0.5:9652/ext/bc/" + testChainID + "/rpc",
		"http://10.0.0.6:9650/ext/bc/" + testChainID + "/rpc",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("discoverRPCEndpoints:\ngot  %v\nwant %v", got, want)
	}
}

func TestDiscoverRPCEndpointsRequiresChainID(t *testing.T) {
	root := writeInventory(t, testNodesINI, "NETWORK=fuji\n")
	if _, err := discoverRPCEndpoints(root); err == nil {
		t.Fatal("expected an error when CHAIN_ID is absent")
	}
}

func writeGenesisKey(t *testing.T, root, keyHex string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, genesisKeyFile), []byte(keyHex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadIssuerKey(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyHex := hex.EncodeToString(crypto.FromECDSA(key))
	address := crypto.PubkeyToAddress(key.PublicKey)

	root := writeInventory(t, testNodesINI, "NETWORK=fuji\nGENESIS_EVM_ADDRESS="+address.Hex()+"\n")
	writeGenesisKey(t, root, keyHex)

	loaded, loadedAddress, err := loadIssuerKey(root)
	if err != nil {
		t.Fatalf("loadIssuerKey: %v", err)
	}
	if loadedAddress != address {
		t.Fatalf("loadIssuerKey address = %s, want %s", loadedAddress, address)
	}
	if hex.EncodeToString(crypto.FromECDSA(loaded)) != keyHex {
		t.Fatal("loadIssuerKey returned a different key")
	}
}

// An address mismatch means the issuer account holds no genesis funds, which
// produces a silent zero-throughput run. It must be fatal.
func TestLoadIssuerKeyRejectsAddressMismatch(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	root := writeInventory(t, testNodesINI, "NETWORK=fuji\nGENESIS_EVM_ADDRESS=0x0000000000000000000000000000000000000001\n")
	writeGenesisKey(t, root, hex.EncodeToString(crypto.FromECDSA(key)))

	if _, _, err := loadIssuerKey(root); err == nil {
		t.Fatal("expected an error when the derived address does not match GENESIS_EVM_ADDRESS")
	}
}

func TestLoadIssuerKeyRejectsMalformedKey(t *testing.T) {
	root := writeInventory(t, testNodesINI, "NETWORK=fuji\nGENESIS_EVM_ADDRESS=0x0000000000000000000000000000000000000001\n")
	writeGenesisKey(t, root, "not-hex")

	if _, _, err := loadIssuerKey(root); err == nil {
		t.Fatal("expected an error for a malformed key file")
	}
}
