package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// runExporter serves the fleet's on-chain stake weights as Prometheus gauges
// on addr, one series per staking slot with instance labels matching the node
// scrape vocabulary (a1..b4). Started by run/02_monitoring.sh next to
// Prometheus.
//
//	fleet_actual_weight  the P-chain weight per registered validator
//	                     (read via the same set cmd/l1 manages), refreshed
//	                     every 30s in the background so a scrape never blocks
func runExporter(cfg *config, addr string) {
	var mu sync.Mutex
	actual := map[int]uint64{} // slot -> last known on-chain weight

	refresh := func() {
		w, err := fetchWeights(cfg)
		if err != nil {
			return // transient RPC hiccup: keep the last known values
		}
		mu.Lock()
		actual = w
		mu.Unlock()
	}
	go func() {
		for {
			refresh()
			time.Sleep(30 * time.Second)
		}
	}()

	http.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
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
