# Continuous physical state replication (warm standby)

**Status:** proposal / spec. Nothing here is wired up yet.
**Goal:** keep the headline profile untouched — ~5 ms finality, ~4000 TPS — while
making the backup site a near-tip *warm* standby, so a site loss costs seconds of
data (small bounded RPO), not the current cross-region consensus-follow lag.

## 1. Why the standby can't keep up by following consensus

Measured on the consensus dashboard at 500 and 1000 rps (site A = active, site B =
cross-region standby tracking via consensus):

| offered load | A produces | B accepts | tx/block | B's actual tx/s (gas work) |
|---|---|---|---|---|
| 500 rps  | ~100 blk/s | ~45 blk/s | ~5  | ~225 tx/s |
| 1000 rps | ~88 blk/s  | ~41 blk/s | ~11 | ~450 tx/s |

Two facts fall out:

1. **B is block-count-bound, not gas-bound.** B's acceptance is pinned at ~41–45
   **blocks/s** regardless of how full each block is. Its actual execution work
   (~225–450 tx/s) is ~10× below the system's 4000 tx/s — B has gas/exec headroom to
   spare. The wall is the *number* of blocks, each of which costs B a verify/commit
   plus a cross-region consensus round-trip. The ~3000-deep `blks_processing`
   backlog on B (vs 1 on A) is the fingerprint of decision-rate saturation, not
   execution saturation.

2. **Lowering TPS doesn't help.** Block *rate* is decoupled from tx rate — it's
   pinned near the proposer floor (`initialMinDelayMS: 5` in `genesis.json`, with the
   dynamic-fee throttle disabled via `targetGas`/`baseFeeChangeDenominator` maxed),
   so halving rps shrinks blocks, not their count. The B/A keep-up ratio is ~0.45 at
   both 500 and 1000 rps. Only around ~180 rps does the chain go tx-starved enough
   for the block rate to fall under B's ~45 blk/s ceiling — i.e. throttling TPS to
   keep B synced means nerfing the system to ~5% of its capacity. Dead end.

**The fundamental limit (stated honestly):** zero-RPO cross-region failover requires
*synchronous* replication (B acks before A finalizes), which forces finality ≥ the
cross-region round-trip (~15–40 ms us-east-1 ↔ us-east-2). That is incompatible with
5 ms finality — it's speed-of-light, not a config knob. So with 5 ms finality the
replication is necessarily **asynchronous**, and RPO > 0. The design target is
therefore *small, bounded RPO + fast recovery*, not zero loss.

## 2. The idea: replicate committed state, not blocks

Stop trying to make B *follow via consensus* (the ~45 blk/s wall). Keep A at 5 ms /
4000 TPS, untouched. Instead, asynchronously ship A's **already-committed pebble/EVM
state** to B and have B apply it as raw bytes — **no EVM re-execution, no per-block
verification, no consensus poll.** That bypasses the exact bottleneck; the transfer
is bandwidth-bound, not block-rate-bound. This is the existing on-demand DB-snapshot
path (`cmd/reconcile/snapshot.go`) turned into a **continuous, incremental,
pre-staged** pipeline sourced from a node that is *not* lagging.

### The source already exists on the active site

Site A's own spare (`m4`) and pinned RPC (`m5`) are **same-region**, so they keep
pace at full block rate (~150 blk/s) — only the *cross-region* hop is slow. `m4` is
the ideal source: zero-weight (no quorum impact), holds no validator key, carries no
ingress (so brief stops are free), and is exactly what `snapshotSourceIdx()` already
selects (uncordoned, non-validator, non-RPC tracker). So a consistent near-tip image
is continuously available on A — we just need to stream it to B ahead of time.

## 3. Architecture

```
        site A (us-east-1)                         site B (us-east-2)
  ┌───────────────────────────┐            ┌───────────────────────────────┐
  │ m1 m2 m3  validators       │            │ b1 b2 b3  trackers (zero-wt)   │
  │ m5  pinned RPC (ingress)   │            │ b5  pinned RPC                 │
  │ m4  spare  ◄── SOURCE      │            │ data/staging/validator ◄────┐  │
  └───────┬───────────────────┘            └─────────────────────────────┼──┘
          │ 1. consistent local capture (brief stop)                     │
          │ 2. async cross-region incremental ship  ──────────────────────┘
          replicator loop (period T)
```

**Replicator loop** (period `T`, e.g. 5–30 s), one cycle:

1. **Consistent local capture on m4 (double-buffer).** `killNode(m4)` → local
   `rsync --delete data/validator/ data/.mirror/` (local disk, only the delta since
   last cycle, sub-second to seconds) → `start(m4)`. m4's downtime per cycle is just
   the *local* delta copy, never the cross-region transfer. `data/.mirror` is now a
   consistent, openable point-in-time image. Record its height (read m4's
   `eth_blockNumber` just before the stop) into a sidecar `MANIFEST.height`.
   - pebble/leveldb sorted-table files are immutable once written, so the rsync delta
     is just the new SSTs + manifest; `--delete` reaps compacted-away files.
2. **Canonical gate (reuse existing logic).** Before publishing, verify the captured
   image is on the live branch and within `snapshotSourceMaxLag` of a live validator —
   exactly `snapshotSourceCanonical()`. Never stage a stale/forked image (the b4-brick
   guard). A failed gate skips the cycle, leaving the last good staged image in place.
3. **Async cross-region ship.** `rsync --delete data/.mirror/` from m4 → each surviving
   site-B node's `data/staging/validator/` (+ `MANIFEST.height`). Incremental, so each
   cycle ships only the delta. Runs while m4 is back up — no node is stopped during the
   slow cross-region leg.

**At failover/restore**, the validator-set cutover seeds targets from the **pre-staged**
image instead of cloning a lagging tracker:

- **Hard `site-failover` (A nuked):** `reconcileBackupHeights()` currently clones the
  highest *surviving B node* (which is ~14k blocks behind). Change it to promote from
  `data/staging/validator` (seconds behind) when present and newer: atomic
  `mv data/staging/validator data/validator`, then start. RPO collapses from the
  cross-region lag to `T + ship time`.
- **Graceful `restore` (A alive):** `takeSnapshot()` already produces a fresh image;
  the staged copy means B targets start from a near-tip base and replay a trivial delta
  (or skip the snapshot entirely and do one final live delta-rsync from m4). Near-zero
  RPO because A is still there for the final sync.

## 4. Components & files

**New**
- `cmd/reconcile/replicate.go` — the replicator: `replicateOnce(cfg)` (one cycle:
  capture → gate → ship) and a `replicate` subcommand that loops every `T`
  (`REPLICATE_INTERVAL`, default 15s) until interrupted. Reuses `killNode`, `start`,
  `snapshotSourceIdx`, `snapshotSourceCanonical`, `checkHealth`.
- `scripts/failover/replicator.sh` — thin wrapper to run `reconcile replicate` under
  `setsid`/`nohup` on the control box (or as a systemd unit) so it survives logout.
- `docs/continuous-state-replication.md` — this doc.

**Modified**
- `cmd/reconcile/failover.go` — `reconcileBackupHeights()`: prefer
  `data/staging/validator` as the clone source (newer than any surviving node →
  `mv` into place) before falling back to the current highest-surviving-node clone.
- `cmd/reconcile/restore.go` — `rollingRestore()`/`prepareTarget()`: when a staged
  image is present and canonical, seed from it (or do a final delta-rsync from m4)
  instead of a full `takeSnapshot`.
- `cmd/reconcile/snapshot.go` — factor the capture half of `takeSnapshot` into a
  reusable `captureConsistent(host, destDir)` used by both `takeSnapshot` and the
  replicator (single definition of "stop → copy → restart → gate").
- `04_monitoring.sh` / consensus dashboard — add a **staging lag** panel (see §6).

**On-host layout (site B nodes)**
- `data/staging/validator/` — rsync landing zone for the streamed image.
- `data/staging/MANIFEST.height` — height of the staged image (for lag metric + the
  newer-than-surviving-node decision).

## 5. Consistency model (why the staged image always opens)

- The image is captured from a **stopped** m4, so it's a clean point-in-time pebble/EVM
  state — never a torn live copy. (This is why the current `snapshotPull` insists on
  `killNode` first; we keep that invariant, just locally and incrementally.)
- The cross-region `rsync --delete` writes into `data/staging`, which **no node has
  open** on B — so a partially-shipped cycle can't corrupt a running DB. Promotion is
  an atomic directory `mv`, done only after a cycle completes.
- The **canonical gate** (`snapshotSourceCanonical`) runs every cycle, so a stale/forked
  capture is never published — the same guard that stopped the b4 brick, applied
  continuously.
- Write `MANIFEST.height` *after* the data rsync succeeds; readers treat its presence +
  value as the commit marker (no manifest ⇒ incomplete ⇒ ignore).

## 6. RPO/RTO characterization + measurement plan

- **RPO** (data lost on a hard nuke) = age of the last published staged image =
  `T + ship_time`. Instrument it: replicator writes `staged_height` and capture
  timestamp; a node-exporter textfile collector (or pushgateway) exposes it, and a new
  dashboard panel plots **validator tip − staged_height** (in blocks) and the wall-clock
  age. This is the live RPO readout, analogous to the keep-up ratio panel.
- **RTO** (time to serve after failover) = `mv` + node start + replay of the residual
  delta (seconds, since the base is near-tip) + validator-set cutover (already
  measured).
- **Experiment:** run at full 4000 TPS, sweep `T ∈ {5,15,30,60}s`, record staged-lag
  blocks and bytes-per-cycle. Then trigger a hard `site-failover` and measure actual
  blocks-lost vs the predicted RPO. This answers "how far can we push it" with finality
  and TPS *maxed*, quantifying the data-loss window instead of throttling to hide it.

## 7. Failure modes

| failure | handling |
|---|---|
| replicator process dies | staged image goes stale; the staging-lag panel climbs → alert. Failover still works off the last good image (just older RPO). Restart is stateless. |
| ship interrupted mid-cycle | `data/staging` left partial, but `MANIFEST.height` not yet updated ⇒ treated as the *previous* good image. Next cycle resumes (rsync is restartable). |
| capture gate fails (stale/forked m4) | cycle skipped, last good staged image retained; logged. |
| m4 unhealthy / gone | no source on A ⇒ replicator pauses; on a real nuke the last staged image is what failover uses. |
| disk pressure | `data/.mirror` (on m4) ≈ one DB copy; `data/staging` (on B) ≈ one DB copy. Budget ~2× DB size per relevant host; the loop never accumulates more than two generations. |
| torn image somehow promoted | guarded three ways: stop-before-capture, manifest-after-data commit marker, canonical gate. |

## 8. Phasing

- **Phase 1 — rsync double-buffer (no node-code changes; works on current ext4 infra).**
  Everything above. Brief periodic m4 stops; RPO ≈ `T + ship` (seconds-to-tens-of-seconds).
  This is the recommended first cut — pure ops tooling around the binary we already ship.
- **Phase 2 — zero-stop capture.** Eliminate the m4 stop using pebble's `Checkpoint()`
  (hard-linked, consistent, in-process, no stall) exposed via a small `/ext/admin` hook
  or sidecar, OR a filesystem snapshot (would need the data dir moved to its own
  LVM/ZFS volume — not present today). Drops RPO toward 1 s and removes all source
  downtime. Pebble-specific; leveldb has no equivalent clean checkpoint, so confirm the
  engine (`--db-type` default; code comments say pebble) before committing to this.

## 9. Open questions for sign-off

1. **Cadence `T`** — start at 15 s? (RPO ≈ 15–20 s at 4000 TPS.)
2. **Transport** — Phase 1 routes m4 → control → B (control already holds keys to all
   nodes). Direct m4 → B is one less hop but needs cross-site SSH/SG rules. OK to
   double-hop via control for v1?
3. **Where the replicator runs** — control box (simplest, already orchestrates
   reconcile) vs. on m4 (needs the bench key on m4). Recommend control box.
4. **Scope** — is the requirement just *recoverability* (warm standby, this doc), or
   also *B serving reads at the live tip* (a different, harder goal that runs into the
   same consensus-follow ceiling)? This doc targets recoverability.
5. **Phase 2 appetite** — is a small avalanchego/subnet-evm admin hook for
   `Checkpoint()` on the table, or stay ops-only (Phase 1)?
