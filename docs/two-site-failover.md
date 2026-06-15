# Two-Site Failover (Site A/B Simulation)

Extension of the [single-site failover simulation](failover-recovery-simulation.md)
that adds a **backup data center (site B)**: a second fixed pool of 5 machines
running as **live zero-weight syncing trackers**, onto which the whole validator
set can be swapped when the primary site (site A) suffers a full outage —
mirroring the production design where consensus runs in one DC and a backup DC
holds non-voting nodes that are BLS/cert-swapped into the active set on a
full-site failure.

Everything from the single-site doc still applies; this doc only covers the
deltas. Single-site mode (no `BACKUP_SITE_NODE_IPS`) is byte-for-byte unchanged.

## Topology

- **Site A (primary)** = `NODE_IPS` (5 machines): `m1-m3` weighted validators,
  `m4` hot spare, `m5` pinned dedicated RPC. Unchanged.
- **Site B (backup)** = `BACKUP_SITE_NODE_IPS` (5 machines): `b1-b4` zero-weight
  syncing trackers, `b5` pinned dedicated RPC for the backup site. For realistic
  results put site B in a different region/DC so the cross-site sync latency is
  real (e.g. site A in us-east-1, site B in us-east-2 ≈ NY/NJ↔Chicago).
- Site B nodes are **never registered on the P-chain**. They track the subnet
  exactly like `m4`/`m5` do — full chain state, no consensus weight — which is
  what makes site failover fast: their `data/` is already at tip when the
  validator keys arrive.

## Identity map (keys, conserved)

The single-site model's "5 keys" grows to 13. Every machine always holds
exactly one identity; no two **live** machines ever share one:

| Key | Role |
|-----|------|
| 6-8 | `v1-v3` — the only P-chain-registered validator identities. Move between machines; cross sites **only** via `site-failover`. |
| 9 | `m4`'s home: site-A spare |
| 10 | `m5`'s pinned RPC identity (never promoted) |
| 11-13 | `m1-m3`'s home identities — worn when displaced (e.g. while site B is active) |
| 14-17 | `b1-b4`'s home identities — zero-weight sync trackers |
| 18 | `b5`'s pinned RPC identity (never promoted) |

Unique homes (vs. the single shared `nv` key 9) exist because a backup site
means several live non-validating machines at once, and live machines can't
share a NodeID. In single-site mode the shared-9 behavior is preserved
unchanged. Identities 11-18 were generated with `go run ./cmd/genstaking 11 18`
(NodeIDs in `staking/node-ids.env`).

## Mapping policy delta

One rule is added to the sticky mapping in `cmd/reconcile/plan.go`:

> **Orphaned validator keys never cross sites implicitly.** A single-machine
> cordon reassigns the orphan within that machine's own site (m-fault → m4;
> post-failover b-fault → b4). With no same-site spare the key stays uncovered
> and quorum drops — by design, matching the "consensus is single-site"
> invariant. Only an explicit `site-failover` moves the validator set.

## Commands

Most existing wrappers work over the 10-machine pool (`down.sh 7` cordons `b2`)
because they delegate to the topology-aware reconcile binary. The exception is
`clean.sh`, which indexes `NODE_IPS` directly and so operates on site A only.
One new wrapper:

```bash
./scripts/failover/site-failover.sh b   # full site-A outage: cordon all of A,
                                        # v1-v3 swap onto b1-b3, b4 = new spare
./scripts/failover/site-failover.sh a   # hard failback: cordon B, restore A (see caveat)
```

`site-failover` is a **hard cutover** — it cordons a whole site and swaps the
set across in one shot. That's correct for an *outage* (the primary is already
gone), but using it to fail *back* onto a site whose data is stale reproduces
the rollback fork (below). For a planned return with both DCs healthy, use the
graceful rolling restore instead.

### Graceful rolling restore (no chain downtime)

`restore <a|b>` migrates the validator set onto a site **one validator at a
time**, keeping the chain at ≥2/3 throughout — the operational answer to
"restore the original DC after a failover, ideally without downtime."

```bash
./scripts/failover/restore.sh a   # roll the set back to site A, gracefully
```

It runs in two phases:

1. **Trackers + sync gate** — uncordon the target site so its nodes rejoin as
   zero-weight trackers, then **wait until the validator-destination machines are
   synced to the live tip** (within `syncToleranceBlocks`). Only the machines about
   to take a validator key are gated: the spare and pinned-RPC trackers carry no
   vote, so they finish syncing on their own and can't fork — or block — the
   restore. No stake moves until those targets are at tip, so no node is ever
   promoted onto a stale/divergent branch — this is what eliminates the fork.
2. **Rolling swap** — move v1, then v2, then v3 onto the target site, one key at
   a time, with a health gate (`waitForFullValidatorSet`) between each. Dropping
   one validator leaves 2/3 live, so the chain never halts; the promoted node's
   `data/` is preserved, so it continues the live branch in well under a second.

End state equals the steady seed with the target site active. Because the chain
never stops and the target is always at tip, **there is no fork and nothing to
replay** — unlike the hard `site-failover` cutover.

**What "no downtime" does and doesn't mean.** No *chain* downtime: quorum holds
the whole time, so the ATS/settlement path (which talks to the RPC, not the
validators) sees no interruption. It is *not* a live process hot-migration —
each of the three validators restarts (~5s) as its key lands, and while one is
down its proposer slots stall briefly (Ilya's ~1s-per-slot finding), so there
are three short latency/throughput dips, not an outage. Erasing even those dips
would require temporarily running stake in *both* DCs (a real P-chain
validator-set change), which violates the single-DC-consensus invariant — a
topology decision, not a tooling one.

The swap wipes only `staking/active`, never `data/`, and reconcile's two-pass
order (all stops before any starts) holds across the whole pool — so all three
validators come up on site B together, satisfying the 75%-stake bootstrap latch
in one shot rather than stalling on it.

**Failback is sync-bound under sustained load.** The gate above only clears once
the validator destinations reach the tip, so a graceful failback completes quickly
only when the rejoining site is at (or near) tip. If write load produces blocks
faster than the recovering DC can replay them — measured on 4-vCPU nodes, where a
saturated tracker replays at a fraction of the production rate — the gate holds
until load eases. Operationally: **fail back during a lull.** The structural fix is
deterministic EVM state-sync to a chosen height (request #8) rather than full block
replay; until then the gate's per-poll log warns when a target is losing ground.

## Benchmark across a failover

`05_benchmark.sh` automatically adds `b5` to bombard's `--rpc` list in two-site
mode. Bombard fans sends across reachable endpoints, runs one watcher per
endpoint, and resubmits in-flight txs — so the run rides through the site
failover: sends fail over to `b5` when `m5` dies with site A, unmined in-flight
txs get resubmitted to the new validator set (the "replay unmined transactions"
step of the production runbook), and the latency report captures the recovery
window end-to-end.

A full demo cycle:

```bash
./03_wipe_and_deploy_l1.sh              # deploys all 10 machines
./05_benchmark.sh                       # in one window
./scripts/failover/site-failover.sh b   # in another: kill site A mid-load
watch -n 2 ./scripts/failover/status.sh # watch B bootstrap + serve
./scripts/failover/site-failover.sh a   # fail back under load
```

## Validated run (2026-06-12, us-east-1 ↔ us-east-2)

10× m6a.xlarge (+1 control), ~1000 rps bombard via both pinned RPCs:

- **Failover A→B under load: 17.8s** for the reconcile (stop site A, swap
  v1-v3 onto b1-b3, start), **~30s more to 3/3 serving** from the backup site.
  bombard rode through on the backup RPC with a catch-up burst, zero manual
  intervention. All three new validators in lockstep immediately — the
  trackers were at tip, and the all-stops-before-any-starts ordering cleared
  the 75% bootstrap latch in one pass.
- **Failback B→A exposed the rollback hazard (by design of the test):**
  `site-failover a` stops every site-B node and restarts site A from its
  pre-failover state. Site A bootstrapped only to its own highest block — the
  ~7.7k blocks site B mined during its tenure were on stopped machines, so the
  chain resumed ~2 minutes in the past. Same branch, no equivocation (block
  hashes at the divergence point matched exactly), but everything mined during
  the backup tenure was discarded, and the load generator's nonce line
  straddled the discard → 0 TPS until restarted.
- **Discarded-branch cleanup is itself a hazard:** archiving only
  `data/validator/db` was not enough — the rejoining backup nodes recovered
  the discarded 15k-block frontier from surviving VM state and reported a
  height the validators didn't have. Archiving the **entire `data/validator`
  dir** and rejoining resynced all 10 nodes onto the live branch, verified by
  matching heights and a clean follow-up load run (0 resubmits).

**Failback procedure (until state hand-back is automated):**

1. Bring site A up as *trackers first* while B still validates: `up.sh 1..5`
   (they wear home identities 11-13/9/10 and sync the B-tenure history).
2. Wait until site A is at tip (`status.sh` heights match).
3. Then `site-failover.sh a` — no history is lost, no rollback.

A naive immediate `site-failover.sh a` after a real outage (site A state stale)
re-creates the rollback above. This is exactly the gap the
deterministic-EVM-sync protocol ask (request #8) closes.

## What this simulates vs. production

- **Simulated faithfully:** key/identity conservation, zero-weight backup
  trackers at tip, all-at-once BLS/cert swap onto the backup site, in-flight tx
  replay, ingress cutover to the backup RPC, recovery-time measurement under
  load, and the failback rollback hazard (measured above).
- **Not simulated:** the P-chain itself failing (the 5 P-chain validators run
  on the dev machine and stay up throughout — equivalent to the production
  assumption that P-chain state is frozen/controllable during failover); DNS/VIP
  cutover mechanics; a *concurrent* split (both sites mining at once is
  impossible here because the validator keys are conserved — one site holds
  them at a time).

## Open items

- ~~**Staged failback automation:** encode the failback procedure as a single
  command (uncordon-as-trackers → wait-for-tip → swap).~~ **Done** —
  `restore <a|b>` (rolling, one validator at a time; see above).
- **Truly seamless (zero-dip) restore:** would need a transient dual-DC
  validator set via real P-chain validator-management txns — relaxes the
  single-DC-consensus invariant; pending a topology decision.
- **Honest health for idle chains:** `status.sh` reports SERVING from
  `eth_blockNumber` even when no blocks are being produced; add a
  height-advancing check so a post-failback stall is visible.
- **Terraform:** `terraform-aws-untested/` provisions one site; parameterize a
  second region for site B.
- **Configurable site sizes:** both sites are pinned at 5 machines; production
  asks may want asymmetric sites (e.g. 4 backup trackers, no backup spare).
