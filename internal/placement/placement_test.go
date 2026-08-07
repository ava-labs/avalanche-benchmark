package placement

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/internal/config"
	"github.com/ava-labs/avalanche-benchmark/internal/creation"
)

func testFleet() ([]config.Node, creation.Public) {
	nodes := []config.Node{
		{Number: 1, Host: "v1", Role: config.RoleValidator},
		{Number: 2, Host: "v2", Role: config.RoleValidator},
		{Number: 3, Host: "rpc", Role: config.RoleRPC},
		{Number: 4, Host: "pchain", Role: config.RolePChain},
	}
	public := creation.Public{Nodes: []creation.PublicNode{
		{Identity: "a", Node: 1, Role: config.RoleValidator},
		{Identity: "b", Node: 2, Role: config.RoleValidator},
		{Identity: "c", Node: 3, Role: config.RoleRPC},
		{Identity: "d", Node: 4, Role: config.RolePChain},
	}}
	return nodes, public
}

func TestDefaultIsTheGeneratedBijection(t *testing.T) {
	nodes, public := testFleet()
	got := Default(public)
	want := Placement{1: "a", 2: "b", 3: "c", 4: "d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default = %v, want %v", got, want)
	}
	if err := Validate(got, public, nodes); err != nil {
		t.Fatalf("default placement is invalid: %v", err)
	}
	if !reflect.DeepEqual(got.Nodes(), []int{1, 2, 3, 4}) {
		t.Fatalf("nodes = %v", got.Nodes())
	}
	if number, ok := got.NodeOf("b"); !ok || number != 2 {
		t.Fatalf("NodeOf(b) = %d %v", number, ok)
	}
	if _, ok := got.NodeOf("z"); ok {
		t.Fatal("NodeOf found an identity that is not placed")
	}
}

func TestValidate(t *testing.T) {
	nodes, public := testFleet()
	for _, testCase := range []struct {
		name  string
		value Placement
		want  string
	}{
		{"validator swap", Placement{1: "b", 2: "a", 3: "c", 4: "d"}, ""},
		{"validator and rpc swapped", Placement{1: "c", 2: "b", 3: "a", 4: "d"}, "assigns rpc identity \"c\" to validator node 1"},
		{"validator and P-chain swapped", Placement{1: "d", 2: "b", 3: "c", 4: "a"}, "assigns pchain identity \"d\" to validator node 1"},
		{"duplicate identity", Placement{1: "a", 2: "a", 3: "c", 4: "d"}, "assigned to both"},
		{"unknown identity", Placement{1: "z", 2: "b", 3: "c", 4: "d"}, "unknown identity"},
		{"missing machine", Placement{1: "a", 2: "b", 3: "c", 9: "d"}, "no identity for node 4"},
		{"incomplete", Placement{1: "a", 2: "b", 3: "c"}, "nodes.ini has 4"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := Validate(testCase.value, public, nodes)
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestSaveAndLoadRoundTripAndMissingFileFailsLoudly(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, FileName)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "read required placement") {
		t.Fatalf("missing placement error = %v", err)
	}
	want := Placement{1: "b", 2: "a", 3: "c", 4: "d"}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %v, want %v", got, want)
	}
	if Path("/root") != filepath.Join("/root", "deployment", FileName) {
		t.Fatalf("path = %s", Path("/root"))
	}
}
