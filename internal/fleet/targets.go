package fleet

import (
	"encoding/json"
	"fmt"

	"github.com/ava-labs/avalanche-benchmark/internal/config"
	"github.com/ava-labs/avalanchego/ids"
)

// A scrape target file tells Prometheus which nodes to read and how to label
// them. The labels come from the inventory, so they never drift from it the
// way hand-written scrape configs do. Two label names matter:
//
//   - "l1" is the chain NAME from nodes.ini. It exists only on this target,
//     so it filters target-level series such as up{}.
//   - "l1_chain_id" is the blockchain ID. Chain-level avalanchego series
//     carry their own chain="<blockchain-id>" label; this label lets a
//     dashboard map a chain name to that value.
//
// The label "chain" is never used here: a target label named "chain" would
// shadow the metric label of the same name and break every existing query.
type scrapeTarget struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// renderTargets builds the Prometheus file_sd document for every node. The
// chainIDs map may be nil on a fleet that has not run l1 create yet; the
// targets then carry no l1_chain_id label and gain it on the next render.
func renderTargets(nodes []config.Node, ports map[int][2]int, chainIDs map[string]ids.ID) ([]byte, error) {
	targets := make([]scrapeTarget, 0, len(nodes))
	for _, node := range nodes {
		port, known := ports[node.Number]
		if !known {
			return nil, fmt.Errorf("node %d has no port assignment", node.Number)
		}
		labels := map[string]string{
			"node": fmt.Sprintf("%d", node.Number),
			"role": string(node.Role),
		}
		if node.DC != "" {
			labels["dc"] = node.DC
		}
		if chain := chainOf(node); chain != "" {
			labels["l1"] = chain
			if id, created := chainIDs[chain]; created {
				labels["l1_chain_id"] = id.String()
			}
		}
		targets = append(targets, scrapeTarget{
			Targets: []string{fmt.Sprintf("%s:%d", node.Host, port[0])},
			Labels:  labels,
		})
	}
	document, err := json.MarshalIndent(targets, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render scrape targets: %w", err)
	}
	return append(document, '\n'), nil
}

// Targets prints the scrape target file for the current inventory. Pipe it
// to monitoring/targets.json and re-run it after every inventory change:
//
//	./bin/fleet targets > monitoring/targets.json
func (d *Deployer) Targets() error {
	inv, err := d.inventory()
	if err != nil {
		return err
	}
	document, err := renderTargets(inv.nodes, inv.ports, inv.chainIDs)
	if err != nil {
		return err
	}
	_, err = d.out.Write(document)
	return err
}
