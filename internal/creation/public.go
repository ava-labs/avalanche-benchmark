package creation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

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
	// FeederAddress is the EVM address of the price feed key. It signs the
	// direct price submissions to the main chain's price aggregator and, when
	// the inventory declares an oracle L1, that chain's feed transactions and
	// the main chain's Warp deliveries.
	FeederAddress string          `json:"feederAddress"`
	Nodes         []PublicNode    `json:"nodes"`
	Managers      []PublicManager `json:"managers"`
}

type PublicNode struct {
	Identity string      `json:"identity"`
	Node     int         `json:"node"`
	Role     config.Role `json:"role"`
	// Chain is the L1 the node serves. Empty means derived: main for the
	// plain roles, oracle for the oracle roles. keygen writes it only for
	// chains beyond those defaults, so old files load unchanged.
	Chain  string `json:"chain,omitempty"`
	NodeID string `json:"nodeID"`
	Weight uint64 `json:"weight,omitempty"`
	// ExplicitWeight records that Weight came from a weight= tag in
	// nodes.ini. Without it the weight follows the default ladder, and
	// validation recomputes and enforces the ladder value. keygen writes it
	// only for explicit weights, so old files load unchanged.
	ExplicitWeight bool                              `json:"explicitWeight,omitempty"`
	Signer         *platformsigner.ProofOfPossession `json:"signer,omitempty"`
}

// ChainName resolves the chain this node serves, applying the role default
// when the field is empty.
func (n PublicNode) ChainName() string {
	return config.EffectiveChain(n.Role, n.Chain)
}

type PublicManager struct {
	Identity string                            `json:"identity"`
	NodeID   string                            `json:"nodeID"`
	Weight   uint64                            `json:"weight"`
	Signer   *platformsigner.ProofOfPossession `json:"signer"`
}

func NewPublic(generated identity.Set, nodes []config.Node, genesisAddress ethcommon.Address, feederAddress ethcommon.Address) Public {
	public := Public{
		GenesisAddress: genesisAddress.Hex(),
		FeederAddress:  feederAddress.Hex(),
		Nodes:          make([]PublicNode, 0, len(generated.Nodes)),
		Managers:       make([]PublicManager, 0, len(generated.Manager)),
	}
	// Explicit weight= tags from the inventory, by node number. Config load
	// already enforced the all-or-none rule per chain.
	explicit := make(map[int]uint64, len(nodes))
	for _, node := range nodes {
		if node.Weight > 0 {
			explicit[node.Number] = node.Weight
		}
	}
	// The default ladder: the first HighValidatorCount validators OF EACH
	// CHAIN, by node number, carry the heavy weight; the rest are spares.
	// The oracle chain keeps its flat weights. An explicit weight= tag
	// replaces the ladder value for its node.
	validatorIndex := make(map[string]int, 2)
	for _, generated := range generated.Nodes {
		node := PublicNode{
			Identity: generated.Name,
			Node:     generated.NodeNumber,
			Role:     generated.Role,
			NodeID:   generated.NodeID.String(),
			Signer:   generated.Proof,
		}
		chainName := config.EffectiveChain(generated.Role, generated.Chain)
		if chainName != config.EffectiveChain(generated.Role, "") {
			node.Chain = chainName
		}
		switch generated.Role {
		case config.RoleValidator:
			node.Weight = LowWeight
			if validatorIndex[chainName] < HighValidatorCount {
				node.Weight = HighWeight
			}
			validatorIndex[chainName]++
		case config.RoleOracleValidator:
			node.Weight = OracleWeight
		}
		if weight, tagged := explicit[generated.NodeNumber]; tagged {
			node.Weight = weight
			node.ExplicitWeight = true
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
	if !ethcommon.IsHexAddress(p.FeederAddress) {
		return fmt.Errorf("feederAddress must be an EVM address, got %q", p.FeederAddress)
	}
	if len(p.Nodes) == 0 {
		return fmt.Errorf("nodes must not be empty")
	}
	if err := p.validateWeightConsistency(); err != nil {
		return err
	}

	seenNodeIDs := make(map[ids.NodeID]struct{}, len(p.Nodes)+len(p.Managers))
	validatorIndex := make(map[string]int, 2)
	pchainCount := 0
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

		if node.Chain != "" {
			if !config.ValidChainName(node.Chain) {
				return fmt.Errorf("node %s chain must be 1 to 20 characters of lowercase letters, digits, and hyphens, got %q", node.Identity, node.Chain)
			}
			switch node.Role {
			case config.RolePChain:
				return fmt.Errorf("node %s is the P-chain node and serves every chain; chain must be empty", node.Identity)
			case config.RoleOracleValidator, config.RoleOracleRPC:
				if node.Chain != config.OracleChain {
					return fmt.Errorf("node %s role %s always serves chain %q, got %q", node.Identity, node.Role, config.OracleChain, node.Chain)
				}
			default:
				if node.Chain == config.OracleChain || node.Chain == "management" {
					return fmt.Errorf("node %s chain name %q is reserved", node.Identity, node.Chain)
				}
			}
		}

		switch node.Role {
		case config.RoleValidator:
			// An explicit weight only needs to be positive; a default weight
			// must recompute to the ladder value.
			if node.ExplicitWeight {
				if node.Weight == 0 {
					return fmt.Errorf("validator %s explicit weight must be at least 1", node.Identity)
				}
			} else {
				expectedWeight := uint64(LowWeight)
				if validatorIndex[node.ChainName()] < HighValidatorCount {
					expectedWeight = HighWeight
				}
				if node.Weight != expectedWeight {
					return fmt.Errorf("validator %s weight must be %d, got %d", node.Identity, expectedWeight, node.Weight)
				}
			}
			if node.Signer == nil {
				return fmt.Errorf("validator %s signer is required", node.Identity)
			}
			if err := node.Signer.Verify(); err != nil {
				return fmt.Errorf("validator %s signer: %w", node.Identity, err)
			}
			validatorIndex[node.ChainName()]++
		case config.RoleOracleValidator:
			if node.ExplicitWeight {
				if node.Weight == 0 {
					return fmt.Errorf("oracle validator %s explicit weight must be at least 1", node.Identity)
				}
			} else if node.Weight != OracleWeight {
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
			if node.ExplicitWeight {
				return fmt.Errorf("%s %s must not carry an explicit weight", node.Role, node.Identity)
			}
			if node.Signer != nil {
				return fmt.Errorf("%s %s signer must not be provided", node.Role, node.Identity)
			}
			switch node.Role {
			case config.RoleOracleRPC:
				oracleRPCCount++
			case config.RolePChain:
				pchainCount++
			}
		default:
			return fmt.Errorf("node %s role must be validator, rpc, pchain, archive, oracle-validator, or oracle-rpc, got %q", node.Identity, node.Role)
		}
	}
	// Shape opinions (validator count, RPC count, archive redundancy) are
	// warnings at inventory load; only the structural rules stay hard here,
	// mirroring config.LoadNodes.
	if pchainCount != 1 {
		return fmt.Errorf("exactly 1 P-chain node is required, got %d", pchainCount)
	}
	for _, chain := range p.Chains() {
		if chain == config.OracleChain {
			continue
		}
		if validatorIndex[chain] == 0 {
			return fmt.Errorf("chain %q has no validator; a chain needs at least 1", chain)
		}
	}
	if oracleValidatorCount > 0 && oracleRPCCount < 1 {
		return fmt.Errorf("oracle validators require at least 1 oracle-rpc")
	}
	if oracleRPCCount > 0 && oracleValidatorCount == 0 {
		return fmt.Errorf("oracle-rpc nodes require at least 1 oracle-validator")
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

// validateWeightConsistency mirrors the nodes.ini rule on the manifest: a
// chain's validators carry explicit weights all together or not at all.
// keygen never writes a mix, so this catches hand edits.
func (p Public) validateWeightConsistency() error {
	withWeight := make(map[string][]string)
	withoutWeight := make(map[string][]string)
	for _, node := range p.Nodes {
		if node.Role != config.RoleValidator && node.Role != config.RoleOracleValidator {
			continue
		}
		chain := node.ChainName()
		if node.ExplicitWeight {
			withWeight[chain] = append(withWeight[chain], node.Identity)
		} else {
			withoutWeight[chain] = append(withoutWeight[chain], node.Identity)
		}
	}
	for _, chain := range p.Chains() {
		explicit := withWeight[chain]
		defaulted := withoutWeight[chain]
		if len(explicit) == 0 || len(defaulted) == 0 {
			continue
		}
		return fmt.Errorf(
			"chain %q mixes explicit and default validator weights: explicit on %s, default on %s",
			chain, strings.Join(explicit, ", "), strings.Join(defaulted, ", "),
		)
	}
	return nil
}

// HasOracle reports whether the inventory declares an oracle L1.
func (p Public) HasOracle() bool {
	for _, node := range p.Nodes {
		if node.Role == config.RoleOracleValidator {
			return true
		}
	}
	return false
}

// Chains returns the unique chain names the manifest declares, main first
// when present and the rest in name order.
func (p Public) Chains() []string {
	seen := make(map[string]struct{}, 4)
	var names []string
	for _, node := range p.Nodes {
		name := node.ChainName()
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	config.SortChains(names)
	return names
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
			Chain:      node.ChainName(),
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
