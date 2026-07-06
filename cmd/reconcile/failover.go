package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	// syncToleranceBlocks: a node is "synced enough" to carry active weight
	// when its serving height is within this many blocks of the fleet tip
	// (avalanchego self-heals a small gap once it participates).
	syncToleranceBlocks = 100
	// sitePollInterval paces the readiness polls during a graceful restore.
	sitePollInterval = 3 * time.Second
	// siteReadyTimeout bounds how long restore waits for the returning site to
	// catch up (state-sync from scratch can take a while on Fuji).
	siteReadyTimeout = 30 * time.Minute
)

func siteName(site int) string {
	if site == siteB {
		return "b"
	}
	return "a"
}

func otherSite(site int) int {
	if site == siteA {
		return siteB
	}
	return siteA
}

// rpcMachineIdxs lists the pool indexes of a site's pinned RPC slots.
func rpcMachineIdxs(t Topology, site int) []int {
	var idxs []int
	for i := 0; i < t.Size(); i++ {
		if t.Site(i) == site && t.IsRPCSlot(i) {
			idxs = append(idxs, i)
		}
	}
	return idxs
}

// nukeSite models a real site loss — a region outage / power cut, not a graceful drain.
// Every avalanchego on the given site is hard-killed (SIGKILL) CONCURRENTLY, so the whole
// site dies at once instead of being staggered by a sequential stop loop. The surviving
// site's standby validators are full consensus participants (weight 1), so they hold the
// accepted tip already; recovery is purely the weight seesaw on the C-chain/P-chain, no
// process restarts and no state surgery. (We kill the process, not the box, so the boxes
// stay SSH-reachable for the test harness.)
func (c *config) nukeSite(site int) {
	var wg sync.WaitGroup
	for i := 0; i < c.topo.Size(); i++ {
		if c.topo.Site(i) != site {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fmt.Printf("  nuke %s (%s): SIGKILL\n", c.topo.MachineName(i), c.nodeIPs[i])
			c.killNode(i)
		}(i)
	}
	wg.Wait()
}

// waitForSiteReady blocks a graceful restore until every staking slot of the
// target site is SERVING within syncToleranceBlocks of the fleet tip: only
// then may the weight seesaw hand it the consensus (handing active weight to
// a still-bootstrapping site would stall the chain until it caught up).
func (c *config) waitForSiteReady(intents []MachineIntent, targetSite int) {
	fmt.Printf("waiting for site %s staking nodes to serve within %d blocks of tip...\n",
		siteName(targetSite), syncToleranceBlocks)
	deadline := time.Now().Add(siteReadyTimeout)
	for {
		res := c.checkHealth(intents)
		var tip uint64
		for i := range res {
			if res[i].state == healthServing && res[i].block > tip {
				tip = res[i].block
			}
		}
		ready, lagging := true, ""
		for i := 0; i < c.topo.Size(); i++ {
			if c.topo.Site(i) != targetSite || !c.topo.IsStakingSlot(i) || intents[i].Cordoned {
				continue
			}
			if res[i].state != healthServing || tip-res[i].block > syncToleranceBlocks {
				ready = false
				lagging = c.topo.MachineName(i)
				break
			}
		}
		if ready {
			fmt.Printf("  site %s ready (tip %d).\n", siteName(targetSite), tip)
			return
		}
		if time.Now().After(deadline) {
			fatalf("site %s did not become ready within %s (%s lagging) — investigate, then re-run restore",
				siteName(targetSite), siteReadyTimeout, lagging)
		}
		fmt.Printf("  waiting: %s not ready (tip %d)\n", lagging, tip)
		time.Sleep(sitePollInterval)
	}
}
