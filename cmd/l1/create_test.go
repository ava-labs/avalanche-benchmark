package main

import (
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
)

func TestPlanValidators(t *testing.T) {
	nodes, err := topo.Parse(`
a1     host=1.0.0.1 role=validator dc=A
a2     host=1.0.0.2 role=validator dc=A
rpc_a1 host=1.0.0.5 role=rpc       dc=A
b1     host=2.0.0.1 role=validator dc=B
b2     host=2.0.0.2 role=validator dc=B
`)
	if err != nil {
		t.Fatal(err)
	}
	plans := planValidators(nodes)
	if len(plans) != 4 {
		t.Fatalf("planned %d validators, want 4 (rpc nodes are never registered)", len(plans))
	}
	wantNames := []string{"a1", "a2", "b1", "b2"}
	for i, p := range plans {
		if p.name != wantNames[i] {
			t.Errorf("plan[%d] = %q, want %q", i, p.name, wantNames[i])
		}
	}
}
