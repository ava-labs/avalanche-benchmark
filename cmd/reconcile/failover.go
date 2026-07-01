package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// nukeSite models a real site loss — a region outage / power cut, not a graceful drain.
// Every avalanchego on the given site is hard-killed (SIGKILL) CONCURRENTLY, so the whole
// site dies at once instead of being staggered by the sequential stop-swap loop. It runs
// BEFORE reconcileBackupHeights so the surviving site's retained tip is frozen at the true
// RPO boundary: the survivor then forms consensus on whatever blocks it already holds,
// pulling zero state from the dead site. (We kill the process, not the box, so the boxes
// stay SSH-reachable for the test harness — the chain-level effect, loss of the dead site's
// validators and a frozen tip, is identical to a real outage.)
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

// failoverSettleTimeout bounds how long a failover waits for the surviving site to settle
// before it reads the live tip (see waitForSiteSettled). Short, because the common case is a
// node restart that serves within seconds; the bound keeps a genuinely long bootstrap from
// stalling the cutover.
const failoverSettleTimeout = 90 * time.Second

// Hard-site-failover recovery.
//
// A hard `site-failover` assumes the old site is GONE and cuts the validator set over to the
// surviving site. The surviving site's would-be validators are trackers that may sit behind
// the tip (cross-region lag) or at inconsistent heights. Promote them as-is at WILDLY
// different heights and consensus can't form a quorum on any tip — the chain DEADLOCKS even
// though every node reports SERVING (measured 2026-06-22: 659k/426k/412k produced zero blocks).
//
// reconcileBackupHeights recovers EVERY role — validators, spare, AND the pinned RPCs — by
// STATE-SYNC: any node behind the live tip by more than syncToleranceBlocks (or down) is wiped
// and reset to its light role-default config, so reconcile's start resyncs it to the tip.
// State-sync lands every wiped node at a common recent pivot, so the promoted set agrees on a
// tip (no inconsistent-height deadlock) and stays PRUNED — no DB clone, no archive-config flip,
// no pruning-mode mismatch (the failure modes the clone approach kept hitting). The surviving
// site's RPCs (which retain the tip) serve the state-sync; the chain state is small and, because
// the old site is gone, the tip is static, so the sync converges quickly. A node already within
// tolerance of the tip is left as-is (a promoted validator, or a serving RPC — fast path; at a
// healthy cadence the standby keeps up, so this is the common case).
//
// The RPCs now run the same light (state-sync + pruning) profile as the validators, so a
// far-behind RPC self-heals by the same wipe + state-sync — no archive->archive clone. It no
// longer retains full history, but recovery is a plain resync. Runs BEFORE reconcile starts.
func (c *config) reconcileBackupHeights(intents []MachineIntent, targetSite int) {
	topo := c.topo

	// Let the surviving site settle (RPCs serving, heights stable) so the tip read below is the
	// true retained tip, not a momentarily-recovering node's partial height.
	res := c.waitForSiteSettled(intents, targetSite, failoverSettleTimeout)

	// Live tip = highest block any SERVING node on the site holds (the RPCs hold it).
	var tip uint64
	for i := 0; i < topo.Size(); i++ {
		if topo.Site(i) == targetSite && i < len(res) && res[i].state == healthServing && res[i].block > tip {
			tip = res[i].block
		}
	}

	// Every role — validators, spares, AND pinned RPCs: state-sync any that are behind the tip
	// (or down); leave the rest. The RPCs run the same light profile now, so they resync too.
	targets := append([]int{}, validatorDestIdx(topo, targetSite)...)
	targets = append(targets, spareDestIdxs(topo, targetSite)...)
	targets = append(targets, rpcMachineIdxs(topo, targetSite)...)
	var promote, resync []string
	for _, i := range targets {
		if i >= len(intents) || intents[i].Cordoned {
			continue
		}
		serving := i < len(res) && res[i].state == healthServing
		gap := "DOWN"
		if serving && res[i].block <= tip {
			gap = fmt.Sprintf("%d blocks behind tip", tip-res[i].block)
		}
		if serving && tip > 0 && tip-res[i].block <= uint64(syncToleranceBlocks) {
			fmt.Printf("  %s: %s — within %d-block tolerance, PROMOTE AS-IS (avalanchego self-heals the gap)\n",
				topo.MachineName(i), gap, syncToleranceBlocks)
			promote = append(promote, topo.MachineName(i)) // current enough — promote as-is
			continue
		}
		fmt.Printf("  !! %s: %s — beyond %d-block tolerance: WIPE + STATE-SYNC to the site's live tip (deadlock guard)\n",
			topo.MachineName(i), gap, syncToleranceBlocks)
		c.deployChainConfig(i) // pruned + state-sync-enabled, matches the wiped DB
		c.wipeL1Data(i)        // clear data dir -> forces state-sync on start
		resync = append(resync, topo.MachineName(i))
	}
	if len(resync) == 0 {
		fmt.Printf("== failover: site %s recovery (tip %d) — all validators within tolerance, clean hot-standby promote, NO resync ==\n",
			siteName(targetSite), tip)
	} else {
		fmt.Printf("== failover: site %s recovery (tip %d) — promote-as-is: [%s]  WIPED+state-sync: [%s] ==\n",
			siteName(targetSite), tip, strings.Join(promote, " "), strings.Join(resync, " "))
	}
}

// waitForSiteSettled gives the surviving site a bounded window to finish recovering before a
// failover reads the live tip, then returns the settled health snapshot. A failover fired
// right after a restore can catch the site's RPCs still bootstrapping: not yet SERVING,
// or SERVING but still climbing to their retained tip. If the tip were read in that instant the
// at-tip RPC would be missed and behind-but-close validators would be promoted at a stale tip.
//
// In a hard failover the old site is gone, so no new blocks are minted and the surviving nodes'
// heights converge to the highest retained tip. We poll until that max height holds steady
// across two consecutive reads (every node done climbing) with at least two nodes serving, then
// return — or return the latest snapshot once the timeout elapses. Best-effort: a node that
// never serves simply isn't counted; the wait only gives a quickly-restarting node the chance
// to reappear.
func (c *config) waitForSiteSettled(intents []MachineIntent, targetSite int, timeout time.Duration) []healthResult {
	deadline := time.Now().Add(timeout)
	haveMax := false
	var prevMax uint64
	var res []healthResult
	for {
		res = c.checkHealth(intents)
		var maxH uint64
		serving := 0
		for i := 0; i < c.topo.Size(); i++ {
			if c.topo.Site(i) != targetSite || i >= len(intents) || intents[i].Cordoned {
				continue
			}
			if i >= len(res) || res[i].state != healthServing {
				continue
			}
			serving++
			if res[i].block > maxH {
				maxH = res[i].block
			}
		}
		// Settled: enough of the site is serving and the highest retained tip stopped moving.
		if serving >= 2 && maxH > 0 && haveMax && maxH == prevMax {
			fmt.Printf("failover: surviving site settled — %d node(s) serving, tip %d.\n", serving, maxH)
			return res
		}
		if time.Now().After(deadline) {
			fmt.Printf("failover: settle window (%s) elapsed — proceeding with %d serving node(s), tip %d.\n", timeout, serving, maxH)
			return res
		}
		prevMax, haveMax = maxH, true
		time.Sleep(restorePollInterval)
	}
}
