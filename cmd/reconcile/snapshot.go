package main

// DB-snapshot seeding for `restore` (two-site mode). The default restore path
// seeds each recovering target from a point-in-time copy of the live source
// site's chain database, instead of forcing a from-scratch state-sync. A
// recovering node then only linear-replays the tiny delta accumulated since the
// snapshot (well under state-sync-min-blocks), rather than pulling the whole
// state trie from peers that are themselves under load — the fragile, memory- and
// bandwidth-heavy path that wedged recovering nodes under sustained ingress.
//
// Safety rests on two invariants, both enforced in takeSnapshot:
//   - Provenance: the source must be a node on the CANONICAL active site (the one
//     currently holding the full validator set) and SERVING. Snapshotting stale
//     site-A data would re-import the divergent post-failover frontier that the
//     wipe path exists to destroy — so we refuse unless the source site holds the
//     live validator majority.
//   - Consistency: the source is STOPPED for the copy. Copying a live pebble/EVM
//     DB yields a torn image; the source is the zero-weight sync tracker (b4) or
//     spare (m4), so stopping it briefly costs neither quorum nor ingress.
//
// If no eligible source is available, restore falls back to wipe + state-sync.

import (
	"fmt"
	"os"
)

const snapshotTar = "/tmp/failover-restore-snapshot.tgz"

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
		if isValidatorKey(in.Key) || isRPCKey(in.Key) {
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
	topo := c.topo

	// Provenance: only snapshot a site that currently holds the full live validator
	// set — that is the canonical tip. Never snapshot a stale/displaced site.
	if got := validatorKeysInSite(intents, topo, sourceSite, true); got != len(validatorKeys()) {
		fmt.Printf("snapshot: source site %s holds %d/%d live validators — not canonical, cannot snapshot.\n",
			siteName(sourceSite), got, len(validatorKeys()))
		return "", false
	}
	srcIdx := snapshotSourceIdx(intents, topo, sourceSite)
	if srcIdx < 0 {
		fmt.Printf("snapshot: site %s has no eligible (non-validator, non-RPC) tracker to snapshot from.\n", siteName(sourceSite))
		return "", false
	}

	// Gate: the source must be SERVING (a healthy, caught-up copy of the chain).
	res := c.checkHealth(intents)
	if srcIdx >= len(res) || res[srcIdx].state != healthServing {
		fmt.Printf("snapshot: source %s is not serving — cannot snapshot a healthy tip.\n", topo.MachineName(srcIdx))
		return "", false
	}
	srcHost := c.nodeIPs[srcIdx]
	tip := res[srcIdx].block

	fmt.Printf("== restore: snapshot site %s from %s @ block %d (stop -> copy -> restart) ==\n",
		siteName(sourceSite), topo.MachineName(srcIdx), tip)
	_ = os.Remove(snapshotTar)
	c.killNode(srcHost) // stop for a consistent on-disk image (zero-weight: no quorum/ingress impact)
	if !c.snapshotPull(srcHost, snapshotTar) {
		c.start(srcHost, srcHost) // best-effort restart even if the copy failed
		return "", false
	}
	c.start(srcHost, srcHost) // restart immediately — source downtime is just the copy
	fmt.Printf("  snapshot captured: %s (%s); %s restarted.\n", snapshotTar, humanSize(snapshotTar), topo.MachineName(srcIdx))
	return snapshotTar, true
}

// loadSnapshot stops the target, removes its (stale) chain data, and extracts the
// snapshot in its place. The node is started later by reconcile, at which point it
// opens the seeded DB and linear-replays only the small delta to the live tip.
func (c *config) loadSnapshot(host, tar string) {
	c.killNode(host)
	c.ssh(host, fmt.Sprintf("cd %s && rm -rf data/validator", c.remoteDir))
	c.snapshotPush(host, tar)
}

// prepareTarget readies one recovering machine for restart: seed it from the
// snapshot (default) or wipe it for a from-scratch state-sync (fallback). Used for
// both the Phase-1 validator targets and the later-joined pinned RPC, so the two
// paths stay identical.
func (c *config) prepareTarget(host, name string, snap bool, tar string) {
	if snap {
		fmt.Printf("  %s: stop + wipe + seed from snapshot\n", name)
		c.loadSnapshot(host, tar)
		return
	}
	fmt.Printf("  %s: stop + wipe data/validator (state-sync)\n", name)
	c.wipeL1Data(host)
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
