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

	// restoreThrottleMinDelayMS slows the active site's block production during catch-up.
	// A node replaying a backlog in bootstrap tops out ~14 blk/s; at the normal 40 blk/s
	// (25ms) cadence a behind node never closes the gap, so we drop the producers to
	// ~10 blk/s for the recovery. min-delay-target is read once at VM init, so applying it
	// requires a node restart (see rollingSetMinDelay). Production returns to the normal
	// cadence on its own once the key-roll moves the validators onto the un-throttled target
	// site; the old source config is then reset to its role default for the next cycle.
	restoreThrottleMinDelayMS = 100
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

	rpcIdxs := rpcMachineIdxs(topo, targetSite)
	srcIdx := validatorMachineIdx(trackerStep) // active (source) validators = the producers + tip reference

	// Slow the active source site's block production so the recovering nodes can catch up to a
	// near-static tip instead of a 40 blk/s one (see restoreThrottleMinDelayMS). Rolling-restart
	// the source validators one at a time so the set never drops below quorum. Ctrl-C just exits —
	// we deliberately do NOT roll-restart to undo on abort (re-running restore re-applies it).
	fmt.Printf("== restore: slowing site %s block production to %dms (~%d blk/s) for catch-up ==\n",
		siteName(otherSite(targetSite)), restoreThrottleMinDelayMS, 1000/restoreThrottleMinDelayMS)
	cfg.rollingSetMinDelay(trackerStep, srcIdx, restoreThrottleMinDelayMS)

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
	//   - pinned RPCs          <- a full ARCHIVE snapshot of the source site's REDUNDANT (2nd) RPC
	//                             (its twin keeps serving ingress, so no blip).
	// Both snapshots are captured up front and EVERY target is seeded + started together in
	// Phase 1 — the RPCs are NOT held back behind validator sync, since a near-tip archive
	// snapshot needs no catch-up race. If a source is unavailable, that role falls back to
	// wipe + state-sync (validators) / from-genesis bootstrap (RPCs).
	var snapTar string
	if tar, ok := cfg.takeSnapshot(trackerStep, otherSite(targetSite)); ok {
		snapTar = tar
		defer cleanupSnapshot(snapTar)
	} else {
		fmt.Println("WARN: no usable pruned snapshot source — recovering validators will wipe + state-sync.")
	}
	var archiveTar string
	if tar, ok := cfg.takeArchiveSnapshot(trackerStep, otherSite(targetSite)); ok {
		archiveTar = tar
		defer cleanupSnapshot(archiveTar)
	} else {
		fmt.Println("WARN: no usable archive snapshot source — recovering RPCs will full-bootstrap from genesis.")
	}

	// Phase 1 — seed EVERY target-site node together (validators + spare from the pruned
	// snapshot, RPCs from the archive snapshot), bring them all up as trackers, and wait until
	// the validator destinations reach the tip. Each target's stale data is discarded first, so
	// it can never resurrect a divergent post-failover frontier — identities (staking/active,
	// staking/l1) live outside data/ and are preserved. See prepareTarget / wipeL1Data.
	fmt.Printf("== restore: prepare + bring site %s up as trackers ==\n", siteName(targetSite))
	if err := saveIntents(cfg.stateFile, trackerStep); err != nil {
		fatalf("%v", err)
	}
	printIntents(topo, trackerStep)

	for i := range trackerStep {
		if topo.Site(i) == targetSite && !trackerStep[i].Cordoned {
			if cfg.isArchiveNode(i) { // archive RPC: full-state archive clone (can't state-sync)
				cfg.prepareTarget(cfg.nodeIPs[i], topo.MachineName(i), archiveTar != "", archiveTar)
			} else { // validator / spare: pruned snapshot clone (falls back to state-sync)
				cfg.prepareTarget(cfg.nodeIPs[i], topo.MachineName(i), snapTar != "", snapTar)
			}
		}
	}

	reconcile(cfg, trackerStep, false)

	// Gate on ALL recovering nodes — validators, spare, AND archive RPCs — reaching tip, not just
	// the validators. The RPCs are the slowest (full-trie writes), so they catch up last; gating
	// them here means the whole site is at tip before any key moves.
	allRecovering := append([]int{}, destIdx...)
	if sp := spareDestIdx(topo, targetSite); sp >= 0 {
		allRecovering = append(allRecovering, sp)
	}
	allRecovering = append(allRecovering, rpcIdxs...)
	fmt.Printf("waiting for site %s recovering nodes (validators + spare + RPCs) to reach tip (within %d blocks)...\n", siteName(targetSite), syncToleranceBlocks)
	cfg.waitForTargetSynced(trackerStep, allRecovering, srcIdx)

	// Phase 2 — roll each validator key onto the target site, one at a time. The RPCs already
	// came up (snapshot-seeded) in Phase 1, so there is no separate RPC bring-up step here.
	current := trackerStep
	for n, step := range keySteps {
		fmt.Printf("== restore: migrate validator %d/%d to site %s ==\n", n+1, len(keySteps), siteName(targetSite))
		cfg.waitForFullValidatorSet(current)
		if err := saveIntents(cfg.stateFile, step); err != nil {
			fatalf("%v", err)
		}
		printIntents(topo, step)
		reconcile(cfg, step, false)
		cfg.waitForFullValidatorSet(step)
		current = step
	}

	// The validators are now producing on the target site at the normal 25ms cadence (they were
	// never throttled). Reset the old source validators' chain-config to their role default so a
	// future failover brings them back up at the normal cadence, not the catch-up one. They are
	// trackers now, so this is a lazy config rewrite — it takes effect on their next restart.
	fmt.Printf("== restore: resetting site %s block production config to normal cadence ==\n", siteName(otherSite(targetSite)))
	for _, i := range srcIdx {
		if i >= 0 && i < len(cfg.nodeIPs) {
			cfg.deployChainConfig(cfg.nodeIPs[i])
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
	want := len(validatorKeys())
	for {
		res := c.checkHealth(intents)
		got := servingValidatorCount(intents, res)
		if got >= want {
			fmt.Printf("  validators serving: %d/%d.\n", got, want)
			return
		}
		time.Sleep(restorePollInterval)
	}
}

// setMinDelay rewrites min-delay-target (ms) in the node's chain-config.json. A node restart is
// required for it to take effect — the VM reads min-delay-target once at init (subnet-evm vm.go).
func (c *config) setMinDelay(host string, ms int) {
	c.ssh(host, fmt.Sprintf(`cd %s && sed -i 's/"min-delay-target": *[0-9]*/"min-delay-target": %d/' chain-config.json`, c.remoteDir, ms))
}

// rollingSetMinDelay rewrites min-delay-target on each machine and restarts it one at a time,
// waiting for the set to be producing again before the next — so an active validator set never
// drops below quorum while its block-production cadence is changed.
func (c *config) rollingSetMinDelay(intents []MachineIntent, idxs []int, ms int) {
	for _, i := range idxs {
		if i < 0 || i >= len(c.nodeIPs) {
			continue
		}
		ip := c.nodeIPs[i]
		c.setMinDelay(ip, ms)
		c.killNode(ip)
		c.start(ip, ip)
		c.waitSetProducing(intents, idxs)
	}
}

// waitSetProducing blocks until every validator in idxs is SERVING AND the set's tip has advanced
// between two consecutive polls — i.e. the chain has a working quorum and is minting blocks again,
// not merely that the RPCs answer. A freshly-restarted validator serves eth_blockNumber (at its
// last-accepted height) seconds before it has rejoined consensus; gating the NEXT restart on the
// tip advancing guarantees only ever one validator is out at a time, so production just dips
// (proposer misses) instead of stalling at 0 TPS (quorum lost).
func (c *config) waitSetProducing(intents []MachineIntent, idxs []int) {
	var lastTip uint64
	havePrev := false
	for {
		res := c.checkHealth(intents)
		allServing := true
		var tip uint64
		for _, i := range idxs {
			if i < 0 || i >= len(res) || res[i].state != healthServing {
				allServing = false
				continue
			}
			if res[i].block > tip {
				tip = res[i].block
			}
		}
		if allServing && havePrev && tip > lastTip {
			return // full set up and the tip moved -> quorum is producing
		}
		lastTip, havePrev = tip, true
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
