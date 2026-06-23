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
// The pinned RPC is held back until the target site holds the validator MAJORITY
// (after the 2nd validator lands), then brought up + seeded so it catches up from
// SAME-SITE validators rather than racing a cross-region sync (off the still-active
// remote site) while also carrying ingress load. It is gated to tip before
// completion, and the restore self-verifies (single branch + quorum) at the end.
// See docs/two-site-failover.md.

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// syncToleranceBlocks: a target tracker is "synced enough" to take a
	// validator key when within this many blocks of the active tip. Its data/
	// is preserved across the key swap, so it closes a small residual gap in
	// well under a second once promoted.
	syncToleranceBlocks = 30
	restorePollInterval = 3 * time.Second
	restoreSyncTimeout  = 30 * time.Minute // generous: replaying a heavy post-failover backlog can take minutes (benign wait, logged each poll)
	restoreSetTimeout   = 5 * time.Minute
	restoreRPCTimeout   = 6 * time.Minute // bounded gate for the late pinned-RPC catch-up; warn (don't block) if it overruns
)

func siteName(site int) string {
	if site == siteB {
		return "b"
	}
	return "a"
}

func siteBase(site int) int {
	if site == siteB {
		return sitePoolSize
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
		if topo.Site(i) != site || !isValidatorKey(in.Key) {
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
func validatorMachineIdx(intents []MachineIntent) []int {
	var idx []int
	for i, in := range intents {
		if !in.Cordoned && isValidatorKey(in.Key) {
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
	base := siteBase(targetSite)

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
	for slot := 0; slot < len(validatorKeys()); slot++ {
		k := valKeyLo + slot
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

	want := len(validatorKeys())
	if validatorKeysInSite(prev, topo, targetSite, true) == want {
		fmt.Printf("site %s already holds the full validator set — nothing to restore.\n", siteName(targetSite))
		return
	}
	if n := validatorKeysInSite(prev, topo, targetSite, false); n > 0 {
		fatalf("site %s holds %d validator key(s) in an unexpected (partial) state; run `reconcile status` and resolve before restore", siteName(targetSite), n)
	}

	trackerStep, keySteps := RestorePlan(topo, prev, targetSite)
	destIdx := validatorDestIdx(topo, targetSite) // machines that will receive a validator key (gate only these)

	// Hold the pinned RPCs back until the target site holds the validator MAJORITY,
	// so they catch up from same-site validators instead of racing a cross-region sync
	// (off the still-active remote site) under ingress load (see package doc). rpcAfter
	// = majority count; the RPCs come up right after that many validators land.
	rpcIdxs := rpcMachineIdxs(topo, targetSite)
	rpcAfter := want/2 + 1
	delayRPC := len(rpcIdxs) > 0 && rpcAfter <= len(keySteps)
	if delayRPC {
		for _, ri := range rpcIdxs {
			trackerStep[ri].Cordoned = true
			for i := range keySteps {
				keySteps[i][ri].Cordoned = i < rpcAfter
			}
		}
	}
	srcIdx := validatorMachineIdx(trackerStep) // currently-active validators (the source site = the tip)

	// Choose the seeding strategy. Default: a DB snapshot of the live source site,
	// so targets replay only a tiny delta. RESTORE_MODE=state-sync forces the
	// from-scratch path; an unavailable snapshot source falls back to it too.
	snapMode := os.Getenv("RESTORE_MODE") != "state-sync"
	var snapTar string
	if snapMode {
		if tar, ok := cfg.takeSnapshot(trackerStep, otherSite(targetSite)); ok {
			snapTar = tar
			defer cleanupSnapshot(snapTar)
		} else {
			fmt.Println("WARN: no usable snapshot source — falling back to wipe + state-sync.")
			snapMode = false
		}
	}
	// The recovering site's pinned RPCs are ARCHIVE nodes (pruning + state-sync disabled,
	// see chain-config-rpc.json). They are NOT seeded from the pruning tracker snapshot — a
	// pruned DB would strip their history — nor left to bootstrap from genesis (which wedges
	// under load). Instead they are seeded from a full ARCHIVE snapshot of the SOURCE site's
	// REDUNDANT (2nd) RPC: restore stops that one RPC for a consistent copy while its twin
	// keeps serving ingress, so there is no blip. If no archive source is available they
	// fall back to a from-genesis bootstrap. RPCs carry no validator vote, so they never gate
	// the restore (it waits only on validator destinations); the gate at the end warns
	// (does not block) if an RPC is still syncing.
	var archiveTar string
	if snapMode {
		if tar, ok := cfg.takeArchiveSnapshot(trackerStep, otherSite(targetSite)); ok {
			archiveTar = tar
			defer cleanupSnapshot(archiveTar)
		} else {
			fmt.Println("WARN: no usable archive snapshot source — recovering RPCs will full-bootstrap from genesis.")
		}
	}

	// Phase 1 — seed the target site's validators (+ spare), bring them up as
	// trackers, and wait until the validator destinations reach the tip. The pinned
	// RPC is excluded here when delayRPC; it comes up in Phase 2. Each target's
	// stale data is discarded first, so it can never resurrect the divergent
	// post-failover frontier — identities (staking/active, staking/l1) live outside
	// data/ and are preserved. See prepareTarget / wipeL1Data.
	fmt.Printf("== restore: prepare + bring site %s up as trackers ==\n", siteName(targetSite))
	if err := saveIntents(cfg.stateFile, trackerStep); err != nil {
		fatalf("%v", err)
	}
	printIntents(topo, trackerStep)

	for i := range trackerStep {
		if topo.Site(i) == targetSite && !trackerStep[i].Cordoned {
			tar, snap := snapTar, snapMode
			if cfg.isArchiveNode(i) { // archive RPC: full-state archive seed, never the pruned snapshot
				tar, snap = archiveTar, archiveTar != ""
			}
			cfg.prepareTarget(cfg.nodeIPs[i], topo.MachineName(i), snap, tar)
		}
	}

	reconcile(cfg, trackerStep, false)

	fmt.Printf("waiting for site %s validator targets to sync to tip (within %d blocks)...\n", siteName(targetSite), syncToleranceBlocks)
	if !cfg.waitForTargetSynced(trackerStep, destIdx, srcIdx) {
		fatalf("site %s validator targets did not reach tip within %s; aborting. No stake moved — the site is a tracker, safe to retry.", siteName(targetSite), restoreSyncTimeout)
	}

	// Phase 2 — roll each validator key onto the target site, one at a time; bring
	// the pinned RPC up once the majority is local so it state-syncs from local peers.
	current := trackerStep
	for n, step := range keySteps {
		fmt.Printf("== restore: migrate validator %d/%d to site %s ==\n", n+1, len(keySteps), siteName(targetSite))
		if !cfg.waitForFullValidatorSet(current) {
			fatalf("validator set not at %d/%d before migrating validator %d/%d; aborting without moving it (%d already migrated). Re-run restore once the set is healthy.", want, want, n+1, len(keySteps), n)
		}
		if err := saveIntents(cfg.stateFile, step); err != nil {
			fatalf("%v", err)
		}
		printIntents(topo, step)
		reconcile(cfg, step, false)
		if !cfg.waitForFullValidatorSet(step) {
			fatalf("validator did not return to %d/%d after migrating a key; chain is at 2/3 (still live). Re-run `reconcile apply` once the node recovers, then re-run restore.", want, want)
		}
		current = step

		if delayRPC && n+1 == rpcAfter {
			fmt.Printf("== restore: site %s holds the validator majority — bring up + seed %d archive RPC(s) from the source site's redundant-RPC snapshot ==\n", siteName(targetSite), len(rpcIdxs))
			rpcUp := cloneIntents(current)
			for _, ri := range rpcIdxs {
				rpcUp[ri].Cordoned = false
				// Seed each recovering archive RPC from the source site's redundant-RPC
				// archive snapshot (full state, near-tip); falls back to genesis bootstrap.
				cfg.prepareTarget(cfg.nodeIPs[ri], topo.MachineName(ri), archiveTar != "", archiveTar)
			}
			if err := saveIntents(cfg.stateFile, rpcUp); err != nil {
				fatalf("%v", err)
			}
			printIntents(topo, rpcUp)
			reconcile(cfg, rpcUp, false)
			current = rpcUp
		}
	}

	// Gate the pinned RPCs to tip so "restore complete" doesn't paper over a stale
	// ingress. Bounded + non-fatal: consensus is already restored, so if an RPC is
	// still sync-bound under load we warn rather than block the operator.
	if delayRPC {
		fmt.Printf("waiting for site %s pinned RPCs to sync to tip (within %d blocks)...\n", siteName(targetSite), syncToleranceBlocks)
		if !cfg.waitForTargetSyncedT(current, rpcIdxs, validatorMachineIdx(current), restoreRPCTimeout) {
			fmt.Printf("WARN: site %s pinned RPC(s) have not caught up within %s. Consensus is restored (validators %d/%d) but an RPC is still syncing — likely sync-bound under sustained load. Ease load and it will finish; re-run `reconcile verify` to confirm.\n", siteName(targetSite), restoreRPCTimeout, want, want)
		}
	}

	fmt.Printf("\nrestore complete — validator set is active on site %s, chain held quorum throughout.\n", siteName(targetSite))

	fmt.Println()
	verifyAgreement(cfg)
}

// validatorDestIdx returns the machine indexes that will receive validator keys
// when the set is rolled onto targetSite — the first len(validatorKeys()) machines
// of that site (base+0..). These are the ONLY machines whose sync state gates
// promotion: a behind validator is the fork risk, so it must be at tip before it
// votes. The spare and pinned-RPC trackers never take a vote, so they finish
// syncing on their own and must not block (or deadlock) the restore.
func validatorDestIdx(topo Topology, targetSite int) []int {
	base := siteBase(targetSite)
	idx := make([]int, len(validatorKeys()))
	for i := range idx {
		idx[i] = base + i
	}
	return idx
}

// spareDestIdx returns the index of the site's hot-standby spare — the slot immediately
// after the validator destinations (layout per site is [v, v, v, spare, rpc, rpc]). The
// spare is a state-sync node (same DB profile as the validators, not archive) held ready to
// be promoted if a validator fails. -1 if the site has no spare slot.
func spareDestIdx(topo Topology, targetSite int) int {
	i := siteBase(targetSite) + len(validatorKeys())
	if i >= topo.Size() {
		return -1
	}
	return i
}

// rpcMachineIdxs returns the indexes of the pinned dedicated-RPC machines in the given
// site (their home identity is an RPC key), in ascending order — so [0] is the primary
// (1st) RPC and the last is the redundant (2nd) RPC. Empty if the site has none.
func rpcMachineIdxs(topo Topology, site int) []int {
	var idx []int
	for i := 0; i < topo.Size(); i++ {
		if topo.Site(i) == site && isRPCKey(topo.HomeKey(i)) {
			idx = append(idx, i)
		}
	}
	return idx
}

// redundantRPCMachineIdx returns the site's REDUNDANT (last) RPC machine — the one safe
// to stop for a consistent archive snapshot because the primary RPC keeps serving
// ingress. -1 if the site has fewer than one RPC. (With a single RPC it returns that
// one, which a caller stopping it should treat as an ingress-affecting fallback.)
func redundantRPCMachineIdx(topo Topology, site int) int {
	idx := rpcMachineIdxs(topo, site)
	if len(idx) == 0 {
		return -1
	}
	return idx[len(idx)-1]
}

// waitForTargetSynced polls until every validator-destination machine (destIdx) is
// SERVING and within syncToleranceBlocks of the active tip. It gates only the
// machines about to receive a validator key; spare/RPC trackers carry no vote and
// catch up on their own. Every poll logs the tip and each target's gap, and when
// that gap is GROWING it warns that write load is outpacing catch-up — the signal
// that a graceful failback should wait for a lull (see docs/two-site-failover.md).
func (c *config) waitForTargetSynced(intents []MachineIntent, destIdx, srcIdx []int) bool {
	return c.waitForTargetSyncedT(intents, destIdx, srcIdx, restoreSyncTimeout)
}

// waitForTargetSyncedT is waitForTargetSynced with an explicit timeout (used for
// the bounded pinned-RPC catch-up gate).
func (c *config) waitForTargetSyncedT(intents []MachineIntent, destIdx, srcIdx []int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	prevWorst := -1
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
		fmt.Printf("  sync gate: tip=%d  %s\n", tip, strings.Join(parts, "  "))
		if synced {
			fmt.Printf("  validator targets synced (worst gap %d <= %d).\n", worst, syncToleranceBlocks)
			return true
		}
		// Not converging — gap above tolerance and NOT shrinking (growing or stuck) —
		// means live write load is outrunning the rejoining site's catch-up. Surface
		// it so the operator can lower bombard's rate (or wait for a lull) if the gap
		// won't close; the chain now keeps up at full rate, so this is rarely hit.
		if worst > syncToleranceBlocks && prevWorst >= 0 && worst >= prevWorst {
			fmt.Printf("  WARN: targets not converging (worst gap %d, was %d) — write load is outpacing catch-up; lower bombard rps if it won't close.\n", worst, prevWorst)
		}
		prevWorst = worst
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(restorePollInterval)
	}
}

// waitForFullValidatorSet polls until every validator identity is SERVING.
func (c *config) waitForFullValidatorSet(intents []MachineIntent) bool {
	deadline := time.Now().Add(restoreSetTimeout)
	want := len(validatorKeys())
	for {
		res := c.checkHealth(intents)
		got := servingValidatorCount(intents, res)
		if got >= want {
			fmt.Printf("  validators serving: %d/%d.\n", got, want)
			return true
		}
		if time.Now().After(deadline) {
			fmt.Printf("  validators serving: %d/%d (timeout).\n", got, want)
			return false
		}
		time.Sleep(restorePollInterval)
	}
}

// servingValidatorCount counts distinct validator identities SERVING under the
// given intents + health snapshot.
func servingValidatorCount(intents []MachineIntent, results []healthResult) int {
	n := 0
	for i, in := range intents {
		if i >= len(results) {
			break
		}
		if !in.Cordoned && isValidatorKey(in.Key) && results[i].state == healthServing {
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
