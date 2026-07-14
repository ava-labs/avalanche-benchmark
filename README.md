# Avalanche L1 Failover Benchmark

Tooling to run an Avalanche L1 across two data centers and test site failover
under transaction load (~4000 tx/s with the default profile). All validators
of both sites are registered on the L1 once, at chain creation. A failover is
a change of consensus weight between them: the chain is created with its
validator manager recorded on the L1's OWN chain (address 0x..01, no contract
exists there), we hold every validator's BLS key, so the `bin/l1` tool signs
each weight change locally with all of them and submits the
SetL1ValidatorWeightTx straight to the P-chain. No ValidatorManager contract,
no courier, no signature aggregator; the only external dependency is one
P-chain RPC. Keys never move between machines, so a failover cannot fork the
chain.

Each site runs 3 validators + 1 hot spare + 2 fixed RPC nodes (counts
configurable in `.env`); on the primary site all 4 stake slots (validators and
spare alike) start at validator weight, so the spare validates from day one.
One shared control host handles orchestration, load
generation, and monitoring. The control host runs no L1 node, so it keeps
working when a site is lost.

The L1 anchors on a public Avalanche P-chain, Fuji (testnet) or mainnet, and
needs it only as a read feed. Validators have zero internet connectivity by
design; the RPC tier follows the P-chain through ONE pinned upstream peer,
the only outbound connection in the whole fleet. In production the operator
runs that upstream themselves: a follow-only avalanchego node is a tiny
footprint (1 CPU, 2 GB RAM, ~60 GB disk), one is enough, four for redundancy.

Everything below runs from the kit root on the control host, in order:

1. **[Create your chain](#1-create-your-chain-one-time)**: generate secrets,
   fund the wallet, create the L1 on Fuji or mainnet. One-time. **Skip this
   step if you received a secrets bundle**: untar it over the kit root and go
   to step 2 (see [Install from a release](#install-from-a-release)).
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
  upstream peer (default `18.192.93.241:9651` on Fuji,
  `54.232.137.108:9651` on mainnet).
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

## Install from a release

To operate a deployment whose chain already exists (you have the secrets
bundle from `setup/03_backup_secrets.sh`), the GitHub release is the whole
install; no Go toolchain, no chain creation:

1. Download `remote-benchmark.tar.gz` from the
   [latest release](https://github.com/ava-labs/avalanche-benchmark/releases/latest)
   and extract it into an empty directory (it unpacks flat).
2. Untar the secrets bundle over the kit root: it restores `staking/` and
   `network.env` (the chain identity, including which network it lives on).
3. Copy in your `.env` (or `cp .env.example .env` and fill in the fleet IPs).
4. `./fleet status` now drives the existing fleet; `./run/01_deploy.sh` and
   everything after work as below.

**Restarting an existing fleet (preserved chain data):** use `./fleet up`
(or `./scenarios/00_healthy.sh`), never `./run/01_deploy.sh`. Deploy wipes
every node's `data/` and starts from genesis; on mainnet that costs ~6h of
P-chain resync before the fleet serves again. `fleet up` restarts nodes on
the data they already have.

Creating a NEW chain instead is section 1: `00_gen_secrets` -> `01_fund_wallet`
-> `02_create_chain` -> `03_backup_secrets`, then deploy.

## 1. Create your chain (one-time)

> Received a secrets bundle (`staking/`, `network.env`, wallet key) from your
> vendor? Untar it over the kit root and **skip to step 2**. Your chain
> already exists, and nothing in step 2 or 3 ever re-creates or re-pays for
> it.

Three scripts, run in order after `.env` is configured (key generation sizes
itself to your topology):

```bash
./setup/00_gen_secrets.sh    # staking identities + fund/fee wallet
./setup/01_fund_wallet.sh    # prints the addresses, you hit the Fuji faucet, it does the rest
./setup/02_create_chain.sh   # creates the L1 (add --mainnet to both for mainnet). SPENDS AVAX. Once per chain.
./setup/03_backup_secrets.sh # bundle staking/ + network.env into a tar.gz, store it off-machine
```

The backup tarball is also the secrets bundle you hand to an operator:
untarring it over a fresh kit root is the whole restore.

`02` writes the chain's identity (including `NETWORK=fuji|mainnet`) to
`network.env`; everything after this point only reads it. The generated
`staking/` and wallet key are secrets: gitignored, never in any archive, a
leaked staking key means validator impersonation. Re-running `00` generates a
new identity set and orphans the old chain; only do that to start over with a
new chain.

The chain is disposable by design: each validator's continuous-fee deposit
(0.1 AVAX on Fuji, ~5-6 days; 0.15 AVAX on mainnet, ~3 days at the 512
nAVAX/s fee floor) drains in days, after which validators deactivate and the
L1 halts. Extend a run with `bin/fuji-wallet topup [days]`: it tops every
validator up to at least that many days of runway (default 3), skipping any
already at or above the target, funded from the same wallet.

### Mainnet

Pass `--mainnet` to `01` and `02` to anchor the L1 on Avalanche mainnet
instead of Fuji; `02` records the choice in `network.env` and every later
command follows it. There is no faucet: `01` prints the P-chain address and
you fund it with your own AVAX (0.15 AVAX per registered validator plus
~0.1 AVAX in fee budget). The RPC machines' single outbound
TCP goes to the pinned public mainnet peer (default `54.232.137.108:9651`);
update the firewall egress rule accordingly.

## 2. Deploy and monitor

```bash
./run/01_deploy.sh        # wipe + deploy every node, start the chain from genesis
./run/02_monitoring.sh    # Prometheus + Grafana on the control host
./fleet status     # expect all nodes SERVING, "validators serving: 4/4"
```

`run/01_deploy.sh` is **destructive by design**: it wipes node data and restarts the
chain from genesis (block 0). Re-run it any time you want a clean chain, or
after editing `chain-config.json`; the on-chain registration persists, so
re-deploys never re-spend AVAX. First boot of a fresh fleet syncs the anchor
P-chain (RPC tier first, ~minutes, then validators sync through them). Progress is visible in
`watch -n5 ./fleet status`. A fresh chain sits at block 0 until load arrives
(Avalanche produces blocks on demand).

`./fleet status` is single-shot; wrap it in `watch -n5` for a live view. Node
states: `SERVING` (at the fleet tip), `CATCHING UP (behind N)` (answering RPC
but N blocks behind tip), `BOOTSTRAPPING`, `DOWN`.

Grafana is on the control host at `:3000`, Prometheus at `:9090` (anonymous
admin). If those ports aren't open to you, tunnel:

```bash
ssh -i <key> -L3000:localhost:3000 <user>@<control-host>   # open http://localhost:3000
```

Four dashboards are provisioned: **Failover Overview** (per-server serving
state and stake tier timelines, successful polls %, chain TPS, plus a
"Load generator (bombard)" row with bombard's end-to-end tx latency p50/p95,
mined TPS, and resubmits), **Failover
Details** (per-node finalized height, the A-to-B finalized gap, mempools),
**Benchmark** (per-node TPS, consensus, verification), and **Machine
Metrics** (per-box CPU, memory, disk, throttle pressure).
`run/02_monitoring.sh` is re-runnable and discovers the fleet
from your `.env` topology automatically.

## 3. Benchmark and failover

Start the load (leave it running through everything below):

```bash
./run/03_bombard.sh
```

One fixed profile: **4000 tx/s** (override with `-tps N`), 2000 in-flight cap, 5s resubmit.
Ingress is **every pinned RPC node on both sites** (`rpc_a1` `rpc_a2` `rpc_b1` `rpc_b2` in the
default shape). bombard broadcasts each tx to ALL of them, health-checks every
endpoint continuously, drops one from rotation when it falls behind and
re-adds it when it catches up, and resubmits anything in flight, so ingress
survives dead nodes, dead sites, and recovering nodes without you touching it.
The RPC nodes are never promoted to validators, which is what keeps that
ingress path alive through a failover. To change the profile, edit the
constants at the top of `run/03_bombard.sh`. bombard is single-account, so
the script refuses to start a second instance (two would issue duplicate
nonces and mine nothing).

Operating the fleet is two independent axes, hardware via `./fleet` and
stake via `bin/l1`:

- **hardware**: `up` / `down` start or hard-kill avalanchego on a machine.
  `down` is a real crash (SIGKILL); `up` rebuilds the machine clean,
  state-syncs it from the live network and blocks until it is SERVING at the
  fleet tip. Neither touches stake. While waiting, `up` watches for a
  fork-wedged node (a catching-up node whose height stays frozen across
  consecutive polls: it self-finalized a sibling block and will never reach
  tip); instead of timing out it rebuilds the node in place (wipe its L1
  chainData only, keep the P-chain db and staking keys, restart, state
  re-sync).
- **stake**: `bin/l1 apply --weights a1=100000,b1=1,...` (or
  `bin/l1 set-weight --node <name> --weight <w>` for one validator) moves
  on-chain consensus weight between the tiers (validator=100000, spare=1000,
  dead=1). The tool holds every validator's BLS signer key, so it signs each
  weight message locally with all of them, submits the
  SetL1ValidatorWeightTx to the P-chain itself and verifies the new weight
  on-chain before the next tx. `apply` orders every raise before any lower,
  so the fleet never passes through a low-weight window mid-seesaw. It never
  starts or stops a process, and no keys move. NOTE: unlike the old
  contract flow there is no raise-gate; do not raise a node that is behind
  the fleet tip (a behind node with more stake wins proposer slots on stale
  heights and can self-finalize a fork) - check `./fleet status` first.

A hard-down box (ssh-unreachable, not just process-dead) never blocks
operating the rest of the fleet: reconciliation records it as down with a
warning and proceeds; only commands that target that box specifically fail
(non-zero, naming it, after the reachable targets are handled).

```bash
watch -n5 ./fleet status                   # live per-DC stake tier + node state
./fleet down 1                             # crash machine 1
./bin/l1 apply --weights b1=100000,a1=1    # promote a spare, retire the dead box
./fleet up 1                               # rebuild machine 1, it re-syncs and rejoins
```

**Raise the replacement validators before lowering the old ones.** A single
`bin/l1 apply` does this for you: it always applies raises first, then
lowers. Weights only move when you ask: a dead box keeps blocking quorum
until you drain it (weight 1).

The worked drills live in `scenarios/`. `00_healthy.sh` is the canonical
reset (all machines up, machines 1-4 validating, site B stake slots spare);
every other scenario invokes it first, so they can be run from any starting
point.

```bash
./scenarios/00_healthy.sh                  # ground state only
./scenarios/01_validator_down.sh           # one validator dies, 3 of 4 remain
./scenarios/02_validator_down_replace.sh   # site B machine steps in, back to 4
./scenarios/03_datacenter_failure.sh       # site A dies, site B takes over
./scenarios/04_validator_maintenance.sh    # planned maintenance: drain a1, then power it off
./scenarios/05_2x2.sh                      # 2x2 split: consensus spread across both DCs
./scenarios/06_all_validators.sh           # all eight stake slots validating, four per DC
./scenarios/07_two_validators_down.sh      # two validators die at once: chain halts, P-chain weight ops revive it
```

Recovery from any scenario is `./scenarios/00_healthy.sh`; failing back
after 03 is just running it.

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
`BOOTSTRAPPING`. The other way out of a halt, without restarting anything, is
draining the dead machines (`bin/l1 apply --weights a1=1,a2=1`): the weight
txs go straight to the P-chain, so they land even while the L1 is halted
(scenario 07).

One more recovery: a validator hard-killed under heavy load can come back
with a diverged local chain (its VM dies repeatedly, `status` shows it
`BOOTSTRAPPING` forever while others are `SERVING`). `./fleet up <m>` fixes
it: the rebuild wipes its chain data and it re-syncs from the live network
with the same identity. The related fork-wedge case (`CATCHING UP` with a
height that never moves: the node self-finalized a sibling block) is
detected and rebuilt by `up` automatically.

Every node starts with two self-defense guards baked into its launch script:
the stdout capture (`data/validator/logs/avalanchego.out`) is truncated if it
ever exceeds 2 GiB (subnet-evm runs at `log-level: warn`, so the capture stays
small; WARN/ERROR still land in the rotated chain logs), and the process tree
runs with `GOMEMLIMIT` at 75% of the box's RAM plus a raised `oom_score_adj`,
so a runaway node is GC-throttled and, at worst, killed by the kernel instead
of wedging the whole machine.

## Block cadence

All nodes propose 25ms blocks (the hot ~40 blk/s profile), uniform across
both sites. Tune `min-delay-target` in `chain-config.json` and re-run
`./run/01_deploy.sh`. Throughput is gas-bound, not block-rate-bound: see
[docs/throughput-tuning-and-benchmarks.md](docs/throughput-tuning-and-benchmarks.md).

## Further reading

- [docs/e2e-runbook.md](docs/e2e-runbook.md): the full end-to-end drill with
  expected output, install to failback.
- [docs/two-site-failover.md](docs/two-site-failover.md): the two-site design,
  block cadence, and what is simulated vs production.
- [docs/failover-recovery-simulation.md](docs/failover-recovery-simulation.md):
  the failover model: weight seesaw, warp message path, halt/recovery theory.
- [docs/throughput-tuning-and-benchmarks.md](docs/throughput-tuning-and-benchmarks.md):
  the 2026-06-03 throughput study behind the 4000 tx/s profile (historical).
- [FUJI_PLAN.md](FUJI_PLAN.md): the original design plan for anchoring on
  Fuji's public P-chain (historical).
