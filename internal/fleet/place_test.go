package fleet

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/placement"
)

// placementTestInventory mirrors the fleet used by writeFleetInputs: validators
// 1 to 4 with identities a to d, rpc 5 with e, P-chain 6 with f.
func placementTestInventory(t *testing.T) inventory {
	t.Helper()
	nodes := []config.Node{
		{Number: 1, Host: "v1", Role: config.RoleValidator},
		{Number: 2, Host: "v2", Role: config.RoleValidator},
		{Number: 3, Host: "v3", Role: config.RoleValidator},
		{Number: 4, Host: "v4", Role: config.RoleValidator},
		{Number: 5, Host: "rpc", Role: config.RoleRPC},
		{Number: 6, Host: "pchain", Role: config.RolePChain},
	}
	public := creation.Public{Nodes: []creation.PublicNode{
		{Identity: "a", Node: 1, Role: config.RoleValidator, NodeID: "NodeID-A"},
		{Identity: "b", Node: 2, Role: config.RoleValidator, NodeID: "NodeID-B"},
		{Identity: "c", Node: 3, Role: config.RoleValidator, NodeID: "NodeID-C"},
		{Identity: "d", Node: 4, Role: config.RoleValidator, NodeID: "NodeID-D"},
		{Identity: "e", Node: 5, Role: config.RoleRPC, NodeID: "NodeID-E"},
		{Identity: "f", Node: 6, Role: config.RolePChain, NodeID: "NodeID-F"},
	}}
	result := inventory{
		nodes:            nodes,
		public:           public,
		placement:        placement.Default(public),
		identityByLetter: make(map[string]creation.PublicNode, len(public.Nodes)),
	}
	for _, node := range public.Nodes {
		result.identityByLetter[node.Identity] = node
	}
	return result
}

func TestPlaceSwapsAndKeepsTheBijection(t *testing.T) {
	inv := placementTestInventory(t)
	next, moves, err := planPlace(inv, "a", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := placement.Placement{1: "c", 2: "b", 3: "a", 4: "d", 5: "e", 6: "f"}
	if !reflect.DeepEqual(next, want) {
		t.Fatalf("placement = %v, want %v", next, want)
	}
	if err := placement.Validate(next, inv.public, inv.nodes); err != nil {
		t.Fatalf("swap broke the bijection: %v", err)
	}
	joined := strings.Join(moves, "\n")
	if !strings.Contains(joined, "identity a: node 1 -> node 3") ||
		!strings.Contains(joined, "identity c: node 3 -> node 1") {
		t.Fatalf("moves = %q", joined)
	}
	if inv.placement[1] != "a" {
		t.Fatalf("planPlace mutated the current placement: %v", inv.placement)
	}
}

func TestPlaceOntoItsOwnMachineChangesNothing(t *testing.T) {
	inv := placementTestInventory(t)
	next, moves, err := planPlace(inv, "b", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(next, inv.placement) {
		t.Fatalf("placement = %v, want unchanged %v", next, inv.placement)
	}
	if len(moves) != 1 || !strings.Contains(moves[0], "already on node 2") {
		t.Fatalf("moves = %v", moves)
	}
}

func TestPlaceRejectsNonValidatorMachinesAndIdentities(t *testing.T) {
	inv := placementTestInventory(t)
	for _, testCase := range []struct {
		name     string
		identity string
		node     int
		want     string
	}{
		{"validator onto rpc machine", "a", 5, "validator machines only"},
		{"validator onto P-chain machine", "a", 6, "validator machines only"},
		{"rpc identity moves", "e", 1, "never movable"},
		{"P-chain identity moves", "f", 1, "never movable"},
		{"rpc identity onto its own machine", "e", 5, "never movable"},
		{"unknown letter", "z", 1, "unknown identity"},
		{"not a letter", "A", 1, "lowercase letters"},
		{"unknown node", "a", 99, "unknown node"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := planPlace(inv, testCase.identity, testCase.node)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestApplyPlacementRestartsOnlyDriftedOrInterruptedNodes(t *testing.T) {
	inv := placementTestInventory(t)
	inv.placement = placement.Placement{1: "c", 2: "b", 3: "a", 4: "d", 5: "e", 6: "f"}
	l1 := inv.l1Nodes()
	probes := map[int]placementProbe{
		// drifted: still running its old identity after a place
		1: {active: true, enabled: true, nodeID: "NodeID-A"},
		// consistent
		2: {active: true, enabled: true, nodeID: "NodeID-B"},
		// drifted the other way
		3: {active: true, enabled: true, nodeID: "NodeID-C"},
		// deliberately down: fleet stop disables the unit
		4: {active: false, enabled: false},
		// interrupted apply-placement: stopped but still enabled
		5: {active: false, enabled: true},
	}
	restart, notes, err := planApply(inv, l1, probes)
	if err != nil {
		t.Fatal(err)
	}
	var numbers []int
	for _, node := range restart {
		numbers = append(numbers, node.Number)
	}
	if !reflect.DeepEqual(numbers, []int{1, 3, 5}) {
		t.Fatalf("restart set = %v, want [1 3 5]", numbers)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "node 4 is deliberately down") {
		t.Fatalf("notes = %q", joined)
	}
}

func TestApplyPlacementIsANoOpWhenEveryNodeIsConsistent(t *testing.T) {
	inv := placementTestInventory(t)
	l1 := inv.l1Nodes()
	probes := map[int]placementProbe{
		1: {active: true, enabled: true, nodeID: "NodeID-A"},
		2: {active: true, enabled: true, nodeID: "NodeID-B"},
		3: {active: true, enabled: true, nodeID: "NodeID-C"},
		4: {active: true, enabled: true, nodeID: "NodeID-D"},
		5: {active: true, enabled: true, nodeID: "NodeID-E"},
	}
	restart, _, err := planApply(inv, l1, probes)
	if err != nil {
		t.Fatal(err)
	}
	if len(restart) != 0 {
		t.Fatalf("consistent fleet restarts %v", restart)
	}
}

func TestApplyPlacementFailsWhenAnActiveNodeCannotBeIdentified(t *testing.T) {
	inv := placementTestInventory(t)
	probes := map[int]placementProbe{1: {active: true, enabled: true}}
	_, _, err := planApply(inv, inv.nodes[:1], probes)
	if err == nil || !strings.Contains(err.Error(), "no runtime NodeID") {
		t.Fatalf("error = %v", err)
	}

	if _, _, err := planApply(inv, inv.nodes[:1], nil); err == nil || !strings.Contains(err.Error(), "not probed") {
		t.Fatalf("unprobed error = %v", err)
	}
}
