package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Hard-site-failover quorum reconciliation.
//
// A hard `site-failover` assumes the old site is GONE (nuked) and cuts the whole
// validator set over to the surviving site in one shot. The hazard: the surviving
// site's machines are trackers that may sit at WILDLY different heights — one that
// kept up next to others that wedged tens of thousands of blocks back (cross-region
// trackers don't self-heal under load; see standby lag notes). Promote them as-is and
// consensus can't form a quorum on ANY tip — the ahead node can't get votes for a block
// the others lack, the behind nodes can't vote on blocks they don't have — so the chain
// DEADLOCKS at the failover even though every node reports "SERVING" (measured 2026-06-22:
// site B promoted at heights 659k/426k/412k produced zero new blocks).
//
// With the old site gone there is no external canonical reference, so the most-advanced
// surviving node IS canonical by definition. reconcileBackupHeights clones its DB onto
// every surviving node more than failoverConsistencyTolerance blocks behind it — the
// validators AND the pinned RPC and spare, not just the validators:
//   - validators must match or no quorum forms and the chain deadlocks;
//   - the RPC must be at the tip too, or it serves ingress on stale state — the
//     failed-over load generator then gets stale-nonce rejections and the chain starves
//     with an empty mempool even though consensus is healthy (observed 2026-06-22);
//   - the spare should be current so it is a ready hot-spare / snapshot source.
// Non-validators "catch up on their own" only reliably same-region under light load — the
// very assumption that bites under failover load — so we make the WHOLE site consistent
// in one deterministic step. The chain resumes from the highest height; blocks the old
// site finalized above it are the unavoidable RPO of losing it. A no-op when the surviving
// nodes are already consistent (a genuine synced hot standby), so it's free in the happy path.

// failoverConsistencyTolerance is the max height spread allowed among the promoted
// validators before we force a DB clone. Within this, ordinary consensus reconciles
// them in a few blocks; beyond it (especially under load, where a behind node wedges
// instead of catching up) the chain deadlocks. Defaults to syncToleranceBlocks; override
// with FAILOVER_CONSISTENCY_TOLERANCE.
const failoverConsistencyTolerance = syncToleranceBlocks

func failoverConsistencyToleranceBlocks() uint64 {
	if v := os.Getenv("FAILOVER_CONSISTENCY_TOLERANCE"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return failoverConsistencyTolerance
}

// pickCloneSource selects the most-advanced SERVING validator as the clone source and
// returns the indices of validators that must be re-seeded from it: those more than tol
// blocks behind, or not serving at all. ok=false when no validator is serving (no source
// to clone from). Indices are positions in the parallel blocks/serving slices. Pure —
// unit-tested; the I/O wrapper reconcileBackupHeights reads these values over RPC.
func pickCloneSource(blocks []uint64, serving []bool, tol uint64) (srcIdx int, laggards []int, ok bool) {
	srcIdx = -1
	for i := range blocks {
		if serving[i] && (srcIdx < 0 || blocks[i] > blocks[srcIdx]) {
			srcIdx = i
		}
	}
	if srcIdx < 0 {
		return -1, nil, false
	}
	for i := range blocks {
		if i == srcIdx {
			continue
		}
		if !serving[i] || blocks[srcIdx] > blocks[i]+tol {
			laggards = append(laggards, i)
		}
	}
	return srcIdx, laggards, true
}

// reconcileBackupHeights makes a hard site-failover leave a WORKING quorum: it clones the
// most-advanced surviving validator's DB onto any promoted validator that is too far
// behind to form consensus with it. Runs BEFORE the reconcile pass starts the nodes, so
// they boot on the cloned (consistent) data; the clone is identity-agnostic (it copies
// data/validator only — staking credentials live outside it), so each node keeps the
// validator key it is being promoted to. A no-op when the validators are already
// consistent. Best-effort: on any probe/snapshot failure it logs and leaves the set as-is
// (the operator can see a stuck chain via status) rather than aborting the failover.
func (c *config) reconcileBackupHeights(intents []MachineIntent, targetSite int) {
	topo := c.topo

	// Every machine that will be live on the surviving (target) site — validators AND the
	// spare + pinned RPC. We reconcile them ALL (see package doc): validators so a quorum
	// forms, the RPC so it serves ingress at the tip rather than on stale state, the spare
	// so it is a ready hot-spare / snapshot source.
	var survIdx []int
	for i, in := range intents {
		if topo.Site(i) == targetSite && !in.Cordoned {
			survIdx = append(survIdx, i)
		}
	}
	if len(survIdx) < 2 {
		return // 0 or 1 live machine on the site — nothing to reconcile against
	}

	// Probe current heights — the machines are still running (as trackers) at this point.
	res := c.checkHealth(intents)
	blocks := make([]uint64, len(survIdx))
	serving := make([]bool, len(survIdx))
	for j, i := range survIdx {
		if i < len(res) {
			blocks[j] = res[i].block
			serving[j] = res[i].state == healthServing
		}
	}

	tol := failoverConsistencyToleranceBlocks()
	srcPos, lagPos, ok := pickCloneSource(blocks, serving, tol)
	if !ok {
		fmt.Println("failover: no SERVING node on the surviving site to clone from — skipping DB reconcile; verify the chain forms a quorum via status.sh.")
		return
	}
	highIdx := survIdx[srcPos]

	// Never clone a pruned source onto an archive node — it would strip the RPC's full
	// history, leaving it archive-in-name-only. If the highest (source) node isn't itself
	// archive, drop any archive laggards: they keep their history and catch up on their own
	// (or get reseeded from the other site's archive RPC on a graceful restore).
	if !c.isArchiveNode(highIdx) {
		var kept []int
		for _, p := range lagPos {
			if c.isArchiveNode(survIdx[p]) {
				fmt.Printf("  %s: archive node is %d behind but clone source %s is pruned — NOT cloning (would lose history); it will catch up on its own.\n",
					topo.MachineName(survIdx[p]), blocks[p], topo.MachineName(highIdx))
				continue
			}
			kept = append(kept, p)
		}
		lagPos = kept
	}

	if len(lagPos) == 0 {
		fmt.Printf("failover: surviving site already consistent (within %d blocks of %s @ %d) — no DB clone needed.\n",
			tol, topo.MachineName(highIdx), blocks[srcPos])
		return
	}

	lagNames := make([]string, 0, len(lagPos))
	for _, p := range lagPos {
		lagNames = append(lagNames, fmt.Sprintf("%s(@%d)", topo.MachineName(survIdx[p]), blocks[p]))
	}
	fmt.Printf("== failover: surviving site diverged — cloning %s @ block %d onto [%s] so the chain (and ingress) resume consistent ==\n",
		topo.MachineName(highIdx), blocks[srcPos], strings.Join(lagNames, " "))

	srcHost := c.nodeIPs[highIdx]
	_ = os.Remove(snapshotTar)
	c.killNode(srcHost) // stop for a consistent on-disk image
	if !c.snapshotPull(srcHost, snapshotTar) {
		fmt.Printf("failover: WARNING snapshot of %s failed — leaving the site as-is; the chain may deadlock or starve (check status.sh).\n", topo.MachineName(highIdx))
		return
	}
	defer cleanupSnapshot(snapshotTar)

	for _, p := range lagPos {
		i := survIdx[p]
		fmt.Printf("  %s: stop + wipe + seed from %s's DB\n", topo.MachineName(i), topo.MachineName(highIdx))
		c.loadSnapshot(c.nodeIPs[i], snapshotTar)
	}
	// Source and laggards are left stopped; the reconcile pass starts every surviving
	// machine (validators with their key swapped in) on the now-identical DB.
	fmt.Printf("  cloned %s (%s) onto %d behind node(s) — reconcile will start the site consistent.\n",
		topo.MachineName(highIdx), humanSize(snapshotTar), len(lagPos))
}
