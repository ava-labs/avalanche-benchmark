package main

// Graceful rolling restore (two-site mode). Where `site-failover` is a hard
// cutover (cordon a whole site, swap the validator set across in one shot),
// `restore` moves the set back one validator at a time while both DCs are
// healthy: seed the target site's machines with a fresh copy of the live chain,
// bring them up as trackers and wait until they reach the live tip, then migrate
// v1, v2, v3 in sequence with a health gate between each. Because the chain never
// drops below 2/3 and each promoted node is already at tip (so it continues the
// live branch), there is no chain downtime and no fork.
//
// Seeding defaults to a DB SNAPSHOT of the live source site (see snapshot.go): the
// target replays only the tiny delta since the snapshot instead of pulling the
// whole state trie from loaded peers — the fragile, memory-heavy state-sync that
// wedged recovering nodes under sustained ingress. Set RESTORE_MODE=state-sync to
// force the from-scratch path (also the automatic fallback when no snapshot source
// is available). Either way the target's stale data is discarded first, so it can
// never resurrect the divergent post-failover frontier (the rollback/fork hazard).
//
// The spare and pinned RPC are seeded and started alongside the validators in Phase 1, but the
// rollover is gated ONLY on the validator destinations reaching tip — the spare and RPC hold no
// vote, so they finish catching up on their own and never block (or stall) the failback. This is
// what keeps a from-scratch RPC seed (e.g. RESTORE_MODE=state-sync) from holding consensus hostage
// to a genesis bootstrap. The restore self-verifies (single branch + quorum) at the end.
// See docs/two-site-failover.md.

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// syncToleranceBlocks: a target tracker is "synced enough" to take a
	// validator key when within this many blocks of the active tip. Its data/
	// is preserved across the key swap, and avalanchego self-heals a gap this
	// small via normal bootstrap/consensus gossip in well under a second once
	// promoted. Only a tracker further behind than this is wiped + state-synced
	// (the external fix for the inconsistent-height deadlock — see
	// reconcileBackupHeights). Set generously: a tight hot-standby never trips it.
	syncToleranceBlocks = 100
	restorePollInterval = 3 * time.Second

	// consensusReadyMinConnected: a validator must be connected to at least this
	// fraction of the L1 validator stake (AvalancheGo /ext/health percentConnected)
	// before it counts as ready to vote. ~1.0 == the whole set; we require
	// effectively all of it so the next key never moves until the just-promoted
	// validator has rejoined the consensus mesh — not merely answered eth_blockNumber.
	consensusReadyMinConnected = 0.999
	// consensusMaxProcessing: if the oldest in-flight block has been undecided for
	// longer than this, consensus is stalled (not just slow) — treat as not-ready.
	consensusMaxProcessing = 2 * time.Second
	// idleReadyPollsNeeded: once every validator is fully ready (100% connected, no
	// stuck block) the between-swap gate normally waits for the accepted tip to
	// advance as live proof that production resumed. But an IDLE chain (no load to
	// build blocks from) produces nothing, so the tip stays flat even though
	// consensus is perfectly healthy — which would hang the gate forever (seen on a
	// restore that finished after the bombard stopped). A *limping* chain is
	// distinguishable: it shows a stuck block or <100% connected, which keeps
	// ready<want. So after this many consecutive fully-ready polls with a flat tip,
	// treat the chain as idle-but-healthy and proceed. ~3 polls (≈9s).
	idleReadyPollsNeeded = 3
)

func siteName(site int) string {
	if site == siteB {
		return "b"
	}
	return "a"
}

func siteBase(topo Topology, site int) int {
	if site == siteB {
		return topo.sitePool()
	}
	return 0
}

func cloneIntents(in []MachineIntent) []MachineIntent {
	out := make([]MachineIntent, len(in))
	copy(out, in)
	return out
}

// validatorKeysInSite counts validator identities assigned to machines in a
// site. liveOnly excludes cordoned machines.
func validatorKeysInSite(intents []MachineIntent, topo Topology, site int, liveOnly bool) int {
	n := 0
	for i, in := range intents {
		if topo.Site(i) != site || !topo.isValidatorKey(in.Key) {
			continue
		}
		if liveOnly && in.Cordoned {
			continue
		}
		n++
	}
	return n
}

// validatorMachineIdx returns the pool indexes of uncordoned machines holding a
// validator key (the currently-active validator set).
func validatorMachineIdx(topo Topology, intents []MachineIntent) []int {
	var idx []int
	for i, in := range intents {
		if !in.Cordoned && topo.isValidatorKey(in.Key) {
			idx = append(idx, i)
		}
	}
	return idx
}

// RestorePlan computes the ordered intention snapshots for a graceful rolling
// migration of the validator set onto targetSite. It returns:
//   - trackerStep: every target-site machine uncordoned (validator keys stay on
//     the currently-active site; nothing votes on the target yet).
//   - keySteps: one snapshot per validator key, each moving exactly one key onto
//     a target-site machine and releasing its previous holder to its tracker home.
//
// Applied in order with a health gate between snapshots (target synced to tip
// before the first key move; full validator set serving before each subsequent
// move), the chain holds >=2/3 throughout and the end state equals the steady
// seed with targetSite active. Pure function — caller does the I/O.
func RestorePlan(topo Topology, prev []MachineIntent, targetSite int) (trackerStep []MachineIntent, keySteps [][]MachineIntent) {
	base := siteBase(topo, targetSite)

	// Step 0: uncordon the target site. Keys are left untouched — after a
	// site-failover the target machines already wear their tracker home keys (and
	// rollingRestore's pre-condition guarantees no validator key sits on the target
	// site), so uncordoning brings them up as trackers without granting any vote.
	trackerStep = cloneIntents(prev)
	for i := range trackerStep {
		if topo.Site(i) == targetSite {
			trackerStep[i].Cordoned = false
		}
	}

	// Steps 1..N: move each validator key onto the target site, one at a time.
	cur := cloneIntents(trackerStep)
	for slot := 0; slot < topo.NVal; slot++ {
		k := l1KeyBase + slot
		dest := base + slot
		next := cloneIntents(cur)
		for i := range next {
			if next[i].Key == k {
				next[i].Key = topo.HomeKey(i) // release the previous holder to its tracker home
			}
		}
		next[dest].Cordoned = false
		next[dest].Key = k
		keySteps = append(keySteps, next)
		cur = next
	}
	return trackerStep, keySteps
}

// rollingRestore gracefully migrates the validator set onto targetSite one
// validator at a time, keeping the chain at >=2/3 serving throughout. Typical
// use: restore the original site after a site-failover, with both DCs healthy.
func rollingRestore(cfg *config, targetSite int) {
	topo := cfg.topo
	if !topo.TwoSite {
		fatalf("restore requires two-site mode (set BACKUP_SITE_NODE_IPS)")
	}
	prev, err := loadIntents(cfg.stateFile, topo)
	if err != nil {
		fatalf("%v", err)
	}

	want := topo.NVal
	if validatorKeysInSite(prev, topo, targetSite, true) == want {
		fmt.Printf("site %s already holds the full validator set — nothing to restore.\n", siteName(targetSite))
		return
	}
	if n := validatorKeysInSite(prev, topo, targetSite, false); n > 0 {
		fatalf("site %s holds %d validator key(s) in an unexpected (partial) state; run `reconcile status` and resolve before restore", siteName(targetSite), n)
	}

	trackerStep, keySteps := RestorePlan(topo, prev, targetSite)
	destIdx := validatorDestIdx(topo, targetSite) // machines that will receive a validator key (gate only these)

	rpcIdxs := rpcMachineIdxs(topo, targetSite)
	srcIdx := validatorMachineIdx(topo, trackerStep) // active (source) validators = the producers + tip reference

	// No block-production throttle here: the backup site (B) is configured to produce at
	// backupSiteMinDelayMS (~10 blk/s) whenever it holds the validator set — see deployChainConfig.
	// So during a B->A restore the source is already producing slowly enough for the recovering
	// nodes to out-pace it, with no mid-restore rolling restart to apply or undo.

	// Recovery strategy, by role — both seeded from a fresh DB SNAPSHOT of the live source site
	// (captured AT the tip), so each recovering node replays only a tiny delta. State-sync was
	// tried here and could NOT keep up: a graceful restore runs against the STILL-ACTIVE other
	// site, so the tip keeps moving and the recovering validators never close the gap under
	// load (measured: gap grew all night at 4000 TPS). A snapshot is a point-in-time copy at the
	// tip, so the delta to replay is seconds, not a commit-interval pivot gap. Role-matched so
	// each node's DB pruning-mode matches its role-default config (applied by prepareTarget) —
	// safe now that FAILOVER keeps validators PRUNED (state-sync), so the source site's spare is
	// genuinely a pruned tracker:
	//   - validators + spare  <- a PRUNED snapshot of the source site's spare tracker;
	//   - pinned RPCs          <- a full ARCHIVE snapshot of the source site's REDUNDANT (last) RPC
	//                             (its twin keeps serving ingress, so no blip). Every site runs
	//                             >=2 RPCs (loadPool enforces it), so a redundant RPC always exists
	//                             — they may be co-located on one box; what matters is two processes.
	// Both snapshots are captured up front and EVERY target is seeded + started together in
	// Phase 1 — the RPCs are NOT held back behind validator sync, since a near-tip archive
	// snapshot needs no catch-up race. If a source is unavailable, that role falls back to
	// wipe + state-sync (validators) / from-genesis bootstrap (RPCs).
	// RESTORE_SKIP_SEED=1 resumes a restore whose Phase-1 seed already finished: skip
	// the slow, destructive snapshot + wipe and reuse the target site exactly as it is.
	// Safe only when the target is ALREADY up as in-sync trackers (e.g. a prior restore
	// that synced but was interrupted in the swap phase). The sync gate below still runs,
	// so a target that isn't actually at tip won't be promoted.
	skipSeed := os.Getenv("RESTORE_SKIP_SEED") == "1" || strings.EqualFold(os.Getenv("RESTORE_SKIP_SEED"), "true")
	forceStateSync := strings.EqualFold(os.Getenv("RESTORE_MODE"), "state-sync")
	var archiveTar string
	switch {
	case skipSeed:
		fmt.Printf("== restore: RESTORE_SKIP_SEED set — skipping snapshot + wipe; reusing site %s as-is (sync gate still verifies it is at tip) ==\n", siteName(targetSite))
	case forceStateSync:
		fmt.Println("== restore: RESTORE_MODE=state-sync — seeding every target from scratch (no snapshot); validators state-sync, RPCs full-bootstrap from genesis ==")
	default:
		// DEFAULT: validators + spare STATE-SYNC (wipe, no pruned snapshot); archive RPCs clone
		// an archive snapshot. Why the split: a pruned snapshot is captured at the START of the
		// restore (the source's height then), so a node seeded from it must replay FORWARD to the
		// live tip — through the failover/restore boundary, where a competing block at the swap
		// height can trap it (observed: a recovering validator oscillating between two blocks at one
		// height, "Resetting chain preference" repeatedly, never finalizing). State-sync instead
		// jumps the node straight to a recent CANONICAL state summary, skipping that forward replay.
		// (Trade-offs: under sustained ingress state-sync can be slow/heavy; and if the network has
		// no state summary yet — fewer than StateSyncCommitInterval blocks — it falls back to a
		// bootstrap replay, so this only helps once summaries exist.) Archive RPCs CANNOT state-sync
		// (they need full history), so they still clone an archive snapshot, taken only from a source
		// gated canonical (takeArchiveSnapshot) — a clone of an at-tip archive lands PAST the boundary.
		fmt.Println("== restore: seed mode — validators/spare STATE-SYNC, archive RPCs SNAPSHOT ==")
		if tar, ok := cfg.takeArchiveSnapshot(trackerStep, otherSite(targetSite)); ok {
			archiveTar = tar
			defer cleanupSnapshot(archiveTar)
		} else {
			fmt.Println("WARN: no usable archive snapshot source — recovering RPCs will full-bootstrap from genesis.")
		}
	}

	// Phase 1 — seed EVERY target-site node together (validators + spare via state-sync,
	// RPCs from the archive snapshot), bring them all up as trackers, and wait until
	// the validator destinations reach the tip. Each target's stale data is discarded first, so
	// it can never resurrect a divergent post-failover frontier — identities (staking/active,
	// staking/l1) live outside data/ and are preserved. See prepareTarget / wipeL1Data.
	fmt.Printf("== restore: prepare + bring site %s up as trackers ==\n", siteName(targetSite))
	if err := saveIntents(cfg.stateFile, trackerStep); err != nil {
		fatalf("%v", err)
	}
	printIntents(topo, trackerStep)

	if !skipSeed {
		// Seed every target CONCURRENTLY. Each is a separate stream to a different
		// host, so overlapping them both removes the sequential wait and fills the
		// high-latency cross-region link far better than one transfer at a time.
		var wg sync.WaitGroup
		for i := range trackerStep {
			if topo.Site(i) != targetSite || trackerStep[i].Cordoned {
				continue
			}
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				if cfg.isArchiveNode(idx) { // archive RPC: full-state archive clone (can't state-sync)
					cfg.prepareTarget(idx, archiveTar != "", archiveTar)
				} else { // validator / spare: state-sync (wipe, no snapshot) — see seed-mode note above
					cfg.prepareTarget(idx, false, "")
				}
			}(i)
		}
		wg.Wait()
	}

	reconcile(cfg, trackerStep, false)

	// Gate ONLY the validator destinations to tip before any key moves. A behind validator is
	// the fork risk — it must be at tip before it votes — but the spare and pinned RPC carry NO
	// vote during the rollover, so they must NOT block it (per validatorDestIdx's contract). This
	// matters most when the RPC is seeded from scratch (e.g. RESTORE_MODE=state-sync):
	// gating consensus failback on a genesis-bootstrapping RPC could stall it for hours, while the
	// other site's RPC keeps serving ingress the whole time. The spare and RPC keep syncing in the
	// background and are reported (not gated) once the set is back.
	fmt.Printf("waiting for site %s recovering validators to reach tip (within %d blocks)...\n", siteName(targetSite), syncToleranceBlocks)
	cfg.waitForTargetSynced(trackerStep, destIdx, srcIdx)

	// Phase 2 — roll each validator key onto the target site, one at a time. The spare + RPCs
	// came up (seeded) in Phase 1 and keep catching up in the background; they are never gated here.
	current := trackerStep
	for n, step := range keySteps {
		fmt.Printf("== restore: migrate validator %d/%d to site %s ==\n", n+1, len(keySteps), siteName(targetSite))
		cfg.waitForValidatorsReady(current, targetSite)
		if err := saveIntents(cfg.stateFile, step); err != nil {
			fatalf("%v", err)
		}
		printIntents(topo, step)
		reconcile(cfg, step, false)
		cfg.waitForValidatorsReady(step, targetSite)
		current = step
	}

	// Nothing to reset: site A produces at its 25ms config, site B keeps its
	// backupSiteMinDelayMS config (dormant while it tracks). No throttle was applied.

	fmt.Printf("\nrestore complete — validator set is active on site %s, chain held quorum throughout.\n", siteName(targetSite))

	// One-shot readout of the un-gated nodes (spare + RPC). They hold no vote, so they were never
	// gated; report where they are now so the operator knows whether the recovered site's standby
	// and ingress are at tip yet. A freshly-seeded RPC may still be well behind — it catches up on
	// its own and the other site's RPC keeps serving until it does.
	cfg.reportBackgroundSync(current, append(spareDestIdxs(topo, targetSite), rpcIdxs...), destIdx)

	fmt.Println()
	verifyAgreement(cfg)
}

// reportBackgroundSync prints a single height/gap snapshot for nodes that were not gated during
// the rollover (the spare and pinned RPC). It does not wait — these nodes hold no vote and finish
// syncing on their own; this is a courtesy readout, with refIdx (the now-active validators) as the
// tip reference.
func (c *config) reportBackgroundSync(intents []MachineIntent, idxs, refIdx []int) {
	if len(idxs) == 0 {
		return
	}
	res := c.checkHealth(intents)
	tip := tipBlock(res, refIdx)
	parts := make([]string, 0, len(idxs))
	for _, i := range idxs {
		name := c.topo.MachineName(i)
		switch {
		case i >= len(res):
			parts = append(parts, name+"=?")
		case res[i].state != healthServing:
			parts = append(parts, fmt.Sprintf("%s=%s", name, res[i].state))
		default:
			gap := 0
			if tip > res[i].block {
				gap = int(tip - res[i].block)
			}
			parts = append(parts, fmt.Sprintf("%s=%d(-%d)", name, res[i].block, gap))
		}
	}
	fmt.Printf("background sync (un-gated; catching up on their own): tip=%d  %s\n", tip, strings.Join(parts, "  "))
}

// validatorDestIdx returns the machine indexes that will receive validator keys
// when the set is rolled onto targetSite — the first NVal machines of that site
// (base+0..). These are the ONLY machines whose sync state gates promotion: a behind
// validator is the fork risk, so it must be at tip before it votes. The spares and
// pinned-RPC trackers never take a vote, so they finish syncing on their own and must
// not block (or deadlock) the restore.
func validatorDestIdx(topo Topology, targetSite int) []int {
	base := siteBase(topo, targetSite)
	idx := make([]int, topo.NVal)
	for i := range idx {
		idx[i] = base + i
	}
	return idx
}

// spareDestIdxs returns the indexes of the site's hot-standby spares — the NSpare slots
// immediately after the validator destinations (per-site layout [v..., spare..., rpc...]).
// A spare is a state-sync node (same DB profile as the validators, not archive) held ready
// to be promoted if a validator fails. Empty if the site has no spare slots.
func spareDestIdxs(topo Topology, targetSite int) []int {
	base := siteBase(topo, targetSite) + topo.NVal
	idx := make([]int, 0, topo.NSpare)
	for s := 0; s < topo.NSpare; s++ {
		idx = append(idx, base+s)
	}
	return idx
}

// rpcMachineIdxs returns the indexes of the pinned dedicated-RPC machines in the given
// site (their home identity is an RPC key), in ascending order — so [0] is the primary
// RPC and the last is the redundant RPC. Empty if the site has none.
func rpcMachineIdxs(topo Topology, site int) []int {
	var idx []int
	for i := 0; i < topo.Size(); i++ {
		if topo.Site(i) == site && topo.isRPCKey(topo.HomeKey(i)) {
			idx = append(idx, i)
		}
	}
	return idx
}

// waitForTargetSynced blocks until every recovering machine in destIdx is SERVING and within
// syncToleranceBlocks of the active tip (sampled from srcIdx). Each poll prints the tip and each
// target's height/gap so the operator can watch the catch-up; there is no timeout — it returns
// only once the whole set is at tip.
func (c *config) waitForTargetSynced(intents []MachineIntent, destIdx, srcIdx []int) {
	for {
		res := c.checkHealth(intents)
		tip := tipBlock(res, srcIdx)
		synced, worst := true, 0
		parts := make([]string, 0, len(destIdx))
		for _, i := range destIdx {
			name := c.topo.MachineName(i)
			if res[i].state != healthServing {
				synced = false
				parts = append(parts, fmt.Sprintf("%s=%s", name, res[i].state))
				continue
			}
			gap := 0
			if tip > res[i].block {
				gap = int(tip - res[i].block)
			}
			if gap > worst {
				worst = gap
			}
			if gap > syncToleranceBlocks {
				synced = false
			}
			parts = append(parts, fmt.Sprintf("%s=%d(-%d)", name, res[i].block, gap))
		}
		fmt.Printf("  sync: tip=%d  %s\n", tip, strings.Join(parts, "  "))
		if synced {
			fmt.Printf("  synced — all recovering nodes within %d blocks of tip (worst gap %d).\n", syncToleranceBlocks, worst)
			return
		}
		time.Sleep(restorePollInterval)
	}
}

// waitForFullValidatorSet blocks until every validator identity is SERVING. No timeout.
func (c *config) waitForFullValidatorSet(intents []MachineIntent) {
	want := c.topo.NVal
	for {
		res := c.checkHealth(intents)
		got := servingValidatorCount(c.topo, intents, res)
		if got >= want {
			fmt.Printf("  validators serving: %d/%d.\n", got, want)
			return
		}
		time.Sleep(restorePollInterval)
	}
}

// waitForValidatorsReady is the consensus-aware between-swap gate. Where
// waitForFullValidatorSet only checked that each validator answers eth_blockNumber
// (true within seconds of a restart, long before it rejoins consensus — which let
// the rolling restore fire the next swap while the chain was still limping from the
// last one, compounding the dips into a deep stall), this waits until consensus is
// genuinely healthy. A validator counts as ready when it is:
//   - SERVING (answers eth_blockNumber), and
//   - connected to ~100% of the validator stake (/ext/health percentConnected), and
//   - no block stuck in consensus (longestProcessingBlock < consensusMaxProcessing).
//
// The gate opens when (a) every validator that has LANDED on targetSite is ready —
// so a just-promoted node has rejoined before the next swap fires — and (b) the ready
// set meets quorum. It deliberately does NOT require the OUTGOING source-site
// validators to be ready: their vote is about to move, so their health can't affect
// the next swap's safety, and gating on them DEADLOCKS the rollover when the last
// source validator can't keep pace with the now-fast target site (it drifts behind /
// wedges and never reports ready — yet the swap onto its at-tip destination is exactly
// what would relieve it). Quorum and fork-safety depend on the remaining validators
// plus the at-tip destination, not on the validator that is leaving.
//
// It then prefers live proof that production resumed (the accepted tip advanced since
// the previous poll). But it does NOT require it: an idle chain with no load produces
// no blocks, so once the gate-open state holds flat for idleReadyPollsNeeded
// consecutive polls it proceeds anyway (see that const) — otherwise a restore that
// finishes while the chain is quiescent hangs forever.
//
// No timeout — like the other restore gates it waits as long as recovery takes.
func (c *config) waitForValidatorsReady(intents []MachineIntent, targetSite int) {
	want := c.topo.NVal
	quorum := quorumNeeded(want)
	var prevTip uint64
	havePrev := false
	idleReadyPolls := 0
	for {
		res := c.checkHealth(intents)
		ready, targetCatchingUp := 0, 0
		var tip uint64
		parts := make([]string, 0, want)
		for i, in := range intents {
			if in.Cordoned || !c.topo.isValidatorKey(in.Key) || i >= len(res) {
				continue
			}
			name := c.topo.MachineName(i)
			onTarget := c.topo.Site(i) == targetSite
			isReady := false
			if res[i].state != healthServing {
				parts = append(parts, name+" down")
			} else {
				pc, longest, h, ok := c.consensusHealth(i)
				if h > tip {
					tip = h
				}
				switch {
				case !ok:
					parts = append(parts, name+" ?")
				case pc >= consensusReadyMinConnected && longest < consensusMaxProcessing:
					isReady = true
					parts = append(parts, fmt.Sprintf("%s %.0f%%", name, pc*100))
				case longest >= consensusMaxProcessing:
					parts = append(parts, fmt.Sprintf("%s %.0f%% (block stuck)", name, pc*100))
				default:
					parts = append(parts, fmt.Sprintf("%s %.0f%%", name, pc*100))
				}
			}
			switch {
			case isReady:
				ready++
			case onTarget:
				// A validator that has LANDED on the target site but hasn't rejoined —
				// must wait for it (else we compound swaps). An OUTGOING source-site
				// holder that isn't ready does NOT block: its key is about to move.
				targetCatchingUp++
			}
		}
		advancing := havePrev && tip > prevTip
		motion := "flat"
		if advancing {
			motion = "rising"
		}
		fmt.Printf("  gate: %d/%d ready (quorum %d; %d landed catching up) | tip %d %s | %s\n",
			ready, want, quorum, targetCatchingUp, tip, motion, strings.Join(parts, ", "))
		// Open when every landed target validator is in consensus AND the ready set
		// meets quorum — NOT when all NVal are ready (outgoing source validators need
		// not be, see above).
		if targetCatchingUp == 0 && ready >= quorum {
			if advancing {
				fmt.Printf("  gate: quorum healthy (%d ready), tip rising — proceeding to next swap.\n", ready)
				return
			}
			// Gate-open but the tip is flat: the chain is idle, not limping. Proceed
			// once that has held for a few polls rather than wait for a block only
			// load would produce.
			idleReadyPolls++
			if idleReadyPolls >= idleReadyPollsNeeded {
				fmt.Printf("  gate: quorum healthy (%d ready), tip flat but consensus healthy (idle — no load) — proceeding to next swap.\n", ready)
				return
			}
		} else {
			idleReadyPolls = 0
		}
		prevTip, havePrev = tip, true
		time.Sleep(restorePollInterval)
	}
}

// servingValidatorCount counts distinct validator identities SERVING under the
// given intents + health snapshot.
func servingValidatorCount(topo Topology, intents []MachineIntent, results []healthResult) int {
	n := 0
	for i, in := range intents {
		if i >= len(results) {
			break
		}
		if !in.Cordoned && topo.isValidatorKey(in.Key) && results[i].state == healthServing {
			n++
		}
	}
	return n
}

// tipBlock returns the highest block height observed among the given machine
// indexes.
func tipBlock(results []healthResult, idxs []int) uint64 {
	var max uint64
	for _, i := range idxs {
		if i < len(results) && results[i].block > max {
			max = results[i].block
		}
	}
	return max
}
