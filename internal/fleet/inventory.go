package fleet

import (
	"fmt"
	"path/filepath"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/placement"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/joho/godotenv"
)

// inventory is the read-only view every non-deploying fleet command needs:
// the machines, the generated identities, and the current placement. Unlike
// prepare() it renders nothing and requires no build artifacts, so it also
// works on a fleet where only the P-chain node has been initialized.
type inventory struct {
	environment config.FleetEnvironment
	nodes       []config.Node
	public      creation.Public
	placement   placement.Placement
	ports       map[int][2]int
	pchain      config.Node

	identityByLetter map[string]creation.PublicNode

	// created is false when deployment/network.env is absent or incomplete,
	// which is normal before l1 create.
	created         bool
	chainID         ids.ID
	subnetID        ids.ID
	managerSubnetID ids.ID
	oracleChainID   ids.ID
}

// l1ChainFor returns the chain a node serves: oracle roles live on the oracle
// L1, every other L1 role on the main one.
func (i inventory) l1ChainFor(role config.Role) ids.ID {
	if role == config.RoleOracleValidator || role == config.RoleOracleRPC {
		return i.oracleChainID
	}
	return i.chainID
}

func (d *Deployer) inventory() (inventory, error) {
	environment, nodes, err := d.loadFleet()
	if err != nil {
		return inventory{}, err
	}
	public, _, err := creation.LoadPublic(filepath.Join(d.root, "deployment", "public.json"))
	if err != nil {
		return inventory{}, err
	}
	current, err := placement.Load(placement.Path(d.root))
	if err != nil {
		return inventory{}, err
	}
	if err := placement.Validate(current, public, nodes); err != nil {
		return inventory{}, err
	}

	result := inventory{
		environment:      environment,
		nodes:            nodes,
		public:           public,
		placement:        current,
		ports:            portsByNode(nodes),
		identityByLetter: make(map[string]creation.PublicNode, len(public.Nodes)),
	}
	for _, node := range public.Nodes {
		result.identityByLetter[node.Identity] = node
	}
	for _, node := range nodes {
		if node.Role == config.RolePChain {
			result.pchain = node
			break
		}
	}

	state, err := godotenv.Read(filepath.Join(d.root, "deployment", "network.env"))
	if err != nil {
		return result, nil
	}
	chainID, chainErr := requiredID(state, "CHAIN_ID")
	subnetID, subnetErr := requiredID(state, "SUBNET_ID")
	managerSubnetID, managerErr := requiredID(state, "MANAGER_SUBNET_ID")
	if chainErr != nil || subnetErr != nil || managerErr != nil {
		return result, nil
	}
	if state["NETWORK"] != environment.Network {
		return inventory{}, fmt.Errorf(
			"deployment/network.env NETWORK=%q does not match .env NETWORK=%q",
			state["NETWORK"], environment.Network,
		)
	}
	result.created = true
	result.chainID = chainID
	result.subnetID = subnetID
	result.managerSubnetID = managerSubnetID
	// Optional: only present when the deployment includes the oracle L1.
	if oracleChainID, err := requiredID(state, "ORACLE_CHAIN_ID"); err == nil {
		result.oracleChainID = oracleChainID
	}
	return result, nil
}

// assigned returns the identity currently placed on a machine.
func (i inventory) assigned(node config.Node) (creation.PublicNode, error) {
	letter, exists := i.placement[node.Number]
	if !exists {
		return creation.PublicNode{}, fmt.Errorf("placement has no identity for node %d", node.Number)
	}
	generated, known := i.identityByLetter[letter]
	if !known {
		return creation.PublicNode{}, fmt.Errorf("placement assigns unknown identity %q to node %d", letter, node.Number)
	}
	return generated, nil
}

// target builds the addressing view of a machine: which host, which identity,
// which HTTP port. It carries no render directory because nothing is rendered.
func (i inventory) target(node config.Node) (nodeDeployment, error) {
	generated, err := i.assigned(node)
	if err != nil {
		return nodeDeployment{}, err
	}
	return nodeDeployment{
		node:     node,
		identity: generated,
		httpPort: i.ports[node.Number][0],
	}, nil
}

// l1Nodes returns every validator and RPC machine, never the P-chain machine.
func (i inventory) l1Nodes() []config.Node {
	result := make([]config.Node, 0, len(i.nodes))
	for _, node := range i.nodes {
		if node.Role == config.RolePChain {
			continue
		}
		result = append(result, node)
	}
	return result
}
