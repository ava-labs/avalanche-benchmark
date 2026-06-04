package main

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// totalValidators is the fixed L1 validator set size (keys 6,7,8).
const totalValidators = valKeyHi - valKeyLo + 1

// nodeHealth is the ACTUAL state of a node, observed read-only over its RPC —
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
	// Reachable but unexpected — treat as still coming up rather than serving.
	return healthBootstrapping, 0
}

// neededOnlineToRejoin is the number of validators that must be connected for a
// (re)starting validator to clear avalanchego's bootstrap startup latch:
// ceil(75% of the validator set) — see chains/manager.go NewStartup(...,(3*W+3)/4)
// and the wiki note on recovering a fully-stalled L1. For 3 validators this is 3.
func neededOnlineToRejoin() int { return (3*totalValidators + 3) / 4 }

type healthResult struct {
	state nodeHealth
	block uint64
}

func (c *config) rpcURL(ip string) string {
	return fmt.Sprintf("http://%s:9652/ext/bc/%s/rpc", ip, c.chainID)
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

// checkHealth probes every uncordoned machine's RPC once, in parallel, and returns
// a point-in-time snapshot. Read-only and non-blocking: it never stops/swaps/starts
// anything and never waits — run status.sh again (or `watch` it) to see changes.
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
		go func(i int, ip string) {
			defer wg.Done()
			results[i] = probe(client, c.rpcURL(ip))
		}(i, c.nodeIPs[i])
	}
	wg.Wait()
	return results
}

// reportHealth prints the actual per-node health and an honest summary, with
// hints for the two non-obvious failure modes (lost quorum, and the 75% rejoin
// latch that keeps a single brought-up validator from recovering a stalled chain).
func reportHealth(cfg *config, intents []MachineIntent, results []healthResult) {
	servingValidators, intendedValidators := 0, 0
	bootstrappingValidator, downUncordoned := false, false

	for i, in := range intents {
		ip := cfg.nodeIPs[i]
		label := roleLabel(in.Key)
		if in.Cordoned {
			fmt.Printf("  m%d (%s): cordoned       %-9s (down by intent)\n", i+1, ip, label)
			continue
		}
		r := results[i]
		switch r.state {
		case healthServing:
			fmt.Printf("  m%d (%s): SERVING        %-9s block=%d\n", i+1, ip, label, r.block)
		case healthBootstrapping:
			fmt.Printf("  m%d (%s): BOOTSTRAPPING  %-9s (catching up, not yet serving)\n", i+1, ip, label)
		default:
			fmt.Printf("  m%d (%s): DOWN           %-9s (uncordoned but not responding!)\n", i+1, ip, label)
		}
		if isValidatorKey(in.Key) {
			intendedValidators++
			switch r.state {
			case healthServing:
				servingValidators++
			case healthBootstrapping:
				bootstrappingValidator = true
			default:
				downUncordoned = true
			}
		} else if r.state == healthDown {
			downUncordoned = true
		}
	}

	fmt.Printf("validators serving: %d/%d (intended up: %d/%d)\n",
		servingValidators, totalValidators, intendedValidators, totalValidators)

	if servingValidators < 2 {
		fmt.Println("WARNING: fewer than 2 validators serving — chain lacks quorum and is HALTED until restored.")
	}
	if bootstrappingValidator && intendedValidators < neededOnlineToRejoin() {
		fmt.Printf("HINT: a rejoining validator needs >=%d of %d validators connected to clear the bootstrap\n",
			neededOnlineToRejoin(), totalValidators)
		fmt.Println("      startup latch. Bring up the remaining validator machine(s) so they recover together.")
	}
	if downUncordoned {
		fmt.Println("NOTE: an uncordoned node is not responding — re-run failover.sh, or check its logs.")
	}
}

func roleLabel(key int) string {
	if isValidatorKey(key) {
		return fmt.Sprintf("v%d", key-valKeyLo+1)
	}
	if isRPCKey(key) {
		return "rpc(nv)"
	}
	return "spare(nv)"
}
