package main

import (
	"fmt"
	"os"
	"strings"
)

// Active-site ingress routing. Bombard must drive ONLY the data center that
// currently holds the validator set, and switch when a failover/restore moves it —
// never both sites at once. Sending to the standby site's RPC injects txs that then
// have to be gossiped cross-region to the active validators (added latency) and
// risks stale-nonce rejections off the standby's slightly-behind view. reconcile
// owns the failover, so it publishes the active site's RPC URLs to a file;
// bombard watches it and restricts ingress to those endpoints (see cmd/bombard
// watchActiveRPCs). Coordinated via a local file on the shared control host, simply
// ignored if bombard isn't running. The path must match bombard's
// defaultActiveRPCsFile / the BOMBARD_ACTIVE_RPCS_FILE override.

func activeRPCsFilePath() string {
	if p := os.Getenv("BOMBARD_ACTIVE_RPCS_FILE"); p != "" {
		return p
	}
	return "/tmp/bombard.active-rpcs"
}

// activeSite returns the site that currently holds the validator MAJORITY — the one
// bombard should target. During a graceful restore the majority flips after the 2nd
// validator lands, so ingress follows the migration. Defaults to siteA when neither
// holds a majority (e.g. a stalled chain), so the published set is never empty.
func activeSite(intents []MachineIntent, topo Topology) int {
	var a, b int
	for i, in := range intents {
		if in.Cordoned || !topo.isValidatorKey(in.Key) {
			continue
		}
		if topo.Site(i) == siteB {
			b++
		} else {
			a++
		}
	}
	if b > a {
		return siteB
	}
	return siteA
}

// writeActiveRPCs publishes the active site's RPC URLs so bombard routes
// ingress to that site only — and re-points when the validator set moves. Best-effort
// (a no-op if bombard isn't running). Single-site mode keeps bombard's default
// (all RPCs), so it is skipped there.
func (c *config) writeActiveRPCs(intents []MachineIntent) {
	if !c.topo.TwoSite {
		return
	}
	site := activeSite(intents, c.topo)
	var urls []string
	for _, i := range rpcMachineIdxs(c.topo, site) {
		in := c.instances[i]
		urls = append(urls, fmt.Sprintf("http://%s:%d/ext/bc/%s/rpc", in.host, in.httpPort, c.chainID))
	}
	if len(urls) == 0 {
		return
	}
	_ = os.WriteFile(activeRPCsFilePath(), []byte(strings.Join(urls, "\n")+"\n"), 0o644)
	fmt.Printf("  ingress: active site %s — bombard routed to %s\n", siteName(site), strings.Join(urls, " "))
}
