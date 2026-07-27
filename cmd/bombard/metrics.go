package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Node metrics are scraped from each validator's /ext/metrics endpoint at the
// start and end of a bounded (-duration) run. Consensus/proposer counters are
// cumulative since node start, so only end-start deltas are meaningful; gauges
// are point-in-time so we report the end value.

// metricNames we keep from the (large, ~5700-series) /ext/metrics page.
// Anything else is dropped during parse to bound memory. Grouped by concern so
// the panel can show consensus disagreement, proposer-slot stalls, and
// subnet-evm execution side by side. NOTE: the L1 EVM metrics live under the
// avalanche_subnetevm_vm_eth_* prefix (the avalanche_evm_eth_* series are the
// C-chain), all carrying chain="<L1 blockchain hash>".
var keepMetrics = map[string]bool{
	// --- snowman consensus: success + disagreement ---
	"avalanche_snowman_blks_accepted_count":                    true, // D + avg num
	"avalanche_snowman_blks_accepted_sum":                      true, // avg: issuance->accept ns
	"avalanche_snowman_blks_rejected_count":                    true, // D: disagreements / fork loss
	"avalanche_snowman_blks_built":                             true, // D: blocks built locally
	"avalanche_snowman_blks_issued":                            true, // D
	"avalanche_snowman_polls_successful":                       true, // D: SUCCESSFUL QUERIES
	"avalanche_snowman_polls_failed":                           true, // D
	"avalanche_snowman_poll_duration_sum":                      true, // avg: ns/poll
	"avalanche_snowman_poll_duration_count":                    true,
	"avalanche_snowman_blks_polls_accepted_sum":                true, // avg: rounds to finalize
	"avalanche_snowman_blks_polls_accepted_count":              true,
	"avalanche_snowman_num_useless_push_query_bytes":           true, // D: wasted gossip
	"avalanche_snowman_num_processing_ancestor_fetches_failed": true, // D: dropped votes
	"avalanche_snowman_blks_processing":                        true, // G: backlog
	"avalanche_snowman_blocked":                                true, // G
	"avalanche_snowman_last_accepted_height":                   true, // D + G
	"avalanche_snowman_max_verified_height":                    true, // G: verify lead
	"avalanche_benchlist_benched_num":                          true, // G: benched validators
	"avalanche_requests_average_latency":                       true, // G: net RTT (no chain label)
	// --- proposerVM: slot-timing stall ---
	"avalanche_proposervm_accepted_blocks_slot_bucket": true, // H: per-slot delta
	"avalanche_proposervm_block_building_slot":         true, // G
	// --- handler: raw consensus traffic (op-labeled) ---
	"avalanche_handler_messages": true, // D per op (chits/queries/put/get/app_gossip)
	// --- subnet-evm execution ---
	"avalanche_subnetevm_vm_eth_chain_txs_accepted":            true, // D: chain-side success count
	"avalanche_subnetevm_vm_eth_chain_txs_processed":           true, // D: processed-accepted = replays
	"avalanche_subnetevm_vm_eth_chain_block_inserts_count":     true, // D: blocks
	"avalanche_subnetevm_vm_eth_chain_block_gas_used_accepted": true, // D: gas
	"avalanche_subnetevm_vm_eth_chain_execution_sum":           true, // avg (builder-only): EVM exec ns/blk
	"avalanche_subnetevm_vm_eth_chain_execution_count":         true,
	"avalanche_subnetevm_vm_eth_chain_reorg_executes":          true, // D: EVM-layer forks
	"avalanche_subnetevm_vm_eth_chain_reorg_add":               true, // D
	"avalanche_subnetevm_vm_eth_chain_reorg_drop":              true, // D
	"avalanche_subnetevm_vm_eth_chain_block_bad_count":         true, // D
	"avalanche_subnetevm_vm_eth_chain_latest_basefee":          true, // G: gas controller
	"avalanche_subnetevm_vm_eth_chain_latest_gas_target":       true, // G
	"avalanche_subnetevm_vm_eth_chain_latest_gas_capacity":     true, // G
	"avalanche_subnetevm_vm_eth_chain_latest_mindelay":         true, // G: is 5ms applied
	"avalanche_subnetevm_vm_eth_chain_latest_mindelay_excess":  true, // G
	// --- subnet-evm txpool ingest health ---
	"avalanche_subnetevm_vm_eth_txpool_pending":     true, // G: mempool depth
	"avalanche_subnetevm_vm_eth_txpool_queued":      true, // G
	"avalanche_subnetevm_vm_eth_txpool_valid":       true, // D: accepted into pool
	"avalanche_subnetevm_vm_eth_txpool_invalid":     true, // D
	"avalanche_subnetevm_vm_eth_txpool_known":       true, // D: DUPLICATE submits
	"avalanche_subnetevm_vm_eth_txpool_underpriced": true, // D
	"avalanche_subnetevm_vm_eth_txpool_overflowed":  true, // D
}

type sample struct {
	name   string
	labels map[string]string
	value  float64
}

// rpcToMetricsURL turns http://host:9652/ext/bc/<id>/rpc into
// http://host:9652/ext/metrics.
func rpcToMetricsURL(rpcURL string) string {
	i := strings.Index(rpcURL, "/ext/")
	if i < 0 {
		return strings.TrimRight(rpcURL, "/") + "/ext/metrics"
	}
	return rpcURL[:i] + "/ext/metrics"
}

// chainIDFromRPC extracts the blockchain ID from .../ext/bc/<id>/rpc; this is
// the value of the {chain="..."} label in the metrics.
func chainIDFromRPC(rpcURL string) string {
	parts := strings.Split(rpcURL, "/")
	for i, p := range parts {
		if p == "bc" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// scrapeMetrics GETs the page and returns the samples we care about.
func scrapeMetrics(ctx context.Context, metricsURL string) ([]sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metricsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var out []sample
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, val, ok := parseMetricLine(line)
		if !ok || !keepMetrics[name] {
			continue
		}
		out = append(out, sample{name: name, labels: labels, value: val})
	}
	return out, sc.Err()
}

func parseMetricLine(line string) (name string, labels map[string]string, val float64, ok bool) {
	var metricPart, valuePart string
	if brace := strings.IndexByte(line, '}'); brace >= 0 {
		metricPart = line[:brace+1]
		fields := strings.Fields(line[brace+1:])
		if len(fields) == 0 {
			return "", nil, 0, false
		}
		valuePart = fields[0]
	} else {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return "", nil, 0, false
		}
		metricPart, valuePart = fields[0], fields[1]
	}

	v, err := strconv.ParseFloat(valuePart, 64)
	if err != nil {
		return "", nil, 0, false
	}

	labels = map[string]string{}
	if lb := strings.IndexByte(metricPart, '{'); lb >= 0 {
		name = metricPart[:lb]
		body := strings.TrimSuffix(metricPart[lb+1:], "}")
		for _, kv := range splitLabels(body) {
			eq := strings.IndexByte(kv, '=')
			if eq < 0 {
				continue
			}
			labels[strings.TrimSpace(kv[:eq])] = strings.Trim(kv[eq+1:], `"`)
		}
	} else {
		name = metricPart
	}
	return name, labels, v, true
}

// splitLabels splits a prometheus label body on commas not inside quotes.
func splitLabels(body string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range body {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// findVal returns the value for name on chain, optionally matching an le bucket.
func findVal(samples []sample, name, chainID, le string) (float64, bool) {
	for _, s := range samples {
		if s.name != name || s.labels["chain"] != chainID {
			continue
		}
		if le != "" && s.labels["le"] != le {
			continue
		}
		return s.value, true
	}
	return 0, false
}

// get returns the value for name. If chainID is non-empty the series must carry
// that chain label (node-wide metrics like requests_average_latency pass "").
// extra constrains additional labels (e.g. op="chits" on handler_messages).
func get(samples []sample, name, chainID string, extra map[string]string) float64 {
	for _, s := range samples {
		if s.name != name {
			continue
		}
		if chainID != "" && s.labels["chain"] != chainID {
			continue
		}
		ok := true
		for k, v := range extra {
			if s.labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return s.value
		}
	}
	return 0
}

// nodeSnapshot pairs an endpoint with a scrape (or the error that skipped it).
type nodeSnapshot struct {
	rpcURL  string
	samples []sample
	err     error
}

// scrapeAllNodes scrapes every endpoint's /ext/metrics with a short per-node
// timeout. Unreachable nodes are recorded with their error, never fatal.
func scrapeAllNodes(rpcURLs []string) []nodeSnapshot {
	snaps := make([]nodeSnapshot, len(rpcURLs))
	for i, u := range rpcURLs {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		s, err := scrapeMetrics(ctx, rpcToMetricsURL(u))
		cancel()
		snaps[i] = nodeSnapshot{rpcURL: u, samples: s, err: err}
	}
	return snaps
}

// sampler scrapes a focused set of rate/gauge metrics every interval and prints
// one compact numeric row, so per-second dynamics (e.g. a dip's coincidence with
// a txpool_pending peg or a blks_processing backlog spike) are visible, the
// full ~45-metric panel is only meaningful as a begin/end delta and hides them.
type sampler struct {
	urls    []string
	chainID string
	prev    map[string][]sample
	prevAt  time.Time
	startAt time.Time
}

func newSampler(urls []string, chainID string) *sampler {
	return &sampler{urls: urls, chainID: chainID, prev: map[string][]sample{}, startAt: time.Now()}
}

// printSampleLegend prints the column key once, before the rows start.
func printSampleLegend(urls []string) {
	fmt.Printf("\nPER-SEC SAMPLE (%d node(s)), txAcc/s: chain txs accepted/s | blkAcc,blkRej/s: blocks accepted/rejected per s | proc: max blks_processing (consensus backlog) | slot0%%: accepted in proposer slot-0 | pendMax: max txpool_pending across nodes | pf/s: polls failed/s\n", len(urls))
	fmt.Printf("SAMPLE   t  txAcc/s blkAcc/s blkRej/s proc slot0%% pendMax pf/s\n")
}

// tick scrapes all nodes and prints a row of per-interval deltas. Replicated
// consensus counters (every validator accepts the same blocks) are taken as the
// max delta across nodes to ignore a momentarily-lagging scrape; per-node gauges
// (pending, processing) are taken as the max to surface the worst node.
func (s *sampler) tick(now time.Time) {
	cur := map[string][]sample{}
	for _, u := range s.urls {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		smp, err := scrapeMetrics(ctx, rpcToMetricsURL(u))
		cancel()
		if err == nil {
			cur[u] = smp
		}
	}
	if s.prevAt.IsZero() {
		s.prev, s.prevAt = cur, now
		return
	}
	dt := now.Sub(s.prevAt).Seconds()
	if dt <= 0 {
		dt = 1
	}
	repDelta := func(name string, extra map[string]string) float64 {
		best := 0.0
		for u, c := range cur {
			p, ok := s.prev[u]
			if !ok {
				continue
			}
			if d := get(c, name, s.chainID, extra) - get(p, name, s.chainID, extra); d > best {
				best = d
			}
		}
		return best
	}
	gMax := func(name string) float64 {
		best := 0.0
		for _, c := range cur {
			if v := get(c, name, s.chainID, nil); v > best {
				best = v
			}
		}
		return best
	}
	txAcc := repDelta("avalanche_subnetevm_vm_eth_chain_txs_accepted", nil)
	blkAcc := repDelta("avalanche_snowman_blks_accepted_count", nil)
	blkRej := repDelta("avalanche_snowman_blks_rejected_count", nil)
	pf := repDelta("avalanche_snowman_polls_failed", nil)
	s0 := repDelta("avalanche_proposervm_accepted_blocks_slot_bucket", map[string]string{"le": "0.5"})
	stt := repDelta("avalanche_proposervm_accepted_blocks_slot_bucket", map[string]string{"le": "+Inf"})
	frac := 0.0
	if stt > 0 {
		frac = 100 * s0 / stt
	}
	fmt.Printf("SAMPLE %3d %8.0f %8.0f %8.0f %4.0f %6.1f %7.0f %4.0f\n",
		int(now.Sub(s.startAt).Seconds()), txAcc/dt, blkAcc/dt, blkRej/dt,
		gMax("avalanche_snowman_blks_processing"), frac,
		gMax("avalanche_subnetevm_vm_eth_txpool_pending"), pf/dt)
	s.prev, s.prevAt = cur, now
}

// printNodeMetrics prints, per node, the full study panel: snowman consensus
// (success + disagreement), proposerVM slot-timing, handler traffic, subnet-evm
// execution, and txpool ingest. Counters are end-start deltas, summaries are
// per-observation averages over the run, gauges are point-in-time @end. elapsed
// is the run duration in seconds (for per-second rates). chainID is the L1
// blockchain hash carried by every chain-scoped series.
func printNodeMetrics(start, end []nodeSnapshot, chainID string, elapsed float64) {
	fmt.Printf("\n=== NODE METRICS (chain %s, %.0fs window; D=delta R=rate A=avg G=gauge@end) ===\n", chainID, elapsed)
	if elapsed <= 0 {
		elapsed = 1
	}
	startByURL := map[string][]sample{}
	for _, s := range start {
		if s.err == nil {
			startByURL[s.rpcURL] = s.samples
		}
	}

	for _, e := range end {
		host := e.rpcURL
		if i := strings.Index(host, "/ext/"); i >= 0 {
			host = host[:i]
		}
		if e.err != nil {
			fmt.Printf("  %s  [unreachable: %v]\n", host, e.err)
			continue
		}
		s0 := startByURL[e.rpcURL]
		if s0 == nil {
			fmt.Printf("  %s  [no start snapshot; deltas vs 0]\n", host)
		}

		// Helpers bound to this node's start/end snapshots.
		endC := func(name string, extra map[string]string) float64 { return get(e.samples, name, chainID, extra) }
		d := func(name string) float64 { return get(e.samples, name, chainID, nil) - get(s0, name, chainID, nil) }
		// avg returns Δsum/Δcount for a summary "base" (reads base_sum/base_count).
		avg := func(base string) float64 {
			ds := d(base + "_sum")
			dc := d(base + "_count")
			if dc <= 0 {
				return 0
			}
			return ds / dc
		}
		dop := func(op string) float64 {
			return get(e.samples, "avalanche_handler_messages", chainID, map[string]string{"op": op}) -
				get(s0, "avalanche_handler_messages", chainID, map[string]string{"op": op})
		}
		// slot histogram: cumulative bucket counts -> per-slot deltas.
		bd := func(le string) float64 {
			ev, _ := findVal(e.samples, "avalanche_proposervm_accepted_blocks_slot_bucket", chainID, le)
			sv, _ := findVal(s0, "avalanche_proposervm_accepted_blocks_slot_bucket", chainID, le)
			return ev - sv
		}
		ms := func(ns float64) float64 { return ns / 1e6 }
		pct := func(num, den float64) float64 {
			if den <= 0 {
				return 0
			}
			return 100 * num / den
		}

		// --- consensus success / disagreement ---
		acc, rej := d("avalanche_snowman_blks_accepted_count"), d("avalanche_snowman_blks_rejected_count")
		pollsOK, pollsFail := d("avalanche_snowman_polls_successful"), d("avalanche_snowman_polls_failed")
		// --- proposer slots ---
		b05, b15, b25, bInf := bd("0.5"), bd("1.5"), bd("2.5"), bd("+Inf")
		slot0, slot1, slot2, slot3 := b05, b15-b05, b25-b15, bInf-b25
		slotTot := slot0 + slot1 + slot2 + slot3
		// --- evm execution ---
		txAcc, txProc := d("avalanche_subnetevm_vm_eth_chain_txs_accepted"), d("avalanche_subnetevm_vm_eth_chain_txs_processed")
		blkIns := d("avalanche_subnetevm_vm_eth_chain_block_inserts_count")
		gas := d("avalanche_subnetevm_vm_eth_chain_block_gas_used_accepted")
		txPerBlk := 0.0
		if blkIns > 0 {
			txPerBlk = txAcc / blkIns
		}

		fmt.Printf("  %s\n", host)
		// CONSENSUS
		fmt.Printf("    consensus: accepted +%.0f  rejected +%.0f (reject %.2f%%)  height +%.0f  built +%.0f  issued +%.0f\n",
			acc, rej, pct(rej, acc+rej), d("avalanche_snowman_last_accepted_height"),
			d("avalanche_snowman_blks_built"), d("avalanche_snowman_blks_issued"))
		fmt.Printf("    queries:   polls ok +%.0f fail +%.0f (fail %.2f%%)  poll_dur %.2fms  rounds/accept %.2f  accept_lat %.1fms\n",
			pollsOK, pollsFail, pct(pollsFail, pollsOK+pollsFail), ms(avg("avalanche_snowman_poll_duration")),
			avg("avalanche_snowman_blks_polls_accepted"), ms(avg("avalanche_snowman_blks_accepted")))
		fmt.Printf("    disagree:  useless_query +%.0fKB  dropped_votes +%.0f  net_rtt %.2fms  benched@end %.0f\n",
			d("avalanche_snowman_num_useless_push_query_bytes")/1024, d("avalanche_snowman_num_processing_ancestor_fetches_failed"),
			ms(get(e.samples, "avalanche_requests_average_latency", "", nil)), endC("avalanche_benchlist_benched_num", nil))
		// PROPOSER SLOTS
		fmt.Printf("    proposer:  slot0=%.0f slot1=%.0f slot2=%.0f slot3+=%.0f  slot1+=%.2f%% (STALL)  building_slot@end=%.0f\n",
			slot0, slot1, slot2, slot3, pct(slot1+slot2+slot3, slotTot), endC("avalanche_proposervm_block_building_slot", nil))
		// HANDLER TRAFFIC
		fmt.Printf("    traffic:   chits +%.0f  push_q +%.0f  pull_q +%.0f  put +%.0f  get +%.0f  app_gossip +%.0f\n",
			dop("chits"), dop("push_query"), dop("pull_query"), dop("put"), dop("get"), dop("app_gossip"))
		// EVM EXECUTION
		fmt.Printf("    evm-exec:  txs_acc +%.0f (%.0f/s)  txs_proc +%.0f (replays %.0f)  blocks +%.0f  txs/blk %.0f  gas %.2fG/s  exec_lat %.2fms\n",
			txAcc, txAcc/elapsed, txProc, txProc-txAcc, blkIns, txPerBlk, gas/elapsed/1e9, ms(avg("avalanche_subnetevm_vm_eth_chain_execution")))
		fmt.Printf("    evm-fork:  reorg_exec +%.0f add +%.0f drop +%.0f  bad_blocks +%.0f  basefee@end %.0f  mindelay@end %.0f (excess %.0f)\n",
			d("avalanche_subnetevm_vm_eth_chain_reorg_executes"), d("avalanche_subnetevm_vm_eth_chain_reorg_add"),
			d("avalanche_subnetevm_vm_eth_chain_reorg_drop"), d("avalanche_subnetevm_vm_eth_chain_block_bad_count"),
			endC("avalanche_subnetevm_vm_eth_chain_latest_basefee", nil), endC("avalanche_subnetevm_vm_eth_chain_latest_mindelay", nil),
			endC("avalanche_subnetevm_vm_eth_chain_latest_mindelay_excess", nil))
		// TXPOOL INGEST
		fmt.Printf("    txpool:    pending@end %.0f  queued@end %.0f  valid +%.0f  known(dup) +%.0f  invalid +%.0f  underpriced +%.0f  overflowed +%.0f\n",
			endC("avalanche_subnetevm_vm_eth_txpool_pending", nil), endC("avalanche_subnetevm_vm_eth_txpool_queued", nil),
			d("avalanche_subnetevm_vm_eth_txpool_valid"), d("avalanche_subnetevm_vm_eth_txpool_known"),
			d("avalanche_subnetevm_vm_eth_txpool_invalid"), d("avalanche_subnetevm_vm_eth_txpool_underpriced"),
			d("avalanche_subnetevm_vm_eth_txpool_overflowed"))
	}
}
