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

// metricNames we keep from the (large) /ext/metrics page. Anything else is
// dropped during parse to bound memory.
var keepMetrics = map[string]bool{
	"avalanche_snowman_blks_accepted_count":            true, // counter (delta)
	"avalanche_snowman_blks_rejected_count":            true, // counter (delta)
	"avalanche_snowman_polls_successful":               true, // counter (delta)
	"avalanche_snowman_polls_failed":                   true, // counter (delta)
	"avalanche_snowman_blks_processing":                true, // gauge (end)
	"avalanche_proposervm_accepted_blocks_slot_bucket": true, // histogram (delta per le)
	"avalanche_proposervm_block_building_slot":         true, // gauge (end)
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

// printNodeMetrics prints per-node deltas (counters) and end values (gauges).
func printNodeMetrics(start, end []nodeSnapshot, chainID string) {
	fmt.Printf("\n=== NODE METRICS (chain %s, delta over run) ===\n", chainID)
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
			fmt.Printf("  %s  [no start snapshot; showing end values]\n", host)
		}

		delta := func(name string) float64 {
			endV, _ := findVal(e.samples, name, chainID, "")
			startV, _ := findVal(s0, name, chainID, "")
			return endV - startV
		}
		// slot histogram: cumulative bucket counts -> per-slot deltas.
		bd := func(le string) float64 {
			endV, _ := findVal(e.samples, "avalanche_proposervm_accepted_blocks_slot_bucket", chainID, le)
			startV, _ := findVal(s0, "avalanche_proposervm_accepted_blocks_slot_bucket", chainID, le)
			return endV - startV
		}
		b05, b15, b25, bInf := bd("0.5"), bd("1.5"), bd("2.5"), bd("+Inf")
		slot0, slot1, slot2, slot3 := b05, b15-b05, b25-b15, bInf-b25

		acc, rej := delta("avalanche_snowman_blks_accepted_count"), delta("avalanche_snowman_blks_rejected_count")
		rejPct := 0.0
		if acc+rej > 0 {
			rejPct = 100 * rej / (acc + rej)
		}
		proc, _ := findVal(e.samples, "avalanche_snowman_blks_processing", chainID, "")
		buildSlot, _ := findVal(e.samples, "avalanche_proposervm_block_building_slot", chainID, "")

		fmt.Printf("  %s\n", host)
		fmt.Printf("    blocks:   accepted +%.0f  rejected +%.0f  (reject %.1f%%)\n", acc, rej, rejPct)
		fmt.Printf("    polls:    ok +%.0f  failed +%.0f\n",
			delta("avalanche_snowman_polls_successful"), delta("avalanche_snowman_polls_failed"))
		fmt.Printf("    proposer slots (accepted blocks): slot0=%.0f slot1=%.0f slot2=%.0f slot3+=%.0f\n",
			slot0, slot1, slot2, slot3)
		fmt.Printf("    gauges@end: processing=%.0f building_slot=%.0f\n", proc, buildSlot)
	}
}
