# Two-Site Failover (Site A/B)

The two-site design: a primary data center carrying consensus and a backup
data center holding registered-but-spare validators, onto which consensus is
moved by weight when the primary is lost. The weight mechanics themselves are
in [failover-recovery-simulation.md](failover-recovery-simulation.md); this
doc covers what is specific to running two sites and what the simulation does
and does not model.

## Topology

The fleet is whatever `nodes.ini` lists: named nodes, each `role=validator`
or `role=rpc`, tagged with a display-only `dc=` per data center. The shipped
inventory is 4 validators + 2 RPCs per DC, 12 machines:

- **DC A (primary)**: `a1..a4` and `rpc_a1 rpc_a2`. All four validators
  run at validator weight (raised by `scenarios/00_healthy.sh` right after
  creation), so the ground state is 4 active validators. A "hot spare" is
  not a role, just a validator held at weight 1.
- **DC B (backup)**: `b1..b4` and `rpc_b1 rpc_b2`. Validators sit at
  spare weight (1000): registered on the P-chain at conversion, fully
  synced trackers, negligible vote. Put DC B in a different region so
  cross-site sync latency is real; co-hosting both DCs on the same boxes
  exercises the orchestration but removes fault isolation (`down` then
  kills processes, not boxes).

role=rpc nodes are never registered and never gain weight, which is what
keeps the ingress path alive through any failover. Only the RPC machines
talk to the outside world: one outbound TCP to the pinned public Fuji peer
(`FUJI_UPSTREAM_IPS`, default `18.192.93.241:9651`); validators sync the
Fuji P-chain through the fleet's RPC nodes. All nodes run the same
state-sync + pruning profile (`state-sync-enabled`, `state-sync-min-blocks`
10000), so any node that falls behind or gets rebuilt self-heals by
state-syncing from the live network.

## Block cadence

Both sites run the same cadence: `min-delay-target` 25 ms in
`chain-config.json` (the hot ~40 blk/s profile), deployed verbatim to every
node by `upload` in `cmd/reconcile/remote.go`. Historical note:
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
./fleet down dc=A               # site A is gone (simulated: SIGKILL)
./bin/l1 apply --weights b1=100000,b2=100000,b3=100000,b4=100000,a1=1,a2=1,a3=1,a4=1
                                # raise site B first, then retire site A's stake
```

`apply` self-staggers a large multi-validator shift: it converges in bounded
steps (each moves <=20% of the live total, then waits out the ~30s
proposer-lag window before the next), so the change is absorbed gradually and
the L1 keeps quorum under sustained load instead of wedging on a one-burst
weight flip. This makes the shift take several minutes; pausing the load
generator across it is optional defense-in-depth, no longer required.

Expect a pause of up to ~5 minutes between the weight flip and site B's
first block: post-Durango proposer selection still derives from the parent
block's pre-flip P-chain height, so a spare-weight (1000) B validator must
win a slot by lottery against the dead site's stale 100,000 weights before
its first block re-anchors the schedule onto the new set (measured 3m58s in
the 2026-07-08 drill).

and the failback is:

```bash
./fleet up dc=A                 # rebuild site A, blocks until SERVING
./bin/l1 apply --weights a1=100000,a2=100000,a3=100000,a4=100000,b1=1000,b2=1000,b3=1000,b4=1000
```

`up` rebuilds each node clean (wipes its L1 chain data, keeps the synced
Fuji P-chain db) and state-syncs it onto the live branch, so a returning
site can never resurrect a stale frontier: there is nothing on disk to
resurrect. Raise before lower applies across sites exactly as within one.

One catch-up failure mode `fleet up` cannot see: under sustained load a
rejoining node can replay blocks slower than the chain produces them
(measured ~31 blk/s catch-up against ~37 blk/s at head at 4000 TPS), so its
gap to tip GROWS while every health check stays green. `fleet up` only
detects a frozen gap, not a growing one. Watch the rejoining node's height
against tip; if the gap is widening, pause the load generator for about 3
minutes until the node reaches tip, then resume the load.

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

- the anchor chain failing: the P-chain is a public network and stays up
  throughout (the production equivalent is "the control plane survives the
  DC loss");
- DNS/VIP cutover mechanics in front of the RPCs;
- a concurrent split with both sites producing: impossible here by
  construction, one weight set exists and the seesaw moves it.

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
