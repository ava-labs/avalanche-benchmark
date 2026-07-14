package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/vms/platformvm"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/netcfg"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/vset"
)

// fetchWeights reads every staking slot's CURRENT on-chain weight from the
// P-chain (the registered set cmd/l1 manages; strictly read-only here) and
// maps it back to pool slots via the committed key -> NodeID manifest. This
// is the single batch of on-chain reads a status/exporter refresh makes. Any
// failure returns nil and the reason; callers degrade to a weightless
// display, never crash.
func fetchWeights(cfg *config) (map[int]uint64, error) {
	subnetID, err := ids.FromString(cfg.subnetID)
	if err != nil {
		return nil, fmt.Errorf("parse SUBNET_ID: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	vs, err := vset.Fetch(ctx, platformvm.NewClient(netcfg.Get().API), subnetID, 1)
	if err != nil {
		return nil, err
	}
	byNodeID := make(map[string]uint64, len(vs))
	for _, v := range vs {
		byNodeID[v.NodeID.String()] = v.Weight
	}
	out := map[int]uint64{}
	for _, s := range cfg.topo.StakingSlots() {
		id := cfg.nodeIDForKey(cfg.topo.KeyOf(s))
		w, ok := byNodeID[id]
		if !ok {
			return nil, fmt.Errorf("%s (%s) is not a registered validator of subnet %s", cfg.topo.MachineName(s), id, cfg.subnetID)
		}
		out[s] = w
	}
	return out, nil
}
