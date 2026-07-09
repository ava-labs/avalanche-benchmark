package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// nodeHealth is the ACTUAL state of a node, observed read-only over its RPC -
// distinct from intent. This is what makes the summary honest: a node can be
// "intended up" yet still BOOTSTRAPPING (alive but not voting/serving).
type nodeHealth int

const (
	healthDown nodeHealth = iota
	healthBootstrapping
	healthServing
)

func (h nodeHealth) String() string {
	switch h {
	case healthServing:
		return "SERVING"
	case healthBootstrapping:
		return "BOOTSTRAPPING"
	default:
		return "DOWN"
	}
}

// classifyHealth maps one RPC probe outcome to a health state. Pure (unit-tested).
//   - connErr: the dial/transport failed (refused) ⇒ process down or not listening.
//   - status 503 or body "not done bootstrapping" ⇒ alive but still bootstrapping.
//   - a parseable eth_blockNumber result ⇒ serving, with the block height.
func classifyHealth(connErr bool, status int, body string) (nodeHealth, uint64) {
	if connErr {
		return healthDown, 0
	}
	if status == http.StatusServiceUnavailable || strings.Contains(body, "not done bootstrapping") {
		return healthBootstrapping, 0
	}
	const marker = `"result":"0x`
	if i := strings.Index(body, marker); i >= 0 {
		rest := body[i+len(marker):]
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			if n, err := strconv.ParseUint(rest[:j], 16, 64); err == nil {
				return healthServing, n
			}
		}
	}
	// Reachable but unexpected - treat as still coming up rather than serving.
	return healthBootstrapping, 0
}

// neededOnlineToRejoin is the number of validators that must be connected for a
// (re)starting validator to clear avalanchego's bootstrap startup latch:
// ceil(75% of the validator set) - see chains/manager.go NewStartup(...,(3*W+3)/4)
// and the wiki note on recovering a fully-stalled L1. For 3 validators this is 3.
func neededOnlineToRejoin(nVal int) int { return (3*nVal + 3) / 4 }

// quorumNeeded is the minimum validators that must be serving for the chain to
// keep producing - ceil(2/3 of the set). For 3 validators this is 2.
func quorumNeeded(nVal int) int { return (2*nVal + 2) / 3 }

type healthResult struct {
	state nodeHealth
	block uint64
}

// rpcURL is the L1 RPC endpoint for pool slot i, on that instance's HTTP port
// (9652 for a normal node, bumped for a co-located one).
func (c *config) rpcURL(i int) string {
	in := c.instances[i]
	return fmt.Sprintf("http://%s:%d/ext/bc/%s/rpc", in.host, in.httpPort, c.chainID)
}

func probe(client *http.Client, url string) healthResult {
	resp, err := client.Post(url, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`))
	if err != nil {
		st, bn := classifyHealth(true, 0, "")
		return healthResult{st, bn}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	st, bn := classifyHealth(false, resp.StatusCode, string(b))
	return healthResult{st, bn}
}

// consensusHealth probes a node's AvalancheGo /ext/health for the L1 chain and
// returns the signals that actually indicate "ready to participate in consensus" -
// far stronger than classifyHealth's bare eth_blockNumber answer:
//   - percentConnected: fraction of the chain's validator STAKE this node is
//     connected to (1.0 == it sees the whole validator set; a just-restarted
//     validator answers eth_blockNumber within seconds but climbs from <1 here as
//     it re-handshakes the consensus mesh - this is the gap that let the rolling
//     restore fire the next swap too early);
//   - longestProcessing: how long the oldest undecided block has been in flight
//     (grows when consensus is stalled);
//   - lastAccepted: the consensus-accepted tip (advances only while producing).
//
// ok is false if the endpoint is unreachable or the L1 chain's check is absent
// (node still coming up) - callers treat that as not-ready. A 503 (node-unhealthy)
// still carries the JSON body, so the per-chain numbers are parsed regardless.
func (c *config) consensusHealth(i int) (percentConnected float64, longestProcessing time.Duration, lastAccepted uint64, ok bool) {
	in := c.instances[i]
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/ext/health/health", in.host, in.httpPort))
	if err != nil {
		return 0, 0, 0, false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body) // a 503 (unhealthy) still carries the JSON body
	return parseConsensusHealth(body, c.chainID)
}

// parseConsensusHealth extracts the L1 chain's consensus signals from an
// /ext/health/health body. It decodes the chain's check ON ITS OWN (via
// json.RawMessage) because the checks map mixes message shapes - an object for a
// chain, a bare string for "bls" ("node is not a validator"), an empty array for
// "bootstrapped" - so decoding the whole map into one typed struct fails on those
// non-object messages and reports every node as unreadable. Pure + unit-tested.
func parseConsensusHealth(body []byte, chainID string) (percentConnected float64, longestProcessing time.Duration, lastAccepted uint64, ok bool) {
	var top struct {
		Checks map[string]json.RawMessage `json:"checks"`
	}
	if json.Unmarshal(body, &top) != nil {
		return 0, 0, 0, false
	}
	raw, found := top.Checks[chainID]
	if !found {
		return 0, 0, 0, false
	}
	var chk struct {
		Message struct {
			Engine struct {
				Consensus struct {
					LastAcceptedHeight     uint64 `json:"lastAcceptedHeight"`
					LongestProcessingBlock string `json:"longestProcessingBlock"`
				} `json:"consensus"`
			} `json:"engine"`
			Networking struct {
				PercentConnected float64 `json:"percentConnected"`
			} `json:"networking"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &chk) != nil {
		return 0, 0, 0, false
	}
	lp, _ := time.ParseDuration(chk.Message.Engine.Consensus.LongestProcessingBlock)
	return chk.Message.Networking.PercentConnected, lp, chk.Message.Engine.Consensus.LastAcceptedHeight, true
}

// checkHealth probes every uncordoned machine's RPC once, in parallel, and returns
// a point-in-time snapshot. Read-only and non-blocking: it never stops/swaps/starts
// anything and never waits - run status.sh again (or `watch` it) to see changes.
// Cordoned machines are skipped (they are meant to be down).
func (c *config) checkHealth(intents []MachineIntent) []healthResult {
	client := &http.Client{Timeout: 4 * time.Second}
	results := make([]healthResult, len(intents))
	var wg sync.WaitGroup
	for i, in := range intents {
		if in.Cordoned {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = probe(client, c.rpcURL(i))
		}(i)
	}
	wg.Wait()
	return results
}

// reportHealth prints each node grouped by datacenter with two columns - its
// ACTUAL on-chain STAKE tier (validator/spare/dead/rpc, with a "-> tier
// pending" marker while a weight change is in flight) and its physical
// REACHABILITY (SERVING/BOOTSTRAPPING/DOWN, or off when intentionally down) -
// then an honest summary and hints for the two non-obvious failure modes
// (lost quorum, and the 75% rejoin latch that keeps a single brought-up
// validator from recovering a stalled chain). "Validator" in the summary
// means a slot whose ACTUAL P-chain weight is the validator tier (>=1% of
// total); the parenthetical "intended up" still counts by desired weight.
// actual is fetchActualWeights' slot -> P-chain weight map; nil means the
// P-chain was unreadable and everything falls back to desired weights.
func reportHealth(cfg *config, intents []MachineIntent, results []healthResult, actual map[int]uint64) {
	t := cfg.topo
	total := totalWeight(intents)
	var actualTotal uint64
	for _, w := range actual {
		actualTotal += w
	}

	// The leading number is the machine's CLI handle: `fleet down 7` etc.
	numW := len(strconv.Itoa(len(intents)))
	nameW := len("node")
	for i := range intents {
		if f := fmt.Sprintf("%s (%s)", t.MachineName(i), cfg.nodeIPs[i]); len(f) > nameW {
			nameW = len(f)
		}
	}

	// Stake column width follows its widest cell: a pending marker
	// ("validator -> spare pending") is far wider than a bare tier name.
	stakes := make([]string, len(intents))
	stakeW := len("stake")
	for i, in := range intents {
		w, have := actual[i]
		stakes[i] = stakeCell(in.Weight, w, have)
		if len(stakes[i]) > stakeW {
			stakeW = len(stakes[i])
		}
	}

	sites := []int{siteA}
	if t.TwoSite {
		sites = append(sites, siteB)
	}

	servingValidators, intendedValidators, activeSlots := 0, 0, 0
	bootstrappingValidator, downUncordoned := false, false

	for _, site := range sites {
		fmt.Printf("DC %s\n", strings.ToUpper(siteName(site)))
		fmt.Printf("  %-*s  %-*s  %-*s  %s\n", numW, "#", nameW, "node", stakeW, "stake", "reachable")
		for i, in := range intents {
			if t.Site(i) != site {
				continue
			}
			field := fmt.Sprintf("%s (%s)", t.MachineName(i), cfg.nodeIPs[i])
			// "active" (a consensus-relevant validator) is judged by the
			// ACTUAL P-chain weight when readable, desired otherwise.
			active := isActiveWeight(in.Weight, total)
			if w, have := actual[i]; have {
				active = isActiveWeight(w, actualTotal)
			}
			if active {
				activeSlots++
			}
			var reach string
			switch {
			case in.Cordoned:
				reach = "off (down by intent)"
			case results[i].state == healthServing:
				reach = fmt.Sprintf("SERVING block=%d", results[i].block)
			case results[i].state == healthBootstrapping:
				reach = "BOOTSTRAPPING (catching up)"
			default:
				reach = "DOWN (not responding!)"
			}
			fmt.Printf("  %-*d  %-*s  %-*s  %s\n", numW, i+1, nameW, field, stakeW, stakes[i], reach)

			if in.Cordoned {
				continue
			}
			if isActiveWeight(in.Weight, total) {
				intendedValidators++
			}
			if active {
				switch results[i].state {
				case healthServing:
					servingValidators++
				case healthBootstrapping:
					bootstrappingValidator = true
				default:
					downUncordoned = true
				}
			} else if results[i].state == healthDown {
				downUncordoned = true
			}
		}
	}

	if actual == nil {
		fmt.Println("(P-chain unreadable, showing desired weights)")
	}
	fmt.Printf("validators serving: %d/%d (intended up: %d/%d)\n",
		servingValidators, activeSlots, intendedValidators, activeSlots)

	if servingValidators < quorumNeeded(activeSlots) {
		fmt.Printf("WARNING: fewer than %d validators serving - chain lacks quorum and is HALTED until restored.\n", quorumNeeded(activeSlots))
	}
	if bootstrappingValidator && intendedValidators < neededOnlineToRejoin(activeSlots) {
		fmt.Printf("HINT: a rejoining validator needs >=%d of %d validators connected to clear the bootstrap\n",
			neededOnlineToRejoin(activeSlots), activeSlots)
		fmt.Println("      startup latch. Bring up the remaining validator machine(s) so they recover together.")
	}
	if downUncordoned {
		fmt.Println("NOTE: an uncordoned node is not responding - check its logs, or `up` it to rebuild.")
	}
}
