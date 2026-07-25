package creation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	"github.com/ava-labs/avalanchego/ids"
	platformsigner "github.com/ava-labs/avalanchego/vms/platformvm/signer"
	ethcommon "github.com/ava-labs/libevm/common"
)

const (
	HighValidatorCount = 3
	HighWeight         = 100000
	LowWeight          = 1000
	ManagerWeight      = 1000
	OracleWeight       = 1000
)

type Public struct {
	GenesisAddress string `json:"genesisAddress"`
	// FeederAddress is the EVM address of the oracle feed key. It is present
	// exactly when the inventory declares an oracle L1.
	FeederAddress string          `json:"feederAddress,omitempty"`
	Nodes         []PublicNode    `json:"nodes"`
	Managers      []PublicManager `json:"managers"`
}

type PublicNode struct {
	Identity string                            `json:"identity"`
	Node     int                               `json:"node"`
	Role     config.Role                       `json:"role"`
	NodeID   string                            `json:"nodeID"`
	Weight   uint64                            `json:"weight,omitempty"`
	Signer   *platformsigner.ProofOfPossession `json:"signer,omitempty"`
}

type PublicManager struct {
	Identity string                            `json:"identity"`
	NodeID   string                            `json:"nodeID"`
	Weight   uint64                            `json:"weight"`
	Signer   *platformsigner.ProofOfPossession `json:"signer"`
}

func NewPublic(generated identity.Set, genesisAddress ethcommon.Address, feederAddress *ethcommon.Address) Public {
	public := Public{
		GenesisAddress: genesisAddress.Hex(),
		Nodes:          make([]PublicNode, 0, len(generated.Nodes)),
		Managers:       make([]PublicManager, 0, len(generated.Manager)),
	}
	if feederAddress != nil {
		public.FeederAddress = feederAddress.Hex()
	}
	validatorIndex := 0
	for _, generated := range generated.Nodes {
		node := PublicNode{
			Identity: generated.Name,
			Node:     generated.NodeNumber,
			Role:     generated.Role,
			NodeID:   generated.NodeID.String(),
			Signer:   generated.Proof,
		}
		switch generated.Role {
		case config.RoleValidator:
			node.Weight = LowWeight
			if validatorIndex < HighValidatorCount {
				node.Weight = HighWeight
			}
			validatorIndex++
		case config.RoleOracleValidator:
			node.Weight = OracleWeight
		}
		public.Nodes = append(public.Nodes, node)
	}
	for _, generated := range generated.Manager {
		public.Managers = append(public.Managers, PublicManager{
			Identity: generated.Name,
			NodeID:   generated.NodeID.String(),
			Weight:   ManagerWeight,
			Signer:   generated.Proof,
		})
	}
	return public
}

func SavePublic(path string, public Public) (string, error) {
	contents, err := json.MarshalIndent(public, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode public chain inputs: %w", err)
	}
	contents = append(contents, '\n')
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		return "", fmt.Errorf("write public chain inputs %s: %w", path, err)
	}
	return digest(contents), nil
}

func LoadPublic(path string) (Public, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return Public{}, "", fmt.Errorf("read required public chain inputs %s: %w", path, err)
	}
	defer file.Close()

	hash := sha256.New()
	decoder := json.NewDecoder(io.TeeReader(file, hash))
	decoder.DisallowUnknownFields()
	public := Public{}
	if err := decoder.Decode(&public); err != nil {
		return Public{}, "", fmt.Errorf("decode public chain inputs %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Public{}, "", fmt.Errorf("decode public chain inputs %s: multiple JSON values", path)
		}
		return Public{}, "", fmt.Errorf("decode public chain inputs %s: %w", path, err)
	}
	if err := public.Validate(); err != nil {
		return Public{}, "", fmt.Errorf("%s: %w", path, err)
	}
	return public, hex.EncodeToString(hash.Sum(nil)), nil
}

func (p Public) Validate() error {
	if !ethcommon.IsHexAddress(p.GenesisAddress) {
		return fmt.Errorf("genesisAddress must be an EVM address, got %q", p.GenesisAddress)
	}
	if len(p.Nodes) == 0 {
		return fmt.Errorf("nodes must not be empty")
	}

	seenNodeIDs := make(map[ids.NodeID]struct{}, len(p.Nodes)+len(p.Managers))
	validatorCount := 0
	rpcCount := 0
	pchainCount := 0
	archiveCount := 0
	oracleValidatorCount := 0
	oracleRPCCount := 0
	previousNode := 0
	for i, node := range p.Nodes {
		expectedIdentity := identity.Name(i)
		if node.Identity != expectedIdentity {
			return fmt.Errorf("nodes[%d].identity must be %q, got %q", i, expectedIdentity, node.Identity)
		}
		if node.Node <= previousNode {
			return fmt.Errorf("nodes must have strictly increasing positive node numbers")
		}
		previousNode = node.Node
		nodeID, err := ids.NodeIDFromString(node.NodeID)
		if err != nil {
			return fmt.Errorf("node %s nodeID: %w", node.Identity, err)
		}
		if _, exists := seenNodeIDs[nodeID]; exists {
			return fmt.Errorf("nodeID %s is duplicated", nodeID)
		}
		seenNodeIDs[nodeID] = struct{}{}

		switch node.Role {
		case config.RoleValidator:
			expectedWeight := uint64(LowWeight)
			if validatorCount < HighValidatorCount {
				expectedWeight = HighWeight
			}
			if node.Weight != expectedWeight {
				return fmt.Errorf("validator %s weight must be %d, got %d", node.Identity, expectedWeight, node.Weight)
			}
			if node.Signer == nil {
				return fmt.Errorf("validator %s signer is required", node.Identity)
			}
			if err := node.Signer.Verify(); err != nil {
				return fmt.Errorf("validator %s signer: %w", node.Identity, err)
			}
			validatorCount++
		case config.RoleOracleValidator:
			if node.Weight != OracleWeight {
				return fmt.Errorf("oracle validator %s weight must be %d, got %d", node.Identity, OracleWeight, node.Weight)
			}
			if node.Signer == nil {
				return fmt.Errorf("oracle validator %s signer is required", node.Identity)
			}
			if err := node.Signer.Verify(); err != nil {
				return fmt.Errorf("oracle validator %s signer: %w", node.Identity, err)
			}
			oracleValidatorCount++
		case config.RoleRPC, config.RoleArchive, config.RoleOracleRPC, config.RolePChain:
			if node.Weight != 0 {
				return fmt.Errorf("%s %s weight must be 0, got %d", node.Role, node.Identity, node.Weight)
			}
			if node.Signer != nil {
				return fmt.Errorf("%s %s signer must not be provided", node.Role, node.Identity)
			}
			switch node.Role {
			case config.RoleRPC:
				rpcCount++
			case config.RoleArchive:
				archiveCount++
			case config.RoleOracleRPC:
				oracleRPCCount++
			case config.RolePChain:
				pchainCount++
			}
		default:
			return fmt.Errorf("node %s role must be validator, rpc, pchain, archive, oracle-validator, or oracle-rpc, got %q", node.Identity, node.Role)
		}
	}
	if validatorCount < 4 {
		return fmt.Errorf("at least 4 validators are required, got %d", validatorCount)
	}
	if rpcCount < 1 {
		return fmt.Errorf("at least 1 rpc is required")
	}
	if pchainCount != 1 {
		return fmt.Errorf("exactly 1 P-chain node is required, got %d", pchainCount)
	}
	if archiveCount == 1 {
		return fmt.Errorf("0 or at least 2 archive nodes are required, got 1")
	}
	if oracleValidatorCount > 0 && oracleRPCCount < 1 {
		return fmt.Errorf("oracle validators require at least 1 oracle-rpc")
	}
	if oracleRPCCount > 0 && oracleValidatorCount == 0 {
		return fmt.Errorf("oracle-rpc nodes require at least 1 oracle-validator")
	}
	if oracleValidatorCount > 0 && !ethcommon.IsHexAddress(p.FeederAddress) {
		return fmt.Errorf("feederAddress must be an EVM address when oracle validators exist, got %q", p.FeederAddress)
	}
	if oracleValidatorCount == 0 && p.FeederAddress != "" {
		return fmt.Errorf("feederAddress must be empty without oracle validators, got %q", p.FeederAddress)
	}
	if len(p.Managers) != 1 && len(p.Managers) != 4 {
		return fmt.Errorf("manager count must be 1 or 4, got %d", len(p.Managers))
	}
	for i, manager := range p.Managers {
		expectedIdentity := identity.Name(i)
		if manager.Identity != expectedIdentity {
			return fmt.Errorf("managers[%d].identity must be %q, got %q", i, expectedIdentity, manager.Identity)
		}
		nodeID, err := ids.NodeIDFromString(manager.NodeID)
		if err != nil {
			return fmt.Errorf("manager %s nodeID: %w", manager.Identity, err)
		}
		if _, exists := seenNodeIDs[nodeID]; exists {
			return fmt.Errorf("nodeID %s is duplicated", nodeID)
		}
		seenNodeIDs[nodeID] = struct{}{}
		if manager.Weight != ManagerWeight {
			return fmt.Errorf("manager %s weight must be %d, got %d", manager.Identity, ManagerWeight, manager.Weight)
		}
		if manager.Signer == nil {
			return fmt.Errorf("manager %s signer is required", manager.Identity)
		}
		if err := manager.Signer.Verify(); err != nil {
			return fmt.Errorf("manager %s signer: %w", manager.Identity, err)
		}
	}
	return nil
}

func (p Public) IdentitySet() (identity.Set, error) {
	set := identity.Set{
		Nodes:   make([]identity.Identity, 0, len(p.Nodes)),
		Manager: make([]identity.Identity, 0, len(p.Managers)),
	}
	for _, node := range p.Nodes {
		nodeID, err := ids.NodeIDFromString(node.NodeID)
		if err != nil {
			return identity.Set{}, err
		}
		set.Nodes = append(set.Nodes, identity.Identity{
			Name:       node.Identity,
			NodeNumber: node.Node,
			Role:       node.Role,
			NodeID:     nodeID,
			Proof:      node.Signer,
		})
	}
	for _, manager := range p.Managers {
		nodeID, err := ids.NodeIDFromString(manager.NodeID)
		if err != nil {
			return identity.Set{}, err
		}
		set.Manager = append(set.Manager, identity.Identity{
			Name:   manager.Identity,
			Role:   config.RoleValidator,
			NodeID: nodeID,
			Proof:  manager.Signer,
		})
	}
	return set, nil
}

func digest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}
