package main

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ava-labs/avalanche-benchmark/internal/config"
	"github.com/ava-labs/avalanche-benchmark/internal/creation"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/joho/godotenv"
)

// Endpoint discovery and issuer key loading read the same deployment inventory
// the rest of the kit reads: nodes.ini for the node list, deployment/network.env
// for the chain id and the expected genesis address, and
// deployment/genesis-funds.key for the key that owns the L1 genesis funds.
const (
	nodesFile      = "nodes.ini"
	networkEnvFile = "deployment/network.env"
	genesisKeyFile = "deployment/genesis-funds.key"
)

// httpPortsByNode reproduces the positional HTTP port rule of portsByNode in
// internal/fleet/deploy.go: nodes are walked in inventory order and the Nth node
// on a given host gets HTTP port 9650+2N (staking is the odd port above it).
// Several logical nodes can share one host, so the port depends on the node's
// position among the nodes of ITS host, not on the node number.
//
// internal/fleet keeps portsByNode package private and cmd must not import that
// package, so this is a deliberate sibling copy. internal/fleet/deploy.go is the
// authority; the two must stay in agreement or bombard will send to dead ports.
func httpPortsByNode(nodes []config.Node) map[int]int {
	occurrences := make(map[string]int)
	result := make(map[int]int, len(nodes))
	for _, node := range nodes {
		result[node.Number] = 9650 + 2*occurrences[node.Host]
		occurrences[node.Host]++
	}
	return result
}

// discoverRPCEndpoints builds the ingress URL of every rpc node of the named
// chain. Transaction ingress goes to rpc nodes ONLY, never a validator and
// never the P-chain node, and it fans out across all of them.
func discoverRPCEndpoints(root, chain string) ([]string, error) {
	nodes, err := config.LoadNodes(filepath.Join(root, nodesFile))
	if err != nil {
		return nil, err
	}
	state, err := readNetworkEnv(root)
	if err != nil {
		return nil, err
	}
	_, chainIDKey, _ := creation.StateChainKeys(chain)
	chainID := strings.TrimSpace(state[chainIDKey])
	if chainID == "" {
		return nil, fmt.Errorf("%s: required field %s is not provided; run l1 create first", networkEnvFile, chainIDKey)
	}

	ports := httpPortsByNode(nodes)
	var urls []string
	for _, node := range nodes {
		if node.Role != config.RoleRPC || config.EffectiveChain(node.Role, node.Chain) != chain {
			continue
		}
		urls = append(urls, fmt.Sprintf("http://%s:%d/ext/bc/%s/rpc", node.Host, ports[node.Number], chainID))
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("%s: no rpc nodes on chain %q; transaction ingress requires at least one", nodesFile, chain)
	}
	return urls, nil
}

func readNetworkEnv(root string) (map[string]string, error) {
	state, err := godotenv.Read(filepath.Join(root, networkEnvFile))
	if err != nil {
		return nil, fmt.Errorf("read required deployment state %s: %w", networkEnvFile, err)
	}
	return state, nil
}

// loadIssuerKey loads the key that owns the L1 genesis funds and checks that its
// EVM address is the GENESIS_EVM_ADDRESS recorded at creation time. The check is
// fatal on purpose: sending from an unfunded account is not an error the nodes
// report loudly, it just produces zero throughput for the whole run.
func loadIssuerKey(root string) (*ecdsa.PrivateKey, common.Address, error) {
	path := filepath.Join(root, genesisKeyFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("read required genesis funding key %s: %w", genesisKeyFile, err)
	}
	material := strings.TrimSpace(string(raw))
	if keyBytes, err := hex.DecodeString(material); err != nil || len(keyBytes) != 32 {
		return nil, common.Address{}, fmt.Errorf("%s must be exactly 64 hex characters", genesisKeyFile)
	}
	key, err := crypto.HexToECDSA(material)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("%s is not a valid secp256k1 key: %w", genesisKeyFile, err)
	}
	address := crypto.PubkeyToAddress(key.PublicKey)

	state, err := readNetworkEnv(root)
	if err != nil {
		return nil, common.Address{}, err
	}
	expected := strings.TrimSpace(state["GENESIS_EVM_ADDRESS"])
	if expected == "" {
		return nil, common.Address{}, fmt.Errorf("%s: required field GENESIS_EVM_ADDRESS is not provided; run l1 create first", networkEnvFile)
	}
	if !strings.EqualFold(expected, address.Hex()) {
		return nil, common.Address{}, fmt.Errorf(
			"%s derives address %s but %s records GENESIS_EVM_ADDRESS=%s; the issuer account would be unfunded and the run would mine nothing",
			genesisKeyFile, address.Hex(), networkEnvFile, expected,
		)
	}
	return key, address, nil
}
