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
./scripts/failover/site-failover.sh a   # failback: cordon B, restore A
```

The swap wipes only `staking/active`, never `data/`, and reconcile's two-pass
order (all stops before any starts) holds across the whole pool — so all three
validators come up on site B together, satisfying the 75%-stake bootstrap latch
in one shot rather than stalling on it.

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

## What this simulates vs. production

- **Simulated faithfully:** key/identity conservation, zero-weight backup
  trackers at tip, all-at-once BLS/cert swap onto the backup site, in-flight tx
  replay, ingress cutover to the backup RPC, recovery-time measurement under
  load.
- **Not simulated:** the P-chain itself failing (the 5 P-chain validators run
  on the dev machine and stay up throughout — equivalent to the production
  assumption that P-chain state is frozen/controllable during failover); DNS/VIP
  cutover mechanics; fork reconciliation if site A kept mining after the swap
  decision (the simulation cordons A atomically, so no competing history is
  produced — see open items).

## Open items

- **Rollback/fork recovery:** simulate a non-atomic failover where site A mines
  blocks that site B never saw, then measure recovery (sync everyone from a
  chosen height). Needs the deterministic-EVM-sync mechanism (protocol request
  #8) or a benchmark-local approximation (copy DB from the highest backup node).
- **Terraform:** `terraform-aws-untested/` provisions one site; parameterize a
  second region for site B.
- **Configurable site sizes:** both sites are pinned at 5 machines; production
  asks may want asymmetric sites (e.g. 4 backup trackers, no backup spare).
