package main

import (
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
)

func mustParse(t *testing.T, src string) []topo.Node {
	t.Helper()
	nodes, err := topo.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	return nodes
}

func TestBuildInstancesOneNodePerHost(t *testing.T) {
	// The deployed prod shape: one node per box, everything on 9652/9653 with
	// a per-name data root.
	nodes := mustParse(t, `
a1     host=10.0.0.1 role=validator dc=A
rpc_a1 host=10.0.0.2 role=rpc       dc=A
`)
	insts := buildInstances(nodes)
	for i, in := range insts {
		if in.shared {
			t.Errorf("node %d: shared=true, want false (distinct hosts)", i)
		}
		if in.httpPort != 9652 || in.stakingPort != 9653 {
			t.Errorf("node %d ports = %d/%d, want 9652/9653", i, in.httpPort, in.stakingPort)
		}
	}
	a1 := insts[0]
	if a1.dataDir != "data/a1" || a1.activeDir != "data/a1/staking/active" || a1.startScript != "start-a1.sh" {
		t.Errorf("a1 layout = %+v", a1)
	}
	if insts[1].dataDir != "data/rpc_a1" {
		t.Errorf("rpc_a1 dataDir = %q", insts[1].dataDir)
	}
}

func TestBuildInstancesCoHosted(t *testing.T) {
	// Two nodes on one host: positional ports, per-name dirs, both shared.
	nodes := mustParse(t, `
a1 host=10.0.0.1 role=validator
x9 host=10.0.0.1 role=validator
b1 host=10.0.0.2 role=validator
`)
	insts := buildInstances(nodes)
	if !insts[0].shared || !insts[1].shared || insts[2].shared {
		t.Errorf("shared flags = %v %v %v, want true true false", insts[0].shared, insts[1].shared, insts[2].shared)
	}
	if insts[0].httpPort != 9652 || insts[1].httpPort != 9654 || insts[1].stakingPort != 9655 {
		t.Errorf("ports = %d, %d/%d", insts[0].httpPort, insts[1].httpPort, insts[1].stakingPort)
	}
	if insts[0].dataDir != "data/a1" || insts[1].dataDir != "data/x9" {
		t.Errorf("dirs = %q, %q", insts[0].dataDir, insts[1].dataDir)
	}
}

func TestMakeInstanceProcPatAvoidsSelfMatch(t *testing.T) {
	// The bracketed first digit makes the regex match the avalanchego argv
	// ("--http-port=9654") but NOT the literal pgrep argv ("--http-port=[9]654").
	nodes := mustParse(t, "a1 host=h role=validator\nx9 host=h role=validator")
	if got := makeInstance(nodes[1]).procPat; got != "--http-port=[9]654" {
		t.Errorf("procPat = %q, want --http-port=[9]654", got)
	}
}
