package main

import (
	"fmt"
	"os"
	"time"
)

// failoverSettleTimeout bounds how long a failover waits for the surviving site to finish
// recovering before it picks a clone source (see waitForSiteSettled). Short, because the
// common case is a node restart that serves within seconds; the bound keeps a genuinely
// long bootstrap from stalling the cutover.
const failoverSettleTimeout = 90 * time.Second

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
// stopping every validator destination up front and cloning the most-advanced SERVING node's
// DB onto them — and that source may be ANY role on the surviving site, including an archive
// RPC, since the standby RPCs commonly retain a higher tip than the standby validators. The
// chain resumes from the highest surviving height; blocks the old site finalized above that
// (and never delivered to the surviving site) are the unavoidable RPO of losing it.

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
//   2. picks the most-advanced SERVING node on the surviving site (ANY role — validator,
//      spare, or archive RPC — whichever retained the highest tip, verified on-branch);
//   3. clones its DB (identity-agnostic: data/validator only, staking creds live outside it)
//      onto ALL the validator destinations, unconditionally.
// So every promoted validator boots byte-identical AT THE HIGHEST RETAINED TIP, a quorum
// forms immediately, and there is no forward-replay stall (the failure mode that motivated
// sourcing from any role: a laggard-validator clone forces the set to re-execute up to the
// tip an RPC already held — minutes of zero mining).
//
// The validator destinations are the only REQUIRED writes (quorum). Two best-effort writes
// follow, neither of which can strip an archive RPC's history: the hot-standby spare is
// seeded so it is instantly promotable, and a far-behind archive RPC is reseeded ONLY when
// the clone source is itself archive (an archive->archive copy preserves full history). A
// pruned source never touches an archive RPC — that is the hazard the graceful-restore path
// guards, and gating the RPC reseed on an archive source keeps it impossible here. A
// non-validator (RPC/spare) may be the read source too — copied stopped, then restarted so
// RPC ingress resumes. Runs BEFORE the reconcile pass starts the nodes. Best-effort: on a
// snapshot failure it logs and lets reconcile start the set as-is rather than aborting.
func (c *config) reconcileBackupHeights(intents []MachineIntent, targetSite int) {
	topo := c.topo

	// The quorum-critical set: the machines that will hold validator keys on the surviving
	// site. These — and ONLY these — are the clone DESTINATIONS; they must boot at an
	// identical height for a quorum to form. RPCs are never written to here (they are
	// reseeded only by a graceful restore), so this can never strip an archive RPC's history.
	var live []int
	for _, i := range validatorDestIdx(topo, targetSite) {
		if i < len(intents) && !intents[i].Cordoned {
			live = append(live, i)
		}
	}
	if len(live) < 2 {
		return // 0 or 1 validator destination live — nothing to equalize
	}

	// Give the surviving site a bounded window to finish recovering before choosing the clone
	// source. A failover fired right after a restore can catch the site's archive RPCs still
	// coming up — not yet SERVING, or SERVING but still climbing to their retained tip. Picking
	// the source then would miss the at-tip RPC and fall back to a laggard validator, forcing
	// the multi-thousand-block replay stall this whole routine exists to avoid.
	res := c.waitForSiteSettled(intents, targetSite, failoverSettleTimeout)

	// Clone SOURCE: the most-advanced SERVING node on the surviving site across ALL roles —
	// validators, spare, AND archive RPCs. The standby RPCs routinely track the dying active
	// site FURTHER than the standby validators (they do no consensus work and are not stopped
	// for equalization), so they hold the true retained tip. Cloning the highest such node —
	// instead of the highest validator — boots the promoted set at the tip, so the quorum
	// forms there immediately and there is NO multi-thousand-block re-execution stall while
	// the validators replay forward to the tip the RPC already had (measured 2026-06-23:
	// validators cloned to a laggard @99679 re-executed up to 103065, ~4 minutes of zero
	// mining while the RPC sat at the tip the whole time).
	//
	// Using an archive RPC as the source is safe: its full-history DB is a SUPERSET of a
	// pruned validator DB, so a validator booting on it just carries extra history (its
	// pruning/state-sync config only governs go-forward behavior, and state-sync never
	// re-triggers on a DB already at the tip). The dangerous direction — a pruned DB landing
	// on an archive RPC and stripping its history — never happens here: RPCs are sources,
	// never destinations.
	src, bestVal := -1, -1
	for i := 0; i < topo.Size(); i++ {
		if topo.Site(i) != targetSite || i >= len(intents) || intents[i].Cordoned {
			continue
		}
		if i >= len(res) || res[i].state != healthServing {
			continue
		}
		if src < 0 || res[i].block > res[src].block {
			src = i
		}
		if containsInt(live, i) && (bestVal < 0 || res[i].block > res[bestVal].block) {
			bestVal = i // highest SERVING validator destination (on-branch by construction)
		}
	}
	if src < 0 {
		fmt.Println("failover: no SERVING node on the surviving site to clone from — skipping DB equalize; verify quorum via status.sh.")
		return
	}

	// Branch safety: if the most-advanced node is NOT a validator destination (e.g. an
	// archive RPC), confirm it is on the validators' branch before cloning it site-wide — a
	// node wedged on a stale fork also reports SERVING, and cloning it would import a
	// last-accepted the set never had (see snapshotSourceCanonical). Compare it to the
	// highest SERVING validator destination; on any mismatch or unreadable reference, fall
	// back to that destination (always on-branch).
	if !containsInt(live, src) {
		if bestVal < 0 {
			fmt.Println("failover: no SERVING validator destination to validate the clone source against — skipping DB equalize.")
			return
		}
		if !c.onSameBranch(c.nodeIPs[src], c.nodeIPs[bestVal]) {
			fmt.Printf("failover: most-advanced node %s is off the validator branch — falling back to highest validator %s.\n",
				topo.MachineName(src), topo.MachineName(bestVal))
			src = bestVal
		}
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

	// If the source is itself a validator destination it is already stopped (above) and stays
	// stopped for the reconcile pass to start. If it is a non-destination (archive RPC /
	// spare) it must be stopped for a consistent on-disk image, then restarted immediately so
	// it keeps serving (e.g. RPC ingress) while the clone lands on the validators.
	srcIsDest := containsInt(live, src)
	if !srcIsDest {
		c.killNode(c.nodeIPs[src])
	}
	if !c.snapshotPull(c.nodeIPs[src], snapshotTar) {
		if !srcIsDest {
			c.start(c.nodeIPs[src], c.nodeIPs[src]) // best-effort restart even if the copy failed
		}
		fmt.Printf("failover: WARNING snapshot of %s failed — reconcile will start the set as-is; it may deadlock until nodes re-sync (check status.sh).\n", topo.MachineName(src))
		return
	}
	if !srcIsDest {
		c.start(c.nodeIPs[src], c.nodeIPs[src]) // source downtime is just the copy
	}
	defer cleanupSnapshot(snapshotTar)

	for _, i := range live {
		if i == src {
			continue
		}
		fmt.Printf("  %s: wipe + seed from %s's DB (was @%d)\n", topo.MachineName(i), topo.MachineName(src), res[i].block)
		c.loadSnapshot(c.nodeIPs[i], snapshotTar)
	}

	// Best-effort: also seed the hot-standby spare from the SAME snapshot so it boots at the
	// tip and is instantly promotable if a validator later fails. Left to sync on its own it
	// could sit behind for a while, and promoting a behind spare would re-introduce the very
	// lag/quorum hazard this function exists to prevent. The spare holds no validator key, so
	// it never gates the quorum — a skipped/failed seed here just leaves it to catch up
	// normally and never blocks the failover. Skip it if it is already at the source tip (the
	// hot-standby-site no-op case) or is itself the clone source.
	if sp := spareDestIdx(topo, targetSite); sp >= 0 && sp != src && sp < len(intents) && !intents[sp].Cordoned {
		behind := sp >= len(res) || res[sp].state != healthServing || res[sp].block < srcBlock
		if behind {
			was := "down"
			if sp < len(res) && res[sp].state == healthServing {
				was = fmt.Sprintf("@%d", res[sp].block)
			}
			fmt.Printf("  %s (spare): wipe + seed from %s's DB so it is a hot standby at the tip (was %s)\n",
				topo.MachineName(sp), topo.MachineName(src), was)
			c.loadSnapshot(c.nodeIPs[sp], snapshotTar)
		}
	}

	// Best-effort: if the clone source is itself an ARCHIVE node, reseed any far-behind
	// archive RPC on the surviving site from the same snapshot. This is gated on the source
	// being archive precisely because an archive->archive clone preserves full history — the
	// hazard the graceful-restore path guards against is a PRUNED source stripping an archive
	// RPC's history, which cannot happen when the source is archive. It saves a recovering
	// RPC from a multi-hour from-genesis bootstrap (state-sync is disabled on archive nodes)
	// and returns it to ingress rotation at the tip. Only reseed an RPC that is down or more
	// than snapshotSourceMaxLag behind — a near-tip RPC catches up faster by delta-replay than
	// by a full DB push. RPCs carry no vote, so a skipped/failed reseed never blocks failover.
	if c.isArchiveNode(src) {
		for _, i := range rpcMachineIdxs(topo, targetSite) {
			if i == src || i >= len(intents) || intents[i].Cordoned {
				continue
			}
			serving := i < len(res) && res[i].state == healthServing
			behind := !serving || srcBlock > res[i].block+snapshotSourceMaxLag
			if !behind {
				continue
			}
			was := "down"
			if serving {
				was = fmt.Sprintf("@%d", res[i].block)
			}
			fmt.Printf("  %s (archive RPC): wipe + seed from archive source %s's DB, skipping a from-genesis bootstrap (was %s)\n",
				topo.MachineName(i), topo.MachineName(src), was)
			c.loadSnapshot(c.nodeIPs[i], snapshotTar)
		}
	}

	// All validator destinations are left stopped on the identical DB; the reconcile pass
	// swaps their validator key in and starts them, so the promoted set agrees on the tip.
	fmt.Printf("  validator set equalized at block %d (%s); reconcile will promote them consistent.\n",
		srcBlock, humanSize(snapshotTar))
}

// waitForSiteSettled gives the surviving site a bounded window to finish recovering before a
// failover selects its clone source, then returns the settled health snapshot. A failover
// fired right after a restore can catch the site's archive RPCs still bootstrapping: not yet
// SERVING, or SERVING but still climbing to their retained tip. If the source were chosen in
// that instant the at-tip RPC would be invisible and the set would be cloned from a laggard
// validator — forcing a multi-thousand-block replay stall (measured 2026-06-23: validators
// cloned @177884 while the RPCs held 184552, then froze re-executing ~6.6k fat blocks).
//
// In a hard failover the old site is gone, so no new blocks are minted and the surviving
// nodes' heights converge to the highest retained tip. We poll until that max height holds
// steady across two consecutive reads (every node done climbing) with at least two nodes
// serving, then return — or return the latest snapshot once the timeout elapses. Best-effort:
// a node that never serves simply isn't a candidate, exactly as before; the wait only ensures
// a quickly-restarting at-tip node is given the chance to reappear.
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

// onSameBranch reports whether two nodes share the same block hash at a recent common
// height — i.e. they are on the same (canonical) chain, not divergent forks. Used to
// confirm a non-validator clone source (e.g. an archive RPC that ran ahead) is on the
// validators' branch before its DB is cloned site-wide. Conservative: any unreadable
// tip/hash returns false so the caller falls back to a known on-branch source.
func (c *config) onSameBranch(ipA, ipB string) bool {
	tipA, _, okA := c.blockAt(ipA, "finalized")
	tipB, _, okB := c.blockAt(ipB, "finalized")
	if !okA || !okB {
		return false
	}
	tag := fmt.Sprintf("0x%x", commonHeight(tipA, tipB))
	_, hashA, okHA := c.blockAt(ipA, tag)
	_, hashB, okHB := c.blockAt(ipB, tag)
	return okHA && okHB && hashA != "" && hashA == hashB
}

// containsInt reports whether s contains v.
func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
