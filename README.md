# Avalanche L1 Failover Benchmark

Tools to stand up an Avalanche L1 across **two data centers**, drive it with
transaction load, and **simulate cross-region validator failover** — losing a
whole site and recovering the validator set onto the backup — on a fixed pool of
machines, without ever adding or removing machines from the fleet.

**Topology:** two sites of **6 machines** each, plus a control host. Site A
(primary) runs **3 validators + 1 hot spare + 2 pinned archive RPC nodes**; site
B (backup) runs **3 zero-weight syncing trackers + 1 spare + 2 pinned archive RPC
nodes**. The three validator identities (staking keys) are *conserved* — they
move between machines, and across sites on a `site-failover`, but are never
duplicated, so the chain stays a single branch. The pinned RPC nodes are never
promoted to validators, so the load generator's ingress survives failover. The
control host runs the 5 P-chain (primary network) validators the L1 bootstraps
against, the load generator, and the monitoring stack — it holds no L1 node, so
it keeps coordinating through a full-site outage. Single-site mode (no backup) is
supported unchanged — just leave `BACKUP_SITE_NODE_IPS` unset.

> **Want the full end-to-end drill in one place?** See
> **[docs/e2e-runbook.md](docs/e2e-runbook.md)** — install → configure → deploy →
> benchmark → site failover → graceful failback, start to finish.

## Ports

Open the following ports on your remote nodes:

| Port | Service | Required | Notes |
|------|---------|----------|-------|
| 22 | SSH | Yes | Remote access |
| 9652-9653 | AvalancheGo | Yes | L1 HTTP (RPC) / staking ports |

The five local P-chain validators run on the control host on ports `9650/9651`,
`9660/9661`, `9670/9671`, `9680/9681`, and `9690/9691`.

## Install

The release ships prebuilt and airgap-ready — no Go toolchain or network access
needed on the control host:

```bash
sudo rpm -i avalanche-benchmark-2026.06.23.x86_64.rpm   # installs to /opt/avalanche-benchmark
cd /opt/avalanche-benchmark
```

(Or extract `remote-benchmark.tar.gz` on a non-RHEL control host.) To build from
source instead (Linux, requires Go and git): `make`.

## Configure

```bash
cp .env.example .env
# Edit .env:
#   SSH_USER=ubuntu
#   SSH_KEY_PATH=/path/to/your-fleet-key
#   NODE_IPS=...               # exactly 6 — site A: m1-m3 validators, m4 spare, m5/m6 archive RPC
#   BACKUP_SITE_NODE_IPS=...   # exactly 6 — site B: b1-b3 trackers, b4 spare, b5/b6 archive RPC
```

`.env.example` documents each position. Setting `BACKUP_SITE_NODE_IPS` enables
**two-site mode**: a backup data center of zero-weight syncing trackers the
validator set can be swapped onto when the whole primary site goes down
(`./scripts/failover/site-failover.sh b`). To return once the primary is healthy,
use the graceful `restore.sh a` — it rolls the set back one validator at a time
with no chain downtime; `site-failover.sh a` is the hard-cutover failback for a
true outage (see the rollback caveat in
[docs/two-site-failover.md](docs/two-site-failover.md)). Single-site behavior is
unchanged when it is unset.

## Quick start

Run from the kit root on the control host. This is the condensed sequence; the
full walkthrough — with what to expect at each step — is in
[docs/e2e-runbook.md](docs/e2e-runbook.md).

```bash
./01_bootstrap_primary_network.sh   # 5 local P-chain validators (leave running)
./02_create_l1.sh                   # one-time: register validators, write network.env
./03_wipe_and_deploy_l1.sh          # deploy all 12 nodes, start chain from genesis (destructive)
./04_monitoring.sh                  # Prometheus + Grafana on the control host
./scripts/failover/status.sh        # expect all nodes SERVING, "validators serving: 3/3"
./05_benchmark.sh                   # drive ~4000 tx/s at the pinned RPC nodes
```

Then run the failover drill (separate terminal, benchmark left running):

```bash
./scripts/failover/site-failover.sh b   # nuke site A, fail the validator set onto B
./scripts/failover/restore.sh a         # graceful rolling failback to A (no chain downtime)
```

`03_wipe_and_deploy_l1.sh` is **destructive** — it wipes node data and restarts
the chain from genesis (block 0). Re-run any time to reset to a clean chain; the
P-chain registration is preserved, so you don't re-run `01`/`02`. Editing
`chain-config.json` and re-running `03` is how you apply a new chain config. A
fresh chain sits at block 0 until the benchmark drives load — Avalanche produces
blocks on demand.

## Monitoring (Prometheus + Grafana)

`04_monitoring.sh` runs Prometheus + Grafana **on the control host** — not on a
pool node, since a pool node disappears during a site failover — and scrapes
every node's `:9652/ext/metrics`, so the dashboards keep recording the survivors
as a site drops out.

```bash
make monitoring-deps     # one-time (source builds only): fetch prometheus + grafana
./04_monitoring.sh       # generate scrape config, start both, print URLs
```

It discovers the fleet from `.env` (all of `NODE_IPS`, plus `BACKUP_SITE_NODE_IPS`
in two-site mode) and labels each target by `site` (`a`/`b`) and `machine`
(`m1`-`m6`, `b1`-`b6`). Two dashboards are provisioned:

- **Avalanche Benchmark** (`/d/avalanche-benchmark`) — per-node TPS, consensus,
  and verification panels.
- **Avalanche Failover** (`/d/avalanche-failover`) — built for this demo:
  per-node last-accepted (finalized) height, the **A→B finalized gap**, node
  up/down, and block-acceptance rate. Watch site A flatline and site B take over
  live.

Grafana is on `:3000`, Prometheus on `:9090` (anonymous admin, no login). If
those ports aren't open to you, tunnel over SSH:

```bash
ssh -i <key> -L3000:localhost:3000 -L9090:localhost:9090 <user>@<control-host>
# then open http://localhost:3000
```

Re-runnable (kills + restarts cleanly). Works in single-site mode too — the A→B
gap panel is just empty without a backup site.

## Failover commands

`scripts/failover/` moves validator identities across the fixed pool. Within a
site the hot spare covers a downed validator; a `site-failover` moves the whole
set across sites. See
[docs/failover-recovery-simulation.md](docs/failover-recovery-simulation.md)
(single-site) and [docs/two-site-failover.md](docs/two-site-failover.md)
(two-site) for the design.

```bash
./scripts/failover/status.sh     # read-only: each node's ACTUAL state + honest validators-serving count
./scripts/failover/verify.sh     # read-only: prove the live network is ONE branch + quorum healthy
./scripts/failover/down.sh <m>   # cordon machine m (take it "offline") — within-site failover
./scripts/failover/up.sh <m>     # uncordon machine m (return it to service)
./scripts/failover/clean.sh <m>  # wipe machine m's chain data and re-bootstrap it clean

# Two-site mode (requires BACKUP_SITE_NODE_IPS):
./scripts/failover/site-failover.sh <a|b>  # hard cutover: nuke the other site, swap the whole set here
./scripts/failover/restore.sh <a|b>        # graceful rolling failback: one validator at a time, no downtime
```

`site-failover` models a real outage: it **hard-kills every node on the down site
at once** (freezing its tip at the true data-loss boundary), then the surviving
site forms consensus on the blocks it already holds — no state is pulled from the
dead site. `status.sh` reports every node as `SERVING@block` / `BOOTSTRAPPING` /
`DOWN` and an honest `validators serving: X/3`, so you see what is *actually*
happening rather than what was *intended*.

## Recovering From a Stalled Chain

With 3 validators, the L1 behaves very differently depending on whether you are
**taking a validator down** or **bringing one back up**. The asymmetry is not a
bug — it comes from how AvalancheGo bootstraps a (re)starting validator.

### Taking validators down is safe (the chain keeps going)

- **Take 1 of 3 down, hot spare present:** the spare immediately assumes the
  downed validator's identity → back to 3 active validators → **full speed**.
- **Take 1 of 3 down, no spare left:** the chain runs on **2 of 3**. Quorum is a
  simple majority (>50%), so it **keeps producing blocks — just slower**: the
  missing validator is the scheduled proposer for ~1/3 of slots, and each of those
  slots stalls ~1s before another validator takes over. Lower TPS, higher tail
  latency, but the chain never stops.
- **Take 2 of 3 down:** only **1 of 3** is left. That is below quorum, so the
  chain **HALTS** until validators are restored. (This is an expected, supported
  scenario — it is how you test recovery.)

### Bringing validators back up requires ALL THREE — not two

This is the part that surprises people. **Restarting a validator does not just
need quorum; it needs ~75% of validator stake online before it can bootstrap.**
AvalancheGo will not let a (re)starting validator begin syncing until it is
connected to `ceil(75%)` of the validator set. With 3 equal validators that
rounds up to **all 3**.

- Normal failover never hits this, because when one validator restarts the
  **other two are still running** — it sees 3/3 = 100% and bootstraps in seconds.
- But once the chain has **stalled** (down to 1 live validator), bringing back
  **only one more is not enough**. With two validators online (66% < 75%), the
  one you just started will sit forever in
  `API call rejected because chain is not done bootstrapping`, its block height
  frozen, and **the chain stays halted**. This is not a glitch and it will not
  resolve on its own.

**To recover a stalled chain, start all three validators.** You can bring them up
one at a time — nothing will happen until the **third** one connects, at which
point all of them clear the bootstrap latch together and the chain resumes within
seconds. If you bring up only two and the chain does not recover, that is expected
— bring up the last one.

`status.sh` makes this visible: a node waiting on the latch shows as
`BOOTSTRAPPING`, and the tool prints a hint reminding you to bring the remaining
validator(s) online.

| Action | Result |
|--------|--------|
| Take 1 of 3 down, spare present | Spare takes over → 3 active → full speed |
| Take 1 of 3 down, no spare | Runs on 2/3 → keeps producing, slower |
| Take 2 of 3 down | 1/3 → quorum lost → chain **HALTS** |
| Bring back **one** validator into a stalled chain | Still halted — rejoining node needs ≥75% (all 3) online |
| Bring back **all three** validators | Latch clears → chain resumes in seconds |

## Recovering a Node Stuck `BOOTSTRAPPING` (diverged local chain)

Occasionally a node comes back and **never finishes bootstrapping** — `status.sh`
shows it `BOOTSTRAPPING` indefinitely while the others are `SERVING`, and its
subnet-evm VM keeps dying. This is different from the 75% latch above: here the
node's **local chain database has diverged** from the network.

It happens when a validator is **hard-killed (SIGKILL) under heavy load** — which
is exactly what `down.sh` does to simulate a crash. The node can have locally
committed a block right before being killed that the network then reorged away,
so on restart its last-accepted block sits on an **orphaned fork**. It then fails
to verify the network's next block against its forked tip, the subnet VM logs a
`FATAL ... failed to verify block ... in bootstrapping` and exits, and the node is
stuck with no VM. A plain restart just re-hits the same bad data.

A node in this state can't be fixed by `up.sh`/`failover.sh` (they preserve
runtime data on purpose, so a hot spare rejoins fast). You have to **wipe its
chain data and let it re-bootstrap from the network**:

```bash
./scripts/failover/clean.sh <m>   # wipe machine m's data/, keep credentials, restart clean
```

`clean.sh` kills the node, removes only its `data/` directory (chain DB, logs,
generated chain configs), and restarts it via `reconcile apply`. It does **not**
touch `staking/` (the validator/spare identity) or the uploaded binaries/configs,
so the node returns with the **same identity** and re-syncs from the live
validators. Quorum is unaffected when you clean the spare; if you must clean an
active validator, do it while the other two are serving so the chain keeps
producing.

## Benchmark profile

`05_benchmark.sh` intentionally accepts no command-line flags — the failover demo
uses one fixed profile:

- target rate: **4000 rps**, with a 1% overshoot so *mined* TPS lands at or above
  target
- in-flight cap: **2000** (sized to the block cadence so the rps limiter binds,
  not the cap)
- resubmit interval: **5s**
- ingress: the **pinned archive RPC nodes** — `m5`/`m6` on site A, plus `b5`/`b6`
  on site B in two-site mode. These are never promoted to validators, so ingress
  survives a failover; bombard fans across all reachable RPCs and resubmits
  in-flight txs, so it rides straight through a `site-failover`.

To change the profile, edit the constants at the top of `05_benchmark.sh`. See
[docs/throughput-tuning-and-benchmarks.md](docs/throughput-tuning-and-benchmarks.md)
for why block cadence — not rps — sets the throughput ceiling, and the reasoning
behind these values.

If you pushed too hard and need to restart, wait 60 seconds for the mempool to
clear before starting a new benchmark (mempool expiration is 1 minute).

## Block time

Genesis is configured with ACP-226 excess-gas parameters for fast block
production from the start, and the packaged AvalancheGo build pins a 1s
proposer-window branch. The two sites run **different cadences**, applied at
deploy time:

- **Site A (primary): 25 ms** (`min-delay-target` in `chain-config.json`) — the
  hot ~40 blk/s profile.
- **Site B (backup): 100 ms** — ~10 blk/s, and only while it *produces* (i.e.
  after a failover). `min-delay-target` governs a node only while it proposes, so
  B tracks A's 25 ms blocks at full speed during normal operation; the slower
  backup cadence is what lets a recovering site converge without a rolling
  restart. See [docs/two-site-failover.md](docs/two-site-failover.md).

To tune, edit `min-delay-target` in `chain-config.json` and re-run
`./03_wipe_and_deploy_l1.sh` (which resets the chain to genesis).

## Further reading

- [docs/e2e-runbook.md](docs/e2e-runbook.md) — the full end-to-end failover &
  recovery drill.
- [docs/two-site-failover.md](docs/two-site-failover.md) — two-site design,
  identity map, and what is simulated vs. production.
- [docs/failover-recovery-simulation.md](docs/failover-recovery-simulation.md) —
  the single-site failover model and stalled-chain recovery theory.
- [docs/throughput-tuning-and-benchmarks.md](docs/throughput-tuning-and-benchmarks.md)
  — the 4000-rps profile and block-cadence tuning.
