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
	healthCatchingUp
	healthServing
	// healthHalted: the node answers its RPC (so classifyHealth would call it
	// SERVING) but its consensus tip is wedged - the oldest undecided block has
	// been processing longer than maxItemProcessingTime. This is the
	// whole-fleet-frozen case the relative "behind the fleet max" check misses:
	// when every node is stuck at the same height nobody is "behind", so the
	// summary wrongly read SERVING N/N. Overlaid by markHalted, status-only.
	healthHalted
)

func (h nodeHealth) String() string {
	switch h {
	case healthServing:
		return "SERVING"
	case healthHalted:
		return "HALTED"
	case healthCatchingUp:
		return "CATCHING UP"
	case healthBootstrapping:
		return "BOOTSTRAPPING"
	default:
		return "DOWN"
	}
}

// catchUpThreshold: a node whose height is more than this many blocks below the
// fleet max is CATCHING UP, not SERVING. At 25-80ms blocks 200 blocks is only
// seconds of production - wide enough to absorb sampling skew across the
// sequential polls of one cycle, narrow enough to catch a genuinely-behind node.
const catchUpThreshold = 200

// fleetMaxBlock is the highest height any responding node reported this cycle.
func fleetMaxBlock(results []healthResult) uint64 {
	var max uint64
	for _, r := range results {
		if (r.state == healthServing || r.state == healthCatchingUp) && r.block > max {
			max = r.block
		}
	}
	return max
}

// markCatchingUp downgrades SERVING results more than catchUpThreshold blocks
// below the fleet max. A bare eth_blockNumber answer is not "serving": a node
// thousands of blocks behind tip both misleads status and is fork-risk (the
// documented sibling-race self-finalization). With a single responding node the
// max is its own height, so it stays SERVING - bringing one node up while the
// rest of the fleet is down never deadlocks.
func markCatchingUp(results []healthResult) {
	max := fleetMaxBlock(results)
	for i := range results {
		if results[i].state == healthServing && results[i].block+catchUpThreshold < max {
			results[i].state = healthCatchingUp
		}
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

// wedgeFrozenPolls: consecutive unchanged-height polls (5s apart, so ~10s of
// zero movement) before a CATCHING UP node is declared fork-wedged. A healthy
// syncing node executes hundreds of blocks per poll at 25-80ms block times; a
// node that self-finalized a sibling block (the documented sibling-race fork
// wedge) serves the same frozen height forever. Two frozen intervals instead
// of one is cheap insurance against a momentary execution stall; a false
// positive only costs a chainData wipe + state re-sync (minutes) on a node
// that was behind anyway.
const wedgeFrozenPolls = 2

// wedgeFrozen is the fork-wedge detector decision, pure for testing. Fed one
// poll of a waited-on machine, it returns the updated consecutive-frozen count
// and whether the detector fires. It only ever fires for a CATCHING UP node
// (which by markCatchingUp already means behind the fleet max by more than
// catchUpThreshold) whose reported height matched the previous poll
// wedgeFrozenPolls times in a row; any height movement or state change resets it.
func wedgeFrozen(state nodeHealth, block, prevBlock uint64, prevFrozen int) (frozen int, wedged bool) {
	if state != healthCatchingUp || block != prevBlock {
		return 0, false
	}
	frozen = prevFrozen + 1
	return frozen, frozen >= wedgeFrozenPolls
}

// Stall budgets: how long a waited-on machine may show NO progress before
// waitServing intervenes (rebuild once, then give up). Progress - not wall
// time - drives the clock, so a node legitimately fetching/executing a long
// bootstrap backlog is waited on indefinitely instead of being wiped every 10
// minutes (the 2026-07-11 wipe-loop incident: the recovery step was the
// disease). The BOOTSTRAPPING budget is the generous one: it must cover the
// silent Bootstrapper.Clear window (an unlogged AtomicClear of a leftover
// bootstrap backlog in db/ before "starting state sync" appears - zero bs
// metric movement for many minutes, measured ~21 min for a 869k-block backlog)
// plus a full state sync (~5 min).
const (
	bootstrapStallBudget = 25 * time.Minute
	defaultStallBudget   = 10 * time.Minute
)

// stallBudget is the no-progress allowance for a machine in the given state.
func stallBudget(state nodeHealth) time.Duration {
	if state == healthBootstrapping {
		return bootstrapStallBudget
	}
	return defaultStallBudget
}

// madeProgress reports forward motion between two consecutive polls of a
// waited-on machine: any state change (down->bootstrapping, bootstrapping->
// catching up, ...), block-height movement, or movement of the chain's
// bootstrap counters (bs, the summed bs_fetched+bs_accepted metrics, valid
// only when bsOK). The first successful bs read (prevBSOK false) counts as
// progress: the chain's engine registering its metrics IS forward motion.
// Pure (unit-tested). NOTE: a crash-looping node flaps states and therefore
// always "progresses"; that pathology stays visible in the printed poll lines
// and is left to the operator rather than guessed at here.
func madeProgress(prevState, state nodeHealth, prevBlock, block, prevBS, bs uint64, prevBSOK, bsOK bool) bool {
	if state != prevState {
		return true
	}
	if block > prevBlock {
		return true
	}
	return bsOK && (!prevBSOK || bs > prevBS)
}

// bootstrapCounter reads the L1 chain's bootstrap progress counter from the
// node's /ext/metrics: bs_fetched + bs_accepted summed. Fetched advances while
// blocks download, accepted while the executor replays them, so either phase
// of a legitimate bootstrap moves the counter. ok=false when the endpoint or
// the chain's counters are unavailable (node down, engine not started yet).
func (c *config) bootstrapCounter(i int) (uint64, bool) {
	in := c.instances[i]
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/ext/metrics", in.host, in.httpPort))
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return parseBootstrapCounter(string(b), c.chainID)
}

// parseBootstrapCounter sums avalanche_snowman_bs_fetched + bs_accepted for
// THE L1 chain from a Prometheus /ext/metrics body. Filtering on the chain
// label is load-bearing: the fleet's P-chain runs --p-chain-follow-only and
// its bs counters (chain="P") advance forever, which must never read as L1
// progress. Values are parsed as floats (Prometheus renders large counters in
// scientific notation). Pure (unit-tested).
func parseBootstrapCounter(body, chainID string) (uint64, bool) {
	label := `chain="` + chainID + `"`
	var sum float64
	found := false
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "avalanche_snowman_bs_fetched{") &&
			!strings.HasPrefix(line, "avalanche_snowman_bs_accepted{") {
			continue
		}
		if !strings.Contains(line, label) {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[sp+1:], 64)
		if err != nil {
			continue
		}
		sum += v
		found = true
	}
	return uint64(sum), found
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

// rpcURL is the L1 RPC endpoint for node i, on that node's HTTP port.
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

// maxItemProcessingTime mirrors avalanchego's default consensus parameter: a
// block undecided longer than this trips the "block processing too long"
// health warning and means the chain has stopped finalizing. A wedged L1
// under the one-burst weight-shift failure logged "block processing too long:
// 32s > 30s" right before it stalled with no self-recovery.
const maxItemProcessingTime = 30 * time.Second

// markHalted overlays HALTED onto any SERVING result whose consensus health
// shows the oldest undecided block has been processing longer than
// maxItemProcessingTime: a frozen tip that still answers eth_blockNumber. This
// is the signal that makes a whole-fleet freeze visible - checkHealth alone
// only ranks nodes against the fleet max, so an all-frozen fleet reads SERVING
// N/N. Probed in parallel and only for nodes already answering their RPC (the
// rest have no consensus tip to stall).
//
// Status-only, deliberately NOT folded into checkHealth: the recovery wait
// loop (waitServing) drives off classifyHealth + wedgeFrozen, and a transient
// processing spike on a node that has otherwise reached tip must not knock it
// out of SERVING there and restart its rebuild clock.
func (c *config) markHalted(results []healthResult) {
	var wg sync.WaitGroup
	for i := range results {
		if results[i].state != healthServing {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, longest, _, ok := c.consensusHealth(i); ok && longest > maxItemProcessingTime {
				results[i].state = healthHalted
			}
		}(i)
	}
	wg.Wait()
}

// checkHealth probes every machine's RPC once, in parallel, and returns
// a point-in-time snapshot of observed facts. Read-only and non-blocking: it
// never stops/swaps/starts anything and never waits - run `./fleet status`
// again (or `watch` it) to see changes.
func (c *config) checkHealth() []healthResult {
	client := &http.Client{Timeout: 4 * time.Second}
	results := make([]healthResult, len(c.instances))
	var wg sync.WaitGroup
	for i := range c.instances {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = probe(client, c.rpcURL(i))
		}(i)
	}
	wg.Wait()
	// One RPC round per cycle: the heights just fetched double as the
	// fleet-max sample for the CATCHING UP classification.
	markCatchingUp(results)
	return results
}

// reportHealth prints each node - grouped by its display-only dc tag when the
// inventory carries any, flat otherwise - with two columns: its ACTUAL
// on-chain STAKE tier (validator/spare/dead/rpc, read from the P-chain;
// weights MOVE via bin/l1) and its physical REACHABILITY (SERVING/CATCHING
// UP/BOOTSTRAPPING/DOWN, all observed facts) - then an honest summary and
// hints for the two non-obvious failure modes (lost quorum, and the 75%
// rejoin latch that keeps a single brought-up validator from recovering a
// stalled chain). "Validator" in the summary means a node whose on-chain
// weight is the validator tier (>=1% of total). actual is the node index ->
// P-chain weight map (fetchWeights); nil means the P-chain was unreadable, so
// the stake column degrades to "?" and the quorum math (which needs the
// weights) is skipped.
func reportHealth(cfg *config, results []healthResult, actual map[int]uint64) {
	var total uint64
	for _, w := range actual {
		total += w
	}

	nameW := len("node")
	for i := range results {
		if f := fmt.Sprintf("%s (%s)", cfg.nodes[i].Name, cfg.nodes[i].Host); len(f) > nameW {
			nameW = len(f)
		}
	}

	stakes := make([]string, len(results))
	stakeW := len("stake")
	for i := range results {
		w, have := actual[i]
		stakes[i] = stakeCell(w, have, cfg.nodes[i].IsValidator())
		if len(stakes[i]) > stakeW {
			stakeW = len(stakes[i])
		}
	}

	// Group by dc tag in first-appearance order; untagged inventories get one
	// flat unlabeled group.
	var dcs []string
	byDC := map[string][]int{}
	for i, n := range cfg.nodes {
		if _, ok := byDC[n.DC]; !ok {
			dcs = append(dcs, n.DC)
		}
		byDC[n.DC] = append(byDC[n.DC], i)
	}

	servingValidators, activeSlots := 0, 0
	bootstrappingValidator, downNode, haltedValidator := false, false, false

	for _, dc := range dcs {
		switch {
		case dc != "":
			fmt.Printf("DC %s\n", dc)
		case len(dcs) > 1:
			fmt.Println("(no dc)")
		}
		fmt.Printf("  %-*s  %-*s  %s\n", nameW, "node", stakeW, "stake", "reachable")
		for _, i := range byDC[dc] {
			field := fmt.Sprintf("%s (%s)", cfg.nodes[i].Name, cfg.nodes[i].Host)
			// "active" (a consensus-relevant validator) is judged by the
			// on-chain weight.
			w, have := actual[i]
			active := have && isActiveWeight(w, total)
			if active {
				activeSlots++
			}
			var reach string
			switch results[i].state {
			case healthServing:
				reach = fmt.Sprintf("SERVING block=%d", results[i].block)
			case healthHalted:
				reach = fmt.Sprintf("HALTED (block processing > %s, tip frozen at %d)", maxItemProcessingTime, results[i].block)
			case healthCatchingUp:
				reach = fmt.Sprintf("CATCHING UP (behind %d) block=%d",
					fleetMaxBlock(results)-results[i].block, results[i].block)
			case healthBootstrapping:
				reach = "BOOTSTRAPPING (catching up)"
			default:
				reach = "DOWN (not responding!)"
			}
			fmt.Printf("  %-*s  %-*s  %s\n", nameW, field, stakeW, stakes[i], reach)

			if active {
				switch results[i].state {
				case healthServing:
					servingValidators++
				case healthHalted:
					// Answering RPC but consensus wedged: NOT counted as
					// serving, so it drops out of the quorum tally and the
					// halt warning below fires.
					haltedValidator = true
				case healthCatchingUp:
					// Alive and syncing: not serving (behind tip), not down,
					// and past bootstrap so the rejoin-latch hint doesn't apply.
				case healthBootstrapping:
					bootstrappingValidator = true
				default:
					downNode = true
				}
			} else if results[i].state == healthDown {
				downNode = true
			}
		}
	}

	if actual == nil {
		fmt.Println("(P-chain weights unreadable: stake tiers and quorum math unavailable this run)")
	} else {
		fmt.Printf("validators serving: %d/%d\n", servingValidators, activeSlots)
		if haltedValidator {
			fmt.Printf("HALTED: a validator's consensus tip is frozen (block processing > %s) - the L1 has stopped finalizing and will not self-recover; restore the validator set (see the runbook).\n", maxItemProcessingTime)
		}
		if servingValidators < quorumNeeded(activeSlots) {
			fmt.Printf("WARNING: fewer than %d validators serving - chain lacks quorum and is HALTED until restored.\n", quorumNeeded(activeSlots))
		}
		if bootstrappingValidator && servingValidators < neededOnlineToRejoin(activeSlots) {
			fmt.Printf("HINT: a rejoining validator needs >=%d of %d validators connected to clear the bootstrap\n",
				neededOnlineToRejoin(activeSlots), activeSlots)
			fmt.Println("      startup latch. Bring up the remaining validator machine(s) so they recover together.")
		}
	}
	if downNode {
		fmt.Println("NOTE: a node is not responding - check its logs, or `up` it to rebuild.")
	}
}
