package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	ethcommon "github.com/ava-labs/libevm/common"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

// runExporter serves the fleet's stake weights as Prometheus gauges on addr,
// one series per staking slot with instance labels matching the node scrape
// vocabulary (a1..b4). Started by run/02_monitoring.sh next to Prometheus.
//
//	fleet_desired_weight  intent, read from the state file on every scrape
//	                      (it changes whenever `fleet weight` runs)
//	fleet_actual_weight   ValidatorManager contract weight per validationID
//	                      (C-chain eth_call, never the P-chain), refreshed
//	                      every 30s in the background so a scrape never blocks
func runExporter(cfg *config, addr string) {
	subnetID, err := ids.FromString(cfg.subnetID)
	if err != nil {
		fatalf("exporter: parse SUBNET_ID: %v", err)
	}

	managerHex := os.Getenv("MANAGER_ADDRESS")

	var mu sync.Mutex
	actual := map[int]uint64{} // slot -> last known contract weight

	refresh := func() {
		if managerHex == "" {
			return // pre-manager deploy: no on-chain weights to export
		}
		intents, err := loadIntents(cfg.stateFile, cfg.topo)
		if err != nil {
			return
		}
		targets, err := stakingTargets(cfg, subnetID, intents)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cli, err := valmgr.DialReader(ctx, cchainRPCURL(), ethcommon.HexToAddress(managerHex))
		if err != nil {
			return // transient RPC hiccup: keep the last known values
		}
		for _, t := range targets {
			v, err := cli.GetValidator(ctx, t.validationID)
			if err != nil {
				continue // transient RPC hiccup: keep the last known value
			}
			mu.Lock()
			actual[t.slot] = v.Weight
			mu.Unlock()
		}
	}
	go func() {
		for {
			refresh()
			time.Sleep(30 * time.Second)
		}
	}()

	http.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		intents, err := loadIntents(cfg.stateFile, cfg.topo)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var b strings.Builder
		b.WriteString("# TYPE fleet_desired_weight gauge\n")
		for _, s := range cfg.topo.StakingSlots() {
			fmt.Fprintf(&b, "fleet_desired_weight{instance=%q} %d\n", cfg.topo.MachineName(s), intents[s].Weight)
		}
		b.WriteString("# TYPE fleet_actual_weight gauge\n")
		mu.Lock()
		for _, s := range cfg.topo.StakingSlots() {
			if v, ok := actual[s]; ok {
				fmt.Fprintf(&b, "fleet_actual_weight{instance=%q} %d\n", cfg.topo.MachineName(s), v)
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(b.String()))
	})

	fmt.Printf("exporter: serving fleet weight metrics on %s/metrics\n", addr)
	fatalf("exporter: %v", http.ListenAndServe(addr, nil))
}
