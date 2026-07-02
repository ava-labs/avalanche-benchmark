# Avalanche L1 Two-Data-Center (2x2) Benchmark

Tools to stand up an Avalanche L1 across **two data centers, both active**, and
drive it with transaction load — the 2x2 draft: 2 live validators in DC A and 2
in DC B, on a fixed pool of machines. There is no failover machinery on this
branch; both DCs validate at once and the goal is a cross-DC latency/throughput
measurement.

**Topology (configurable):** each data center runs **N validators + S hot
spares + R pinned RPC trackers**, plus a shared control host. The counts are set
per site in `.env` via per-role IP lists — `VALIDATOR_IPS`, `SPARE_IPS` (≥0),
`RPC_IPS` (≥1) — and the **length of each list is the count, the values are the
placement**. Validators in BOTH lists (`VALIDATOR_IPS` and
`BACKUP_VALIDATOR_IPS`) are registered, weighted L1 validators; the total across
sites must be ≥3 (the 2x2 is 2+2). An IP may repeat to **co-locate** multiple
nodes on one machine (each on its own port + data dir). DC A machines are named
`m1..mN`, DC B machines `b1..bN` — every status/endpoints output shows which DC
a node runs in. The pinned RPC trackers are never promoted to validators and are
the load generator's ingress. The control host runs the 5 P-chain (primary
network) validators the L1 bootstraps against, the load generator, and the
monitoring stack — it holds no L1 node. Single-site mode is supported unchanged
— just leave the `BACKUP_*` lists unset.

> **Co-location is a TEST affordance**, not a production layout: stacking nodes
> on one box removes fault isolation (one box loss takes them all), so a
> representative DR test still wants each site's validators on separate machines,
> ideally in two regions. The tooling warns when a box carries more than one node.

> **The 2x2 draft design** — key layout, consensus parameters, and deploy
> sequence — is in **[docs/two-site-2x2-draft.md](docs/two-site-2x2-draft.md)**.

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
# Edit .env — explicit per-role IP lists (length = count, values = placement;
# repeat an IP to co-locate). The 2x2 shape:
#   SSH_USER=ubuntu
#   SSH_KEY_PATH=/path/to/your-fleet-key
#   VALIDATOR_IPS=A1,A2             # DC A live validators
#   SPARE_IPS=                      # no spares in the 2x2 draft
#   RPC_IPS=A5,A6                   # DC A pinned RPC trackers
#   BACKUP_VALIDATOR_IPS=B1,B2      # DC B live validators (active, not standby)
#   BACKUP_SPARE_IPS=
#   BACKUP_RPC_IPS=B5,B6            # DC B pinned RPC trackers
```

`.env.example` documents every field. After editing, run
`./bin/reconcile endpoints` to print the resulting per-node layout
(name / site / role / host / port) before deploying. Setting the `BACKUP_*`
lists enables **two-data-center mode**: BOTH sites run live validators
(active-active). Single-site behavior is unchanged when the `BACKUP_*` lists
are unset.

> **Validator count > 3** needs more committed staking identities than the 20
> shipped in `staking/l1/`. `02_create_l1.sh` pre-flights this and prints the exact
> `go run ./cmd/genstaking <lo> <hi>` to run (locally, then rebuild the kit).
> The legacy positional `NODE_IPS` / `BACKUP_SITE_NODE_IPS` (exactly 6 each, fixed
> 3/1/2) still works if `VALIDATOR_IPS` is unset.

## Quick start

Run from the kit root on the control host. This is the condensed sequence; the
full walkthrough — with what to expect at each step — is in
[docs/e2e-runbook.md](docs/e2e-runbook.md).

```bash
./01_bootstrap_primary_network.sh   # 5 local P-chain validators (leave running)
./02_create_l1.sh                   # one-time: register validators, write network.env
./03_wipe_and_deploy_l1.sh          # deploy every pool node, start chain from genesis (destructive)
./04_monitoring.sh                  # Prometheus + Grafana on the control host
./scripts/failover/status.sh        # expect all nodes SERVING, "validators serving: 4/4" for the 2x2
./05_benchmark.sh                   # drive ~4000 tx/s at the pinned RPC trackers in both DCs
```

`03_wipe_and_deploy_l1.sh` is **destructive** — it wipes node data and restarts
the chain from genesis (block 0). Re-run any time to reset to a clean chain; the
P-chain registration is preserved, so you don't re-run `01`/`02`. Editing
`chain-config.json` and re-running `03` is how you apply a new chain config. A
fresh chain sits at block 0 until the benchmark drives load — Avalanche produces
blocks on demand.

## Monitoring (Prometheus + Grafana)

`04_monitoring.sh` runs Prometheus + Grafana **on the control host** — not on a
pool node — and scrapes every node's `:9652/ext/metrics`.

```bash
make monitoring-deps     # one-time (source builds only): fetch prometheus + grafana
./04_monitoring.sh       # generate scrape config, start both, print URLs
```

It discovers the fleet from `reconcile endpoints` (the single source of truth for
the configured topology + co-location-aware ports) and labels each target by
`site` (`a`/`b` — the node's data center), `machine` (`m1`…/`b1`…), and `role`
(`validator`/`spare`/`rpc`) — so the dashboards track whatever counts you set
and can be grouped/compared per DC. Two dashboards are provisioned:

- **Avalanche Benchmark** (`/d/avalanche-benchmark`) — per-node TPS, consensus,
  and verification panels.
- **Avalanche Failover** (`/d/avalanche-failover`) — per-node last-accepted
  (finalized) height, the **A→B finalized gap** (in the 2x2 this reads as the
  cross-DC finalization skew), node up/down, and block-acceptance rate.

Grafana is on `:3000`, Prometheus on `:9090` (anonymous admin, no login). If
those ports aren't open to you, tunnel over SSH:

```bash
ssh -i <key> -L3000:localhost:3000 -L9090:localhost:9090 <user>@<control-host>
# then open http://localhost:3000
```

Re-runnable (kills + restarts cleanly). Works in single-site mode too — the A→B
gap panel is just empty without a second data center.

## Pool commands

`scripts/failover/` operates the fixed pool (the directory name is historical —
there is no cross-site failover on this branch; validator identities stay in
their own data center).

```bash
./scripts/failover/status.sh     # read-only: each node's ACTUAL state + honest validators-serving count
./scripts/failover/verify.sh     # read-only: prove the live network is ONE branch + quorum healthy
./scripts/failover/down.sh <m>   # cordon machine m (take it "offline")
./scripts/failover/up.sh <m>     # uncordon machine m (return it to service)
./scripts/failover/clean.sh <m>  # wipe machine m's chain data and re-bootstrap it clean
./scripts/failover/failover.sh   # pure re-apply of the current intentions (recover an interrupted run)
```

`status.sh` reports every node as `SERVING@block` / `BOOTSTRAPPING` / `DOWN`
and an honest `validators serving: X/N`, so you see what is *actually*
happening rather than what was *intended*.

## Recovering From a Stalled Chain

> The worked example below uses the default **3 validators**; the thresholds
> scale with the configured count `N` — quorum is `ceil(2/3·N)` and the bootstrap
> rejoin latch is `ceil(75%·N)` (both = 3 when N = 3). `status.sh` computes them
> from `N` and prints the right numbers for your topology.

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
- ingress: the **pinned RPC trackers in both data centers** (`role=rpc` in
  `reconcile endpoints`). These are never promoted to validators; bombard fans
  across all of them and resubmits in-flight txs.

To change the profile, edit the constants at the top of `05_benchmark.sh`. See
[docs/throughput-tuning-and-benchmarks.md](docs/throughput-tuning-and-benchmarks.md)
for why block cadence — not rps — sets the throughput ceiling, and the reasoning
behind these values.

If you pushed too hard and need to restart, wait 60 seconds for the mempool to
clear before starting a new benchmark (mempool expiration is 1 minute).

## Block time

Genesis is configured with ACP-226 excess-gas parameters for fast block
production from the start, and the packaged AvalancheGo build pins a 1s
proposer-window branch. Both data centers produce at the **same cadence** —
`min-delay-target` in `chain-config.json` (there is no standby-site throttle in
the active-active topology).

To tune, edit `min-delay-target` in `chain-config.json` and re-run
`./03_wipe_and_deploy_l1.sh` (which resets the chain to genesis).

## Further reading

- [docs/two-site-2x2-draft.md](docs/two-site-2x2-draft.md) — this branch's 2x2
  active-active design: key layout, consensus parameters, deploy sequence.
- [docs/throughput-tuning-and-benchmarks.md](docs/throughput-tuning-and-benchmarks.md)
  — the 4000-rps profile and block-cadence tuning.
- Historical (describe the failover machinery this branch removed):
  [docs/e2e-runbook.md](docs/e2e-runbook.md),
  [docs/two-site-failover.md](docs/two-site-failover.md),
  [docs/failover-recovery-simulation.md](docs/failover-recovery-simulation.md).
