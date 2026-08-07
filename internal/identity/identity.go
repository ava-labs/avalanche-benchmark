package identity

import (
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ava-labs/avalanche-benchmark/internal/config"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/staking"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	plformsigner "github.com/ava-labs/avalanchego/vms/platformvm/signer"
)

type Identity struct {
	Name       string
	NodeNumber int
	Role       config.Role
	// Chain is the L1 the node serves, resolved from the inventory: main by
	// default, oracle for the oracle roles, empty for the P-chain node.
	Chain     string
	Directory string
	NodeID    ids.NodeID
	Proof     *plformsigner.ProofOfPossession
}

type Set struct {
	Nodes   []Identity
	Manager []Identity
}

func Generate(root string, nodes []config.Node, committeeSize int) (Set, error) {
	if err := os.Mkdir(filepath.Join(root, "identities"), 0o700); err != nil {
		return Set{}, fmt.Errorf("create node identity directory: %w", err)
	}
	if err := os.Mkdir(filepath.Join(root, "manager"), 0o700); err != nil {
		return Set{}, fmt.Errorf("create manager identity directory: %w", err)
	}

	set := Set{
		Nodes:   make([]Identity, 0, len(nodes)),
		Manager: make([]Identity, 0, committeeSize),
	}
	for i, node := range nodes {
		name := Name(i)
		dir := filepath.Join(root, "identities", name)
		withBLS := node.Role == config.RoleValidator
		generated, err := generateOne(dir, name, node.Number, node.Role, withBLS)
		if err != nil {
			return Set{}, err
		}
		generated.Chain = config.EffectiveChain(node.Role, node.Chain)
		set.Nodes = append(set.Nodes, generated)
	}
	for i := 0; i < committeeSize; i++ {
		name := Name(i)
		dir := filepath.Join(root, "manager", name)
		generated, err := generateOne(dir, name, 0, config.RoleValidator, true)
		if err != nil {
			return Set{}, err
		}
		set.Manager = append(set.Manager, generated)
	}
	return set, nil
}

// Name gives identities a namespace that cannot be confused with numbered
// machines. Placement changes during key swaps, but an identity's name does not.
func Name(index int) string {
	name := ""
	for {
		name = string(rune('a'+index%26)) + name
		index = index/26 - 1
		if index < 0 {
			return name
		}
	}
}

func Index(name string) (int, error) {
	if name == "" {
		return 0, fmt.Errorf("identity must be lowercase letters, got %q", name)
	}
	index := 0
	for _, character := range name {
		if character < 'a' || character > 'z' {
			return 0, fmt.Errorf("identity must be lowercase letters, got %q", name)
		}
		value := int(character - 'a' + 1)
		maxInt := int(^uint(0) >> 1)
		if index > (maxInt-value)/26 {
			return 0, fmt.Errorf("identity is too long, got %q", name)
		}
		index = index*26 + value
	}
	return index - 1, nil
}

func generateOne(dir, name string, nodeNumber int, role config.Role, withBLS bool) (Identity, error) {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Identity{}, fmt.Errorf("create fresh identity %s at %s: %w", name, dir, err)
	}
	certPEM, keyPEM, err := staking.NewCertAndKeyBytes()
	if err != nil {
		return Identity{}, fmt.Errorf("generate TLS identity %s: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staker.crt"), certPEM, 0o644); err != nil {
		return Identity{}, fmt.Errorf("write %s staker.crt: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staker.key"), keyPEM, 0o600); err != nil {
		return Identity{}, fmt.Errorf("write %s staker.key: %w", name, err)
	}

	nodeID, err := LoadNodeID(filepath.Join(dir, "staker.crt"))
	if err != nil {
		return Identity{}, fmt.Errorf("load generated TLS identity %s: %w", name, err)
	}
	generated := Identity{
		Name:       name,
		NodeNumber: nodeNumber,
		Role:       role,
		Directory:  dir,
		NodeID:     nodeID,
	}
	if !withBLS {
		return generated, nil
	}

	secretKey, err := localsigner.New()
	if err != nil {
		return Identity{}, fmt.Errorf("generate BLS identity %s: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "signer.key"), secretKey.ToBytes(), 0o600); err != nil {
		return Identity{}, fmt.Errorf("write %s signer.key: %w", name, err)
	}
	generated.Proof, err = plformsigner.NewProofOfPossession(secretKey)
	if err != nil {
		return Identity{}, fmt.Errorf("build proof of possession for %s: %w", name, err)
	}
	return generated, nil
}

func LoadNodeID(path string) (ids.NodeID, error) {
	certPEM, err := os.ReadFile(path)
	if err != nil {
		return ids.EmptyNodeID, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return ids.EmptyNodeID, fmt.Errorf("%s has no PEM block", path)
	}
	cert, err := staking.ParseCertificate(block.Bytes)
	if err != nil {
		return ids.EmptyNodeID, fmt.Errorf("parse %s: %w", path, err)
	}
	return ids.NodeIDFromCert(cert), nil
}
