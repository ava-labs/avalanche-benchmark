package fleet

import (
	"encoding/json"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/internal/config"
	"github.com/ava-labs/avalanchego/ids"
)

// The targets file is the single source of monitoring labels, so it must
// carry the chain name and the blockchain ID for every L1 node, keep the
// P-chain node unlabeled by chain, and never emit a label named "chain",
// which would shadow the metric label of the same name.
func TestRenderTargetsLabelsNodesByChain(t *testing.T) {
	mainID := ids.GenerateTestID()
	tradingID := ids.GenerateTestID()
	nodes := []config.Node{
		{Number: 1, Host: "10.0.0.11", Role: config.RoleValidator, DC: "A", Chain: "main"},
		{Number: 2, Host: "10.0.0.11", Role: config.RoleValidator, DC: "A", Chain: "trading"},
		{Number: 13, Host: "10.2.0.10", Role: config.RolePChain},
	}
	document, err := renderTargets(nodes, portsByNode(nodes), map[string]ids.ID{
		"main":    mainID,
		"trading": tradingID,
	})
	if err != nil {
		t.Fatal(err)
	}

	var targets []scrapeTarget
	if err := json.Unmarshal(document, &targets); err != nil {
		t.Fatalf("targets file is not valid JSON: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("got %d targets, want 3", len(targets))
	}

	first := targets[0]
	if first.Targets[0] != "10.0.0.11:9650" {
		t.Fatalf("node 1 target = %q, want positional port 9650", first.Targets[0])
	}
	if first.Labels["l1"] != "main" || first.Labels["l1_chain_id"] != mainID.String() {
		t.Fatalf("node 1 labels = %v, want l1=main with its chain ID", first.Labels)
	}
	if first.Labels["dc"] != "A" || first.Labels["role"] != "validator" || first.Labels["node"] != "1" {
		t.Fatalf("node 1 labels = %v, want node, role and dc from the inventory", first.Labels)
	}

	second := targets[1]
	if second.Targets[0] != "10.0.0.11:9652" {
		t.Fatalf("second node on one host = %q, want positional port 9652", second.Targets[0])
	}
	if second.Labels["l1"] != "trading" || second.Labels["l1_chain_id"] != tradingID.String() {
		t.Fatalf("trading node labels = %v, want its own chain ID", second.Labels)
	}

	pchain := targets[2]
	if _, has := pchain.Labels["l1"]; has {
		t.Fatalf("pchain labels = %v, want no l1 label: it serves every chain", pchain.Labels)
	}
	if _, has := pchain.Labels["dc"]; has {
		t.Fatalf("pchain labels = %v, want no empty dc label", pchain.Labels)
	}

	for _, target := range targets {
		if _, has := target.Labels["chain"]; has {
			t.Fatalf("target %v carries a chain label, which shadows the metric label", target)
		}
	}
}

// Before l1 create there are no chain IDs. The targets must still render so
// monitoring can watch a fleet that is only provisioned, not created.
func TestRenderTargetsWithoutChainIDs(t *testing.T) {
	nodes := []config.Node{
		{Number: 1, Host: "10.0.0.11", Role: config.RoleValidator, Chain: "main"},
	}
	document, err := renderTargets(nodes, portsByNode(nodes), nil)
	if err != nil {
		t.Fatal(err)
	}
	var targets []scrapeTarget
	if err := json.Unmarshal(document, &targets); err != nil {
		t.Fatal(err)
	}
	if targets[0].Labels["l1"] != "main" {
		t.Fatalf("labels = %v, want the chain name even before creation", targets[0].Labels)
	}
	if _, has := targets[0].Labels["l1_chain_id"]; has {
		t.Fatalf("labels = %v, want no chain ID before creation", targets[0].Labels)
	}
}
