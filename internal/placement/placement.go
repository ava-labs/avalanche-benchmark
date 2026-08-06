// Package placement holds the control-side machine-to-identity assignment.
//
// Machines are numbers, identities are lowercase letters. Validator identities
// are movable between validator machines; RPC and P-chain identities are
// stable. deployment/placement.json is the only truth for which identity a
// machine currently runs.
package placement

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
)

// Placement maps an inventory node number to an identity letter.
type Placement map[int]string

// FileName is the placement file relative to deployment/.
const FileName = "placement.json"

// Default derives the initial bijection straight from the generated manifest.
func Default(public creation.Public) Placement {
	result := make(Placement, len(public.Nodes))
	for _, node := range public.Nodes {
		result[node.Node] = node.Identity
	}
	return result
}

// Path returns deployment/placement.json under the given repository root.
func Path(root string) string {
	return filepath.Join(root, "deployment", FileName)
}

// Load reads placement.json. A missing file is an error: placement is written
// by keygen and every later command depends on it.
func Load(path string) (Placement, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read required placement %s: %w", path, err)
	}
	result := Placement{}
	if err := json.Unmarshal(contents, &result); err != nil {
		return nil, fmt.Errorf("decode placement %s: %w", path, err)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("placement %s is empty", path)
	}
	return result, nil
}

// Save writes placement.json atomically: placement is control-side truth and a
// torn write would strand the fleet with an unreadable assignment.
func Save(path string, value Placement) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode placement: %w", err)
	}
	contents = append(contents, '\n')
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, contents, 0o644); err != nil {
		return fmt.Errorf("write placement %s: %w", temporary, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("publish placement %s: %w", path, err)
	}
	return nil
}

// Validate checks the placement is a complete bijection over the inventory and
// that every assignment keeps roles intact. Only validator identities move.
func Validate(value Placement, public creation.Public, nodes []config.Node) error {
	roleByIdentity := make(map[string]config.Role, len(public.Nodes))
	homeByIdentity := make(map[string]int, len(public.Nodes))
	chainByIdentity := make(map[string]string, len(public.Nodes))
	for _, node := range public.Nodes {
		roleByIdentity[node.Identity] = node.Role
		homeByIdentity[node.Identity] = node.Node
		chainByIdentity[node.Identity] = node.ChainName()
	}
	if len(value) != len(nodes) {
		return fmt.Errorf("placement assigns %d machines but nodes.ini has %d", len(value), len(nodes))
	}

	seen := make(map[string]int, len(value))
	for _, node := range nodes {
		assigned, exists := value[node.Number]
		if !exists {
			return fmt.Errorf("placement has no identity for node %d", node.Number)
		}
		role, known := roleByIdentity[assigned]
		if !known {
			return fmt.Errorf("placement assigns unknown identity %q to node %d", assigned, node.Number)
		}
		if role != node.Role {
			return fmt.Errorf(
				"placement assigns %s identity %q to %s node %d",
				role, assigned, node.Role, node.Number,
			)
		}
		// An identity carries its chain's stake and key material, so it never
		// crosses onto a machine of another chain.
		if machineChain := config.EffectiveChain(node.Role, node.Chain); chainByIdentity[assigned] != machineChain {
			return fmt.Errorf(
				"placement assigns chain %q identity %q to node %d, which serves chain %q",
				chainByIdentity[assigned], assigned, node.Number, machineChain,
			)
		}
		if role != config.RoleValidator && homeByIdentity[assigned] != node.Number {
			return fmt.Errorf(
				"%s identity %q is not movable; it belongs to node %d, not node %d",
				role, assigned, homeByIdentity[assigned], node.Number,
			)
		}
		if previous, duplicate := seen[assigned]; duplicate {
			return fmt.Errorf("identity %q is assigned to both node %d and node %d", assigned, previous, node.Number)
		}
		seen[assigned] = node.Number
	}
	return nil
}

// Nodes returns the assigned machine numbers in ascending order.
func (p Placement) Nodes() []int {
	result := make([]int, 0, len(p))
	for number := range p {
		result = append(result, number)
	}
	sort.Ints(result)
	return result
}

// NodeOf returns the machine currently running the given identity.
func (p Placement) NodeOf(identity string) (int, bool) {
	for number, assigned := range p {
		if assigned == identity {
			return number, true
		}
	}
	return 0, false
}
