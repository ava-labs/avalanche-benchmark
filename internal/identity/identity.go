package identity

import (
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/staking"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	plformsigner "github.com/ava-labs/avalanchego/vms/platformvm/signer"
)

type Identity struct {
	Name       string
	NodeNumber int
	Role       config.Role
	Directory  string
	NodeID     ids.NodeID
	Proof      *plformsigner.ProofOfPossession
}

type Set struct {
	Nodes   []Identity
	Manager []Identity
}

func Generate(root string, nodes []config.Node, committeeSize int) (Set, error) {
	if err := os.Mkdir(filepath.Join(root, "nodes"), 0o700); err != nil {
		return Set{}, fmt.Errorf("create node identity directory: %w", err)
	}
	if err := os.Mkdir(filepath.Join(root, "manager"), 0o700); err != nil {
		return Set{}, fmt.Errorf("create manager identity directory: %w", err)
	}

	set := Set{
		Nodes:   make([]Identity, 0, len(nodes)),
		Manager: make([]Identity, 0, committeeSize),
	}
	for _, node := range nodes {
		dir := filepath.Join(root, "nodes", strconv.Itoa(node.Number))
		withBLS := node.Role == config.RoleValidator
		generated, err := generateOne(dir, fmt.Sprintf("node-%d", node.Number), node.Number, node.Role, withBLS)
		if err != nil {
			return Set{}, err
		}
		set.Nodes = append(set.Nodes, generated)
	}
	for i := 1; i <= committeeSize; i++ {
		dir := filepath.Join(root, "manager", strconv.Itoa(i))
		generated, err := generateOne(dir, fmt.Sprintf("manager-%d", i), 0, config.RoleValidator, true)
		if err != nil {
			return Set{}, err
		}
		set.Manager = append(set.Manager, generated)
	}
	return set, nil
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
