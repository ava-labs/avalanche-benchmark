# Avalanche L1 Failover Benchmark

Tooling to run an Avalanche L1 across two data centers and test site failover
under transaction load (~4000 tx/s with the default profile). All validators
of both sites are registered on the L1 once, at chain creation. A failover is
a change of consensus weight between them, issued through a ValidatorManager
contract on Fuji's C-chain. Keys never move between machines, so a failover
cannot fork the chain.

Each site runs 3 validators + 1 hot spare + 2 fixed RPC nodes (counts
configurable in `.env`); on the primary site all 4 stake slots (validators and
spare alike) start at validator weight, so the spare validates from day one.
One shared control host handles orchestration, load
generation, and monitoring. The control host runs no L1 node, so it keeps
working when a site is lost. The L1 anchors on Fuji's public P-chain:
validators need no external connectivity, only the RPC machines make one
outbound TCP connection to a pinned public Fuji peer.

Everything below runs from the kit root on the control host, in order:

1. **[Create your chain](#1-create-your-chain-one-time)**: generate secrets,
   fund the wallet, create the L1 on Fuji. One-time. **Skip this step if you
   received a secrets bundle**: untar it over the kit root and go to step 2.
2. **[Deploy and monitor](#2-deploy-and-monitor)**: start all nodes, bring up
   Grafana.
3. **[Benchmark and failover](#3-benchmark-and-failover)**: run the load
   generator, kill a machine, kill a data center, recover.

The full walkthrough with expected output at each step is in
[docs/e2e-runbook.md](docs/e2e-runbook.md).

## Prerequisites

- A control host plus the fleet machines (12 for the default two-site shape,
  fewer with co-location, see below), all reachable over SSH with one key.
- Ports on the fleet machines: **22** (SSH) and **9652-9653** (L1 RPC /
  staking). The RPC machines additionally need ONE outbound TCP to the pinned
  public Fuji peer (default `18.192.93.241:9651`).
- The kit is prebuilt and airgap-ready, no Go toolchain needed:
  `sudo rpm -i avalanche-benchmark-*.rpm` (installs to
  `/opt/avalanche-benchmark`), or extract `remote-benchmark.tar.gz` anywhere.
  To build from source: `make`.

Configure once:

```bash
cp .env.example .env
# Edit .env: SSH user/key plus one IP list per role, per site.
#   VALIDATOR_IPS=A1,A2,A3            # site A validators (>=3)
#   SPARE_IPS=A4                      # site A hot spares
#   RPC_IPS=A5,A6                     # site A pinned RPCs (>=2)
#   BACKUP_VALIDATOR_IPS=B1,B2,B3     # site B: set these to enable two-site mode
#   BACKUP_SPARE_IPS=B4
#   BACKUP_RPC_IPS=B5,B6
```

Each list's LENGTH is the node count, its VALUES are the placement. Repeat an
IP to co-locate several nodes on one box (ports and data dirs auto-offset), so
the full topology fits on as few machines as you have; `.env.example` shows
the worked co-located shapes. Run `./fleet endpoints` to preview the resulting
layout before deploying. Machines are numbered in list order: site A is 1-6,
site B is 7-12; the failover commands below use those numbers. Names encode
the role: site A stake slots (validators + spare) are `a1..a4` and its RPCs
`rpc_a1 rpc_a2`; site B mirrors them as `b1..b4` and `rpc_b1 rpc_b2`.

## 1. Create your chain (one-time)

> Received a secrets bundle (`staking/`, `network.env`, wallet key) from your
> vendor? Untar it over the kit root and **skip to step 2**. Your chain
> already exists on Fuji, and nothing in step 2 or 3 ever re-creates or
> re-pays for it.

Three scripts, run in order after `.env` is configured (key generation sizes
itself to your topology):

```bash
./setup/00_gen_secrets.sh    # staking identities + Fuji wallet
./setup/01_fund_wallet.sh    # prints the address, you hit the Fuji faucet, it does the rest
./setup/02_create_chain.sh   # creates the L1 on Fuji. SPENDS AVAX. Once per chain.
./setup/03_backup_secrets.sh # bundle staking/ + network.env into a tar.gz, store it off-machine
```

The backup tarball is also the secrets bundle you hand to an operator:
untarring it over a fresh kit root is the whole restore.

`02` writes the chain's identity to `network.env`; everything after
this point only reads it. The generated `staking/` and wallet key are secrets:
gitignored, never in any archive, a leaked staking key means validator
impersonation. Re-running `00` generates a new identity set and orphans the
old chain; only do that to start over with a new chain.

## 2. Deploy and monitor

```bash
./run/01_deploy.sh        # wipe + deploy every node, start the chain from genesis
./run/02_monitoring.sh    # Prometheus + Grafana on the control host
./fleet status     # expect all nodes SERVING, "validators serving: 4/4"
```

`run/01_deploy.sh` is **destructive by design**: it wipes node data and restarts the
chain from genesis (block 0). Re-run it any time you want a clean chain, or
after editing `chain-config.json`; the Fuji registration persists, so
re-deploys never re-spend AVAX. First boot of a fresh fleet replays Fuji's
P-chain (RPC tier first, ~minutes, then validators sync through them). Progress is visible in
`./fleet status --watch`. A fresh chain sits at block 0 until load arrives
(Avalanche produces blocks on demand).

Grafana is on the control host at `:3000`, Prometheus at `:9090` (anonymous
admin). If those ports aren't open to you, tunnel:

```bash
ssh -i <key> -L3000:localhost:3000 <user>@<control-host>   # open http://localhost:3000
```

Three dashboards are provisioned: **Failover Overview** (per-server serving
state and stake tier timelines, successful polls %, chain TPS, plus a
"Load generator (bombard)" row with bombard's end-to-end tx latency p50/p95,
mined TPS, and resubmits), **Failover
Details** (per-node finalized height, the A-to-B finalized gap, mempools),
and **Benchmark** (per-node TPS, consensus, verification). `run/02_monitoring.sh` is re-runnable and discovers the fleet
from your `.env` topology automatically.

## 3. Benchmark and failover

Start the load (leave it running through everything below):

```bash
./run/03_bombard.sh
```

One fixed profile, no flags: **4000 tx/s**, 2000 in-flight cap, 5s resubmit.
Ingress is **every pinned RPC node on both sites** (`rpc_a1` `rpc_a2` `rpc_b1` `rpc_b2` in the
default shape). bombard broadcasts each tx to ALL of them, health-checks every
endpoint continuously, drops one from rotation when it falls behind and
re-adds it when it catches up, and resubmits anything in flight, so ingress
survives dead nodes, dead sites, and recovering nodes without you touching it.
The RPC nodes are never promoted to validators, which is what keeps that
ingress path alive through a failover. To change the profile, edit the
constants at the top of `run/03_bombard.sh`.

Operating the fleet is two independent axes, both via `./fleet`:

- **hardware**: `up` / `down` start or hard-kill avalanchego on a machine.
  `down` is a real crash (SIGKILL); `up` rebuilds the machine clean from
  genesis, re-syncs it from the network and blocks until it is SERVING.
  Neither touches stake.
- **stake**: `weight <tier> <machines...>` moves the listed machines'
  on-chain consensus weight to one tier (`validator`=1000000,
  `spare`=1000, `dead`=1) through the ValidatorManager contract. One tier
  per invocation; it never starts or stops a process.

```bash
./fleet status --watch                     # live per-DC stake tier + node state
./fleet down 1                             # crash machine 1
./fleet weight validator 7                 # promote a spare first...
./fleet weight dead 1                      # ...then retire the dead box
./fleet up 1                               # rebuild machine 1, it re-syncs and rejoins
```

**Raise the replacement validators before lowering the old ones.** Run the
`weight validator ...` command first and the lowering command second, so
the fleet never passes through a low-weight window. Weights only move when
you ask: a dead box keeps blocking quorum until you `weight dead` it.

The worked drills live in `scenarios/`. Each one is idempotent: it first
restores the ground state (all machines up, machines 1-4 validating,
everything else spare), then executes the failure, so any scenario can be
run from any starting point:

```bash
./scenarios/00_healthy.sh                  # ground state only
./scenarios/01_validator_down.sh           # one validator dies, 3 of 4 remain
./scenarios/02_validator_down_replace.sh   # site B machine steps in, back to 4
./scenarios/03_datacenter_failure.sh       # site A dies, site B takes over
./scenarios/04_datacenter_failback.sh      # site A returns, consensus moves home
```

Recovery from any scenario is `./scenarios/00_healthy.sh`.

### What to expect when validators drop

With 4 active validators (thresholds scale with your configured count):

| Action | Result |
|--------|--------|
| 1 of 4 down, replacement promoted | back to 4 active, full speed |
| 1 of 4 down, no replacement | keeps full quorum on 3/4, consensus rides through |
| 2 of 4 down | quorum lost, chain **HALTS** (expected, recoverable) |
| bring back one validator into a halted chain | still halted, see below |
| bring back the rest | chain resumes within seconds |

Row 4 is the non-obvious one: a **(re)starting validator can't bootstrap until ~75%
of validator stake is online** (3 of 4, with 4 equal validators). Normal
failover never hits this because the other three are still running; but
recovering a fully halted chain needs enough validators up to clear the
latch, and nothing happens until they connect, at which point they clear it
together. `./fleet status` shows a node stuck on the latch as
`BOOTSTRAPPING`.

One more recovery: a validator hard-killed under heavy load can come back
with a diverged local chain (its VM dies repeatedly, `status` shows it
`BOOTSTRAPPING` forever while others are `SERVING`). `./fleet up <m>` fixes
it: the rebuild wipes its chain data and it re-syncs from the live network
with the same identity.

## Block cadence

Site A proposes 25ms blocks (the hot ~40 blk/s profile); site B proposes
100ms blocks, and only after a failover makes it the producer. The slower
backup cadence is what lets a recovering site converge without a rolling
restart. Tune `min-delay-target` in `chain-config.json` and re-run
`./run/01_deploy.sh`. Throughput is gas-bound, not block-rate-bound: see
[docs/throughput-tuning-and-benchmarks.md](docs/throughput-tuning-and-benchmarks.md).

## Further reading

- [docs/e2e-runbook.md](docs/e2e-runbook.md): the full end-to-end drill with
  expected output, install to failback.
- [docs/two-site-failover.md](docs/two-site-failover.md): the two-site design,
  block cadence split, and what is simulated vs production.
- [docs/failover-recovery-simulation.md](docs/failover-recovery-simulation.md):
  the failover model: weight seesaw, warp message path, halt/recovery theory.
- [docs/throughput-tuning-and-benchmarks.md](docs/throughput-tuning-and-benchmarks.md):
  the 2026-06-03 throughput study behind the 4000 tx/s profile (historical).
- [FUJI_PLAN.md](FUJI_PLAN.md): the original design plan for anchoring on
  Fuji's public P-chain (historical).
