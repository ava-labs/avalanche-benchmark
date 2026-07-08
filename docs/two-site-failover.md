# Two-Site Failover (Site A/B)

The two-site design: a primary data center carrying consensus and a backup
data center holding registered-but-spare validators, onto which consensus is
moved by weight when the primary is lost. The weight mechanics themselves are
in [failover-recovery-simulation.md](failover-recovery-simulation.md); this
doc covers what is specific to running two sites and what the simulation does
and does not model.

## Topology

Each site is N validators + S spares + R pinned dedicated RPCs, set per site
by the `.env` per-role lists (`VALIDATOR_IPS`/`SPARE_IPS`/`RPC_IPS` and the
`BACKUP_*` equivalents). List length = count, values = placement, a repeated
IP co-locates nodes on one box. Both sites must share the same shape. The
default is 3 validators + 1 spare + 2 RPCs per site, 12 machines:

- **Site A (primary)**: machines 1-6, named `a1..a4` (stake slots) and
  `rpc_a1 rpc_a2`. All four stake slots start at validator weight, so the
  "spare" `a4` validates from day one: the ground state is 4 active
  validators.
- **Site B (backup)**: machines 7-12, named `b1..b4` and `rpc_b1 rpc_b2`.
  Stake slots start at spare weight (1000): registered on the P-chain at
  conversion, fully synced trackers, negligible vote. Put site B in a
  different region so cross-site sync latency is real; co-locating both
  sites on the same boxes exercises the orchestration but removes fault
  isolation (`down` then kills processes, not boxes).

RPC slots are never registered and never gain weight, which is what keeps
the ingress path alive through any failover. Only the RPC machines talk to
the outside world: one outbound TCP to the pinned public Fuji peer
(`FUJI_UPSTREAM_IPS`, default `18.192.93.241:9651`); validators sync the
Fuji P-chain through their own site's RPCs. All nodes run the same
state-sync + pruning profile (`state-sync-enabled`, `state-sync-min-blocks`
10000), so any node that falls behind or gets rebuilt self-heals by
state-syncing from the live network.

## Block cadence

Both sites run the same cadence: `min-delay-target` 25 ms in
`chain-config.json` (the hot ~40 blk/s profile), deployed verbatim to every
node by `deployChainConfig` in `cmd/reconcile/remote.go`. Historical note:
site B used to be throttled to 100 ms so a recovering site never had to
catch up to ~40 blk/s of live production, but the current rebuild+resync
recovery path handles that catch-up, so the asymmetric cadence was dropped.

Recovering nodes also pull faster thanks to a raised inbound bandwidth
allowance in `node-config.json` (`throttler-inbound-bandwidth-refill-rate`
8 MiB/s, burst 16 MiB).

## Running a site failover

The worked drill is `scenarios/03_datacenter_failure.sh` (site A dies,
site B takes consensus); failing back is just running
`scenarios/00_healthy.sh` (site A returns, consensus moves home). Stripped
of the reset preamble, the failover is three commands:

```bash
./fleet down 1 2 3 4 5 6        # site A is gone (simulated: SIGKILL)
./fleet weight validator 7 8 9  # raise site B first
./fleet weight dead 1 2 3 4     # then retire site A's stake
```

Expect a pause of up to ~5 minutes between the weight flip and site B's
first block: post-Durango proposer selection still derives from the parent
block's pre-flip P-chain height, so a spare-weight (1000) B validator must
win a slot by lottery against the dead site's stale 100,000 weights before
its first block re-anchors the schedule onto the new set (measured 3m58s in
the 2026-07-08 drill).

and the failback is:

```bash
./fleet up 1 2 3 4 5 6          # rebuild site A, blocks until SERVING
./fleet weight validator 1 2 3 4
./fleet weight spare 7 8 9 10
```

`up` rebuilds each machine clean (wipes its L1 chain data, keeps the synced
Fuji P-chain db) and state-syncs it onto the live branch, so a returning
site can never resurrect a stale frontier: there is nothing on disk to
resurrect. Raise before lower applies across sites exactly as within one.

Note scenario 03 promotes `7 8 9` (three machines) while the failback
restores `1 2 3 4` (four): the B-side spare `b4` stays at spare weight during the DR
posture, mirroring a production stance of running the minimum on the backup
site; the ground state on A is all four.

## What this simulates vs production

Simulated faithfully:

- one validator set registered once, consensus moved by weight only, no key
  material ever crossing sites;
- backup-site trackers at tip when weight arrives;
- ingress cutover: bombard targets all four pinned RPCs, health-checks each,
  and resubmits in-flight txs, so the load rides through the site loss;
- halt-and-recover when too much weight goes down at once, including the
  ~75% bootstrap latch;
- recovery-time measurement under load (Failover Overview / Failover
  Details dashboards).

Not simulated:

- the anchor chain failing: the P-chain and the ValidatorManager's C-chain
  are Fuji's public networks and stay up throughout (the production
  equivalent is "the control plane survives the DC loss");
- DNS/VIP cutover mechanics in front of the RPCs;
- a concurrent split with both sites producing: impossible here by
  construction, one weight set exists and the churn-capped seesaw moves it.

## Historical: the key-swap era (pre 2026-06)

Earlier iterations moved staking keys between machines instead of moving
weight (only 3 identities existed; failover copied them onto backup boxes).
A validated 2026-06-12 run measured failover A to B at ~18s under load, and
exposed the design's fatal failback hazard: restarting a stale site rolled
the chain back ~7.7k blocks, and rejoining backup nodes resurrected a
discarded frontier from surviving VM state. Fixing that required snapshot
seeding, sync gates, and a rolling restore command. The weight model made
the whole apparatus unnecessary: identities never move, a returning site is
rebuilt empty and state-syncs, and the failback is the same two weight
commands as the failover. The measured facts that outlived the redesign are
the ~14 blk/s replay ceiling (hence the 100 ms backup cadence) and the
full-`data/` wipe rule for rejoining nodes (hence `up` rebuilding clean).
