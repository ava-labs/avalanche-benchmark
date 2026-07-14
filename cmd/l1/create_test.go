package main

import (
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
)

func TestPlanValidators(t *testing.T) {
	nodes, err := topo.Parse(`
a1     host=1.0.0.1 role=validator dc=A weight=100000
a2     host=1.0.0.2 role=validator dc=A weight=100000
rpc_a1 host=1.0.0.5 role=rpc       dc=A
b1     host=2.0.0.1 role=validator dc=B weight=1000
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
	wantWeights := []uint64{100000, 100000, 1000, 1} // weight= tag, default 1
	for i, p := range plans {
		if p.name != wantNames[i] || p.weight != wantWeights[i] {
			t.Errorf("plan[%d] = %q w=%d, want %q w=%d", i, p.name, p.weight, wantNames[i], wantWeights[i])
		}
	}
}
