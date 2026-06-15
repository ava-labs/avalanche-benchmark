package main

// Graceful rolling restore (two-site mode). Where `site-failover` is a hard
// cutover (cordon a whole site, swap the validator set across in one shot),
// `restore` moves the set back one validator at a time while both DCs are
// healthy: bring the target site up as trackers, wait until they are synced to
// the live tip, then migrate v1, v2, v3 in sequence with a health gate between
// each. Because the chain never drops below 2/3 and each promoted node is
// already at tip (so it continues the live branch), there is no chain downtime
// and no fork — the operational answer to "restore the original DC after a
// failover, ideally without downtime." See docs/two-site-failover.md.

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
	restoreSyncTimeout  = 30 * time.Minute // generous: replaying a heavy post-failover backlog can take minutes (benign wait, logged each poll)
	restoreSetTimeout   = 5 * time.Minute
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
	srcIdx := validatorMachineIdx(trackerStep)    // currently-active validators (the source site = the tip)
	destIdx := validatorDestIdx(topo, targetSite) // machines that will receive a validator key (gate only these)

	// Phase 1 — bring the target site up as trackers and wait until the validator
	// destinations reach the tip. The spare/RPC trackers catch up on their own and
	// do NOT gate promotion: they never take a vote, so a slow one can't fork the
	// chain — and must not be able to block (or deadlock) the failback.
	fmt.Printf("== restore: bring site %s up as trackers ==\n", siteName(targetSite))
	if err := saveIntents(cfg.stateFile, trackerStep); err != nil {
		fatalf("%v", err)
	}
	printIntents(topo, trackerStep)
	reconcile(cfg, trackerStep, false)

	fmt.Printf("waiting for site %s validator targets to sync to tip (within %d blocks)...\n", siteName(targetSite), syncToleranceBlocks)
	if !cfg.waitForTargetSynced(trackerStep, destIdx, srcIdx) {
		fatalf("site %s validator targets did not reach tip within %s; aborting. No stake moved — the site is a tracker, safe to retry.", siteName(targetSite), restoreSyncTimeout)
	}

	// Phase 2 — roll each validator key onto the target site, one at a time.
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
	}

	fmt.Printf("\nrestore complete — validator set is active on site %s, chain held quorum throughout.\n", siteName(targetSite))
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

// waitForTargetSynced polls until every validator-destination machine (destIdx) is
// SERVING and within syncToleranceBlocks of the active tip. It gates only the
// machines about to receive a validator key; spare/RPC trackers carry no vote and
// catch up on their own. Every poll logs the tip and each target's gap, and when
// that gap is GROWING it warns that write load is outpacing catch-up — the signal
// that a graceful failback should wait for a lull (see docs/two-site-failover.md).
func (c *config) waitForTargetSynced(intents []MachineIntent, destIdx, srcIdx []int) bool {
	deadline := time.Now().Add(restoreSyncTimeout)
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
		if prevWorst >= 0 && worst > prevWorst {
			fmt.Printf("  WARN: targets losing ground to the tip (worst gap %d->%d) — write load exceeds the rejoining site's sync rate; fail back during a lull.\n", prevWorst, worst)
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
