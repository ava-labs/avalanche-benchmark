# Avalanche L1 Failover Benchmark

Tools to stand up an Avalanche L1 across **two data centers**, drive it with
transaction load, and **simulate cross-region validator failover** — losing a
whole site and recovering the validator set onto the backup — on a fixed pool of
machines, without ever adding or removing machines from the fleet.

**Topology (configurable):** each site runs **N validators + S hot spares + R
pinned archive RPC nodes**, plus a shared control host. The counts are set per
site in `.env` via per-role IP lists — `VALIDATOR_IPS` (≥3), `SPARE_IPS` (≥0),
`RPC_IPS` (≥1) — and the **length of each list is the count, the values are the
placement**. An IP may repeat to **co-locate** multiple nodes on one machine
(each on its own port + data dir), so node count is decoupled from machine count:
the full topology can run on as few as one box, or one node per box. The default
example is the validated shape — **3 validators + 1 spare + 2 RPC** per site (6
machines), site B (backup) running the same shape as zero-weight syncing
trackers. The validator identities (staking keys) are *conserved* — they move
between machines, and across sites on a `site-failover`, but are never
duplicated, so the chain stays a single branch. The pinned RPC nodes are never
promoted to validators, so the load generator's ingress survives failover. The
L1 anchors on **Fuji's public P-chain** (see [FUJI_PLAN.md](FUJI_PLAN.md)):
every node runs `--network-id=fuji --partial-sync-primary-network
--p-chain-follow-only`; the RPC tier follows one pinned public Fuji peer (the
fleet's only external TCP) and serves the P-chain onward to the validators
(two-hop). The control host runs the orchestration, the load generator, and the
monitoring stack; it holds no L1 node, so it keeps coordinating through a
full-site outage. Single-site mode (no backup) is supported unchanged: just
leave the `BACKUP_*` lists unset.

> **Co-location is a TEST affordance**, not a production layout: stacking nodes
> on one box removes fault isolation (one box loss takes them all), so a
> representative DR test still wants each site's validators on separate machines,
> ideally in two regions. The tooling warns when a box carries more than one node.

> **Want the full end-to-end drill in one place?** See
> **[docs/e2e-runbook.md](docs/e2e-runbook.md)** — install → configure → deploy →
> benchmark → site failover → graceful failback, start to finish.

## Ports

Open the following ports on your remote nodes:

| Port | Service | Required | Notes |
|------|---------|----------|-------|
| 22 | SSH | Yes | Remote access |
| 9652-9653 | AvalancheGo | Yes | L1 HTTP (RPC) / staking ports |

The RPC machines additionally need ONE outbound TCP to the pinned public Fuji
peer (default `18.192.93.241:9651`, see `.env.example`); validators need no
external connectivity at all.

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
# repeat an IP to co-locate). VALIDATOR_IPS >= 3, RPC_IPS >= 2 (redundant RPC required).
#   SSH_USER=ubuntu
#   SSH_KEY_PATH=/path/to/your-fleet-key
#   VALIDATOR_IPS=A1,A2,A3      # site A validators (>=3)
#   SPARE_IPS=A4                # site A hot spares (any count, incl. 0)
#   RPC_IPS=A5,A6              # site A pinned archive RPCs (>=2; may co-locate)
#   BACKUP_VALIDATOR_IPS=B1,B2,B3   # site B — set these to enable two-site mode
#   BACKUP_SPARE_IPS=B4
#   BACKUP_RPC_IPS=B5,B6
```

`.env.example` documents every field and shows alternate shapes (e.g. the full
topology co-located on 3 boxes, or everything on one). After editing, run
`./fleet endpoints` to print the resulting per-node layout
(name / site / role / host / port) before deploying. Setting the `BACKUP_*` lists
enables **two-site mode**: a second data center of spare-weight validators the
consensus can be moved onto when the primary site goes down. A failover is a
weight move on the ValidatorManager contract: `./fleet weight validator <site-B
machines>` to hand them the consensus, then `./fleet down <site-A machines>` to
kill the boxes and pull their stake out of quorum. Single-site behavior is
unchanged when
the `BACKUP_*` lists are unset.

> **Staking identities are generated per deploy** by `./00_gen_secrets.sh`
> (gitignored, never committed: on Fuji a leaked staking key means validator
> impersonation). It sizes the key set to the configured topology automatically;
> `02_create_chain.sh` pre-flights that every needed key exists.
> The legacy positional `NODE_IPS` / `BACKUP_SITE_NODE_IPS` (exactly 6 each, fixed
> 3/1/2) still works if `VALIDATOR_IPS` is unset.

## Quick start

Run from the kit root on the control host. This is the condensed sequence; the
full walkthrough — with what to expect at each step — is in
[docs/e2e-runbook.md](docs/e2e-runbook.md).

```bash
./00_gen_secrets.sh                 # per-deploy staking keys + Fuji wallet (gitignored)
./01_fund_wallet.sh                 # manual Fuji faucet (C-chain), auto C->P move
./02_create_chain.sh                # ONCE per chain: create the L1 on Fuji (SPENDS AVAX)
./03_deploy_chain.sh                # deploy all 12 nodes, start chain from genesis (destructive, repeatable)
./04_monitoring.sh                  # Prometheus + Grafana on the control host
./fleet status                      # expect all nodes SERVING, "validators serving: N/N" (3/3 by default)
./05_benchmark.sh                   # drive ~4000 tx/s at the pinned RPC nodes
```

Then run the failover drill (separate terminal, benchmark left running):

```bash
./fleet weight validator 7 8 9            # move the consensus onto site B's validators FIRST
./fleet down 1 2 3                        # site-A outage: hard-kill + drop its stake to dead
# ...later, to fail back once site A is healthy:
./fleet up 1 2 3                          # rebuild site A from genesis; it comes back as spare
./fleet weight validator 1 2 3            # hand the consensus back
./fleet weight spare 7 8 9                # drop site B to standby
```

`03_deploy_chain.sh` is **destructive by design**: it wipes node data and
restarts the chain from genesis (block 0). Re-run any time to reset to a clean
chain; the registration on Fuji's P-chain is preserved, so you never re-run
`00`-`02` (and never re-spend AVAX) for a re-deploy. Editing `chain-config.json`
and re-running `03` is how you apply a new chain config. A fresh chain sits at
block 0 until the benchmark drives load (Avalanche produces blocks on demand).
First boot of a fresh fleet full-replays Fuji's P-chain: RPC tier first
(~minutes), then the validators sync through them.

## Monitoring (Prometheus + Grafana)

`04_monitoring.sh` runs Prometheus + Grafana **on the control host** — not on a
pool node, since a pool node disappears during a site failover — and scrapes
every node's `:9652/ext/metrics`, so the dashboards keep recording the survivors
as a site drops out.

```bash
make monitoring-deps     # one-time (source builds only): fetch prometheus + grafana
./04_monitoring.sh       # generate scrape config, start both, print URLs
```

It discovers the fleet from `fleet endpoints` (the single source of truth for
the configured topology + co-location-aware ports) and labels each target by
`site` (`a`/`b`), `machine` (`m1`…/`b1`…), and `role` (`validator`/`spare`/`rpc`/
`tracker`) — so the dashboards track whatever counts you set. Two dashboards are
provisioned:

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

The whole fleet is driven by the `./fleet` binary. Every registered identity is
permanent (registered once at conversion, never moved); two verbs operate it:

- **`up` / `down`**: the box lifecycle, stake follows. `down` simulates a
  failure (SIGKILL, data left on disk) and drops the identity's on-chain weight
  to `dead`; `up` rebuilds the box from genesis (wipes L1 chain data, keeps the
  Fuji P-chain), starts it, and brings its weight back at `spare`.
- **`weight <validator|spare|dead>`**: moves an identity's on-chain consensus
  weight between three tiers (validator=1000000, spare=1000, dead=1) through
  the ValidatorManager contract. This is the seesaw; it never starts or stops a
  process, so it also expresses the odd states (a down box still holding
  `validator` weight to stall the chain on purpose).

```bash
./fleet status [--watch]                # read-only: per-DC stake tier + reachability
./fleet down 1 2                        # simulate hardware failure on machines 1,2 (stake -> dead)
./fleet up 1 2                          # rebuild machines 1,2, start them (stake -> spare)
./fleet weight validator 7 8 9          # give machines 7,8,9 full consensus weight
./fleet weight spare 1 2 3              # drop machines 1,2,3 to standby weight
./fleet weight dead 1 2 3               # pull machines 1,2,3's stake out of quorum
```

A full site failover is just those primitives composed: `weight validator` the
incoming site **before** `down`ing the outgoing site, or you stall the chain
(post-Durango there is no anyone-can-propose fallback). See
[docs/two-site-failover.md](docs/two-site-failover.md) for the worked drill.

`status` reports every node as `SERVING@block` / `BOOTSTRAPPING` / `DOWN` (or
`off` when intentionally down) alongside its stake tier, per datacenter, so you
see what is *actually* happening rather than what was *intended*.

## Recovering From a Stalled Chain

> The worked example below uses the default **3 validators**; the thresholds
> scale with the configured count `N` — quorum is `ceil(2/3·N)` and the bootstrap
> rejoin latch is `ceil(75%·N)` (both = 3 when N = 3). `status.sh` computes them
> from `N` and prints the right numbers for your topology.

With 3 validators, the L1 behaves very differently depending on whether you are
**taking a validator down** or **bringing one back up**. The asymmetry is not a
bug — it comes from how AvalancheGo bootstraps a (re)starting validator.

### Taking validators down is safe (the chain keeps going)

- **Take 1 of 3 down, hot spare present:** `down` already pulled the dead box's
  stake; `weight validator` the spare → back to 3 active validators →
  **full speed**. (Identities never move: this is a weight seesaw, not a key swap.)
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
`./03_deploy_chain.sh` (which resets the chain to genesis).

## Further reading

- [docs/e2e-runbook.md](docs/e2e-runbook.md) — the full end-to-end failover &
  recovery drill.
- [docs/two-site-failover.md](docs/two-site-failover.md) — two-site design,
  identity map, and what is simulated vs. production.
- [docs/failover-recovery-simulation.md](docs/failover-recovery-simulation.md) —
  the single-site failover model and stalled-chain recovery theory.
- [docs/throughput-tuning-and-benchmarks.md](docs/throughput-tuning-and-benchmarks.md)
  — the 4000-rps profile and block-cadence tuning.
