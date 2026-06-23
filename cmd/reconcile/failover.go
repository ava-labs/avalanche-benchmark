package main

import (
	"fmt"
	"os"
)

// Hard-site-failover quorum reconciliation.
//
// A hard `site-failover` assumes the old site is GONE (nuked) and cuts the whole validator
// set over to the surviving site in one shot. The hazard: the surviving site's machines are
// trackers that may sit at different heights — one that kept up next to others wedged blocks
// back (cross-region trackers don't self-heal under load; see standby lag notes). Promote
// them at inconsistent heights and consensus can't form a quorum on ANY tip — the ahead node
// can't get votes for a block the others lack, the behind nodes can't vote on blocks they
// don't have — so the chain DEADLOCKS even though every node reports "SERVING" (measured
// 2026-06-22: 659k/426k/412k produced zero blocks; 2026-06-23: a validator left ~40k behind
// after a clone-then-drift re-split).
//
// reconcileBackupHeights (below) makes the promoted validator set boot byte-identical by
// stopping every validator destination up front and cloning the most-advanced one's DB onto
// the rest. The chain resumes from the highest surviving height; blocks the old site
// finalized above it are the unavoidable RPO of losing it.

// reconcileBackupHeights makes a hard site-failover leave a WORKING quorum by guaranteeing
// the promoted validator set boots at an IDENTICAL height. Promoted validators must agree
// on a tip; if they boot at different heights, consensus can't form a quorum and the chain
// DEADLOCKS until the laggards re-sync (measured 2026-06-23: b1 promoted @104990 while
// b2/b3 sat @101031 — the chain halted and one validator stayed ~40k blocks behind).
//
// The earlier version cloned only onto >tolerance "laggards" and left the others running.
// That re-split the set: a validator that was at the tip kept following the old site and
// drifted further ahead between the snapshot and the cert swap, so the promoted set was
// inconsistent anyway. This version instead:
//   1. STOPS every validator-destination node up front — including ones already at the tip
//      — so none can advance between snapshot and promotion;
//   2. clones the most-advanced one's DB (identity-agnostic: data/validator only, staking
//      creds live outside it) onto ALL the others, unconditionally.
// So every promoted validator boots byte-identical and a quorum forms immediately.
//
// Only the validator destinations are touched. The spare and pinned RPCs hold no validator
// key, so they never gate quorum (they catch up on their own); and the archive RPCs must
// keep their full history — a pruned clone would strip it, so they are reseeded only by a
// graceful restore. Runs BEFORE the reconcile pass starts the nodes. Best-effort: on a
// snapshot failure it logs and lets reconcile start the set as-is rather than aborting.
func (c *config) reconcileBackupHeights(intents []MachineIntent, targetSite int) {
	topo := c.topo

	// The quorum-critical set: the machines that will hold validator keys on the surviving
	// site. These — and only these — must be height-consistent for a quorum to form.
	var live []int
	for _, i := range validatorDestIdx(topo, targetSite) {
		if i < len(intents) && !intents[i].Cordoned {
			live = append(live, i)
		}
	}
	if len(live) < 2 {
		return // 0 or 1 validator destination live — nothing to equalize
	}

	// Probe heights while the nodes still run as trackers; pick the most advanced as source.
	res := c.checkHealth(intents)
	src := -1
	for _, i := range live {
		if i < len(res) && res[i].state == healthServing && (src < 0 || res[i].block > res[src].block) {
			src = i
		}
	}
	if src < 0 {
		fmt.Println("failover: no SERVING validator destination to clone from — skipping DB equalize; verify quorum via status.sh.")
		return
	}

	// CRITICAL: stop EVERY validator destination now — including ones already at the tip —
	// so none keeps following the old site and drifts ahead before the cert swap. This is
	// what prevents the post-clone re-split that previously deadlocked the failover.
	for _, i := range live {
		c.killNode(c.nodeIPs[i])
	}

	srcBlock := res[src].block
	fmt.Printf("== failover: equalizing %d-validator set on site %s to %s @ block %d (stop-all -> clone -> promote) ==\n",
		len(live), siteName(targetSite), topo.MachineName(src), srcBlock)
	_ = os.Remove(snapshotTar)
	if !c.snapshotPull(c.nodeIPs[src], snapshotTar) {
		fmt.Printf("failover: WARNING snapshot of %s failed — reconcile will start the set as-is; it may deadlock until nodes re-sync (check status.sh).\n", topo.MachineName(src))
		return
	}
	defer cleanupSnapshot(snapshotTar)

	for _, i := range live {
		if i == src {
			continue
		}
		fmt.Printf("  %s: wipe + seed from %s's DB (was @%d)\n", topo.MachineName(i), topo.MachineName(src), res[i].block)
		c.loadSnapshot(c.nodeIPs[i], snapshotTar)
	}
	// All validator destinations are left stopped on the identical DB; the reconcile pass
	// swaps their validator key in and starts them, so the promoted set agrees on the tip.
	fmt.Printf("  validator set equalized at block %d (%s); reconcile will promote them consistent.\n",
		srcBlock, humanSize(snapshotTar))
}
