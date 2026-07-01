package main

// DB-snapshot seeding for `restore` (two-site mode). The default restore path
// seeds each recovering target from a point-in-time copy of the live source
// site's chain database, instead of forcing a from-scratch state-sync. A
// recovering node then only linear-replays the tiny delta accumulated since the
// snapshot (well under state-sync-min-blocks), rather than pulling the whole
// state trie from peers that are themselves under load — the fragile, memory- and
// bandwidth-heavy path that wedged recovering nodes under sustained ingress.
//
// Safety rests on three invariants, all enforced in takeSnapshot:
//   - Provenance: the source must be a node on the CANONICAL active site (the one
//     currently holding the full validator set). Snapshotting stale site-A data
//     would re-import the divergent post-failover frontier the wipe path exists to
//     destroy — so we refuse unless the source site holds the live validator majority.
//   - Fidelity: SERVING is NOT enough — a node serves eth_blockNumber just the same
//     while wedged on a stale or FORKED branch. The source must also be caught up to
//     the live tip (within snapshotSourceMaxLag) and on the SAME branch as a live
//     validator (identical block hash at a common recent height). A behind/forked
//     tracker cloned site-wide imports a last-accepted the validators never had and
//     bricks every target in an unrecoverable BOOTSTRAPPING loop (measured 2026-06-22;
//     see snapshotSourceCanonical).
//   - Consistency: the source is STOPPED for the copy. Copying a live pebble/EVM
//     DB yields a torn image; the source is the zero-weight sync tracker (b4) or
//     spare (m4), so stopping it briefly costs neither quorum nor ingress.
//
// If no eligible source is available — or the only one is stale/forked — restore falls
// back to wipe + state-sync, which pulls the canonical branch straight from the validators.

import (
	"fmt"
	"os"
)

const snapshotTar = "/tmp/failover-restore-snapshot.tgz"

// snapshotArchiveTar holds the role-matched ARCHIVE snapshot — a full (unpruned) copy
// of the source site's REDUNDANT RPC. With two archive RPCs per site, restore stops the
// 2nd RPC to take a consistent copy while the 1st keeps serving ingress (no blip), then
// seeds BOTH recovering archive RPCs from it — so they start near-tip instead of
// bootstrapping the whole chain from genesis (which wedges under load; 2026-06-22).
const snapshotArchiveTar = "/tmp/failover-restore-archive.tgz"

// snapshotSourceMaxLag bounds how far behind the live validator tip a snapshot source
// may be. A dedicated tracker sits at the tip in steady state; the snapshot pull itself
// then adds a few thousand blocks of delta (replayed on load), so a small lag at check
// time is irrelevant. A source lagging far beyond this is not a faithful copy of the
// live chain — and is the canary for one wedged on a stale/forked branch (b4 was 25k+
// blocks behind when it bricked site A, 2026-06-22). Generous enough to ignore load
// jitter, tight enough to catch genuine staleness.
const snapshotSourceMaxLag = 2000

// otherSite returns the site opposite the given one (the source site for a
// restore onto targetSite).
func otherSite(site int) int {
	if site == siteB {
		return siteA
	}
	return siteB
}

// snapshotSourceIdx returns the index of the best DB-snapshot source on sourceSite:
// an uncordoned, non-validator, non-RPC tracker — the zero-weight sync tracker (b4)
// or the spare (m4). Stopping it briefly for a consistent copy risks neither quorum
// (it holds no validator key) nor ingress (it is not the pinned RPC). Returns -1 if
// the site has no eligible tracker live.
func snapshotSourceIdx(intents []MachineIntent, topo Topology, sourceSite int) int {
	for i, in := range intents {
		if topo.Site(i) != sourceSite || in.Cordoned {
			continue
		}
		if topo.isValidatorKey(in.Key) || topo.isRPCKey(in.Key) {
			continue
		}
		return i
	}
	return -1
}

// takeSnapshot captures a consistent DB snapshot of the live source site to a local
// file on the control box, returning its path and true on success. It enforces both
// safety invariants (canonical provenance + stopped-for-consistency) and restarts
// the source immediately after the copy so its downtime is bounded by the tar. On
// any failure it returns ("", false) so the caller can fall back to state-sync.
func (c *config) takeSnapshot(intents []MachineIntent, sourceSite int) (string, bool) {
	if !c.snapshotProvenanceOK(intents, sourceSite) {
		return "", false
	}
	srcIdx := snapshotSourceIdx(intents, c.topo, sourceSite)
	if srcIdx < 0 {
		fmt.Printf("snapshot: site %s has no eligible (non-validator, non-RPC) tracker to snapshot from.\n", siteName(sourceSite))
		return "", false
	}
	return c.captureFrom(intents, srcIdx, snapshotTar)
}

// takeArchiveSnapshot captures a full (unpruned) ARCHIVE snapshot of the source site's
// REDUNDANT (last) RPC for seeding the recovering site's archive RPCs. Every site is
// required to run >=2 RPCs (enforced in loadPool), so there is always a redundant RPC safe
// to stop: its twin keeps serving ingress while we copy, so there is no blip — even when the
// two RPCs are co-located on one box (we stop one process, the other answers). Same canonical
// gate as the pruning snapshot. Returns ("", false) if the source is not a canonical tip, in
// which case the recovering RPCs fall back to a from-genesis bootstrap.
func (c *config) takeArchiveSnapshot(intents []MachineIntent, sourceSite int) (string, bool) {
	if !c.snapshotProvenanceOK(intents, sourceSite) {
		return "", false
	}
	rpcs := rpcMachineIdxs(c.topo, sourceSite)
	if len(rpcs) < 2 {
		// Shouldn't happen (loadPool enforces >=2), but never snapshot a lone RPC.
		fmt.Printf("snapshot: site %s has %d RPC(s); need a redundant one to snapshot — recovering RPCs will full-bootstrap.\n", siteName(sourceSite), len(rpcs))
		return "", false
	}
	srcIdx := rpcs[len(rpcs)-1] // redundant (last) RPC; its twin keeps serving ingress
	return c.captureFrom(intents, srcIdx, snapshotArchiveTar)
}

// snapshotProvenanceOK enforces the Provenance invariant: only snapshot a site that
// currently holds the full live validator set — that is the canonical tip.
func (c *config) snapshotProvenanceOK(intents []MachineIntent, sourceSite int) bool {
	if got := validatorKeysInSite(intents, c.topo, sourceSite, true); got != c.topo.NVal {
		fmt.Printf("snapshot: source site %s holds %d/%d live validators — not canonical, cannot snapshot.\n",
			siteName(sourceSite), got, c.topo.NVal)
		return false
	}
	return true
}

// captureFrom takes a consistent DB snapshot of a specific source machine (by pool
// index) into tarPath, enforcing the SERVING + canonical (Fidelity) gates and restarting
// the source immediately after the copy. Shared by the pruning-tracker and archive-RPC
// capture paths so both keep identical safety checks.
func (c *config) captureFrom(intents []MachineIntent, srcIdx int, tarPath string) (string, bool) {
	topo := c.topo

	// Gate: the source must be SERVING (its RPC answers eth_blockNumber).
	res := c.checkHealth(intents)
	if srcIdx >= len(res) || res[srcIdx].state != healthServing {
		fmt.Printf("snapshot: source %s is not serving — cannot snapshot a healthy tip.\n", topo.MachineName(srcIdx))
		return "", false
	}

	// Fidelity gate: SERVING only proves the RPC answers — not that it answers for the
	// LIVE chain. Refuse a source that has fallen behind or diverged onto a fork, else
	// its DB is cloned onto every target and bricks the site (see snapshotSourceCanonical).
	if !c.snapshotSourceCanonical(intents, srcIdx) {
		return "", false
	}

	tip := res[srcIdx].block

	fmt.Printf("== restore: snapshot %s @ block %d -> %s (stop -> copy -> restart) ==\n",
		topo.MachineName(srcIdx), tip, tarPath)
	_ = os.Remove(tarPath)
	c.gracefulStop(srcIdx) // SIGTERM so the snapshot is a CLEAN image (a SIGKILL'd source has a corrupted EVM snapshot that wedges every seeded target)
	if !c.snapshotPull(srcIdx, tarPath) {
		c.start(srcIdx) // best-effort restart even if the copy failed
		return "", false
	}
	c.start(srcIdx) // restart immediately — source downtime is just the copy
	fmt.Printf("  snapshot captured: %s (%s); %s restarted.\n", tarPath, humanSize(tarPath), topo.MachineName(srcIdx))
	return tarPath, true
}

// snapshotSourceCanonical verifies the chosen source is a faithful copy of the LIVE
// chain before its DB is cloned site-wide. checkHealth's "SERVING" only proves the RPC
// answers eth_blockNumber — a node wedged on a stale or forked branch answers it just
// the same (b4 served block 283501 of an abandoned branch while the live tip was ~308k).
// Cloning that DB onto the recovering site imports a last-accepted the live validators
// never had; every target then FATALs in bootstrap ("failed to verify block … not found
// with last accepted …") and is stuck BOOTSTRAPPING forever (measured 2026-06-22: a
// 25k-stale, forked b4 bricked all of site A).
//
// Validate against a live validator on the same (canonical) site — co-located, so the
// comparison is same-region and fair:
//   - freshness: the source is within snapshotSourceMaxLag blocks of the validator tip.
//   - branch:    source and validator return the SAME block hash at a common recent
//     height (the single-branch proof verifyAgreement uses).
//
// On any staleness, fork, or unreadable reference we refuse (return false) so the caller
// falls back to wipe + state-sync — which pulls the canonical branch from the validators
// and cannot import a fork.
func (c *config) snapshotSourceCanonical(intents []MachineIntent, srcIdx int) bool {
	// Reference: a live, uncordoned validator on the source (canonical) site.
	refIdx := -1
	for _, i := range validatorMachineIdx(c.topo, intents) {
		if c.topo.Site(i) == c.topo.Site(srcIdx) {
			refIdx = i
			break
		}
	}
	if refIdx < 0 {
		fmt.Println("  source check: no live validator to compare against — refusing (will state-sync).")
		return false
	}
	refTip, _, okR := c.blockAt(refIdx, "finalized")
	srcTip, _, okS := c.blockAt(srcIdx, "finalized")
	if !okR || !okS {
		fmt.Println("  source check: could not read source/reference tip — refusing (will state-sync).")
		return false
	}

	// Branch check at a common recent height (back off the tip to avoid sampling skew).
	common := commonHeight(refTip, srcTip)
	tag := fmt.Sprintf("0x%x", common)
	_, refHash, okRH := c.blockAt(refIdx, tag)
	_, srcHash, okSH := c.blockAt(srcIdx, tag)
	if !okRH || !okSH || refHash == "" || srcHash == "" {
		fmt.Printf("  source check: could not read block %d for branch comparison — refusing (will state-sync).\n", common)
		return false
	}

	ok, reason := snapshotSourceVerdict(c.topo.MachineName(srcIdx), refTip, srcTip, common, refHash, srcHash)
	fmt.Printf("  source check: %s\n", reason)
	return ok
}

// commonHeight is a recent height both nodes should already have finalized: the lower
// of the two tips, backed off a small margin so neither is still settling on it.
func commonHeight(refTip, srcTip uint64) uint64 {
	common := srcTip
	if refTip < common {
		common = refTip
	}
	if common > 16 {
		common -= 16
	}
	return common
}

// snapshotSourceVerdict is the pure accept/refuse decision for a snapshot source, given
// the validator/source finalized tips and their block hashes at the common height. A
// source is usable only if it is within snapshotSourceMaxLag of the live tip (not stale)
// AND shares the validator's hash at `common` (same branch, not forked). Returns the
// decision plus a human-readable reason for the log. Pure — unit-tested.
func snapshotSourceVerdict(name string, refTip, srcTip, common uint64, refHash, srcHash string) (bool, string) {
	lag := 0
	if refTip > srcTip {
		lag = int(refTip - srcTip)
	}
	if lag > snapshotSourceMaxLag {
		return false, fmt.Sprintf("%s is %d blocks behind the live tip (%d vs %d) — refusing STALE source (will state-sync).",
			name, lag, srcTip, refTip)
	}
	if refHash != srcHash {
		return false, fmt.Sprintf("%s is on a DIFFERENT branch at block %d (%s vs live %s) — refusing FORKED source (will state-sync).",
			name, common, shortHash(srcHash), shortHash(refHash))
	}
	return true, fmt.Sprintf("%s OK — tip %d (lag %d), branch matches live @ block %d.", name, srcTip, lag, common)
}

// shortHash truncates a 0x block hash for human-readable logs.
func shortHash(h string) string {
	if len(h) > 18 {
		return h[:18] + "…"
	}
	return h
}

// loadSnapshot stops the target, removes its (stale) chain data, and extracts the
// snapshot in its place. The node is started later by reconcile, at which point it
// opens the seeded DB and linear-replays only the small delta to the live tip.
func (c *config) loadSnapshot(i int, tar string) {
	in := c.instances[i]
	c.killNode(i)
	c.ssh(in.host, fmt.Sprintf("cd %s && rm -rf %s", c.remoteDir, in.dataDir))
	c.snapshotPush(i, tar)
}

// seedFromSnapshot loads a snapshot AND sets the target's chain-config to srcCfg — the
// chain-config of the node the snapshot came from. The on-disk DB records the pruning mode it
// was built with, and coreth refuses to open it under a different mode ("node has operated
// with pruning disabled, shutting down to prevent missing tries"). So a node seeded from an
// archive RPC (pruning-disabled) must run the archive config, not its own role default — and
// the start script copies ~/chain-config.json into place on launch, so that is the file to
// overwrite. srcCfg empty (read failed) falls back to a plain load, preserving prior behavior.
func (c *config) seedFromSnapshot(i int, tar, srcCfg string) {
	in := c.instances[i]
	c.loadSnapshot(i, tar)
	if srcCfg != "" {
		c.sshStdin(in.host, fmt.Sprintf("cat > %s/%s", c.remoteDir, in.chainCfg), srcCfg)
	}
}

// prepareTarget readies one recovering machine for restart: seed it from the
// snapshot (default) or wipe it for a from-scratch state-sync (fallback). Used for
// both the Phase-1 validator targets and the later-joined pinned RPC, so the two
// paths stay identical.
func (c *config) prepareTarget(i int, snap bool, tar string) {
	name := c.topo.MachineName(i)
	// Reset the node to its ROLE-default chain-config so its pruning/state-sync mode matches
	// the DB it is about to run: restore seeds validators/spare from a pruning tracker and the
	// archive RPCs from an archive source, so the role default is always the right match. This
	// also undoes any archive config a prior failover left on a validator (which would make
	// coreth reject the pruning snapshot, or block the state-sync fallback that needs
	// state-sync-enabled). See seedFromSnapshot / deployChainConfig.
	c.deployChainConfig(i)
	if snap {
		fmt.Printf("  %s: stop + wipe + seed from snapshot\n", name)
		c.loadSnapshot(i, tar)
		return
	}
	fmt.Printf("  %s: stop + wipe data dir (state-sync)\n", name)
	c.wipeL1Data(i)
}

// cleanupSnapshot removes the local snapshot tar once restore is done with it.
func cleanupSnapshot(tar string) {
	if tar != "" {
		_ = os.Remove(tar)
	}
}

// humanSize renders a file's size as a human-readable string, or "?" if unstattable.
func humanSize(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "?"
	}
	b := fi.Size()
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}
