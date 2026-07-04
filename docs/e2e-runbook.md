# End-to-End Runbook: Two-Datacenter L1 Failover & Recovery

A complete cross-region disaster-recovery drill for an Avalanche L1: stand the
chain up across two data centers, drive it under sustained load, simulate a
total loss of the primary site, fail the validator set over to the backup site,
and gracefully fail back — with a benchmark measuring the impact end to end.

Everything is driven from one **control host**. It runs the orchestration, the
load generator, and the monitoring stack, and it holds no L1 node — so it keeps
coordinating and recording even when an entire site goes dark.

---

## Architecture

| Component | Count | Role |
|-----------|-------|------|
| **Control host** | 1 | Orchestration scripts, load generator (bombard), Prometheus + Grafana. Also runs the 5 local primary-network (P-chain) validators the L1 bootstraps against. Holds no L1 node. |
| **Site A** (primary) | 6 | `m1`–`m3` weighted validators · `m4` hot spare · `m5`/`m6` pinned archive RPC nodes |
| **Site B** (backup) | 6 | `b1`–`b3` zero-weight syncing trackers · `b4` spare · `b5`/`b6` pinned archive RPC nodes |

Put the two sites in **different regions** so cross-site sync latency is real.

Two invariants make the drill faithful to a production failover:

- **Consensus runs in one site at a time.** The three validator identities
  (staking keys) are *conserved* — they move between machines and across sites
  on a failover, but are never duplicated. The chain stays a single branch with
  no equivocation.
- **Site B tracks site A live.** B's nodes are zero-weight trackers at the tip
  during normal operation, so when the validator keys arrive on failover their
  chain data is already current — that's what makes the cutover fast.

---

## Prerequisites

- **13 Linux hosts**: 1 control host + 12 nodes (`linux-amd64`), 6 per site,
  the two sites in two regions.
- The control host can **SSH to all 12 nodes** with a single private key.
- Open on each node: port **22** (SSH) and **9652–9653** (L1 RPC / staking).
- **Grafana `:3000`** and **Prometheus `:9090`** reachable on the control host
  (or tunnel them over SSH — see Step 4).

---

## Install (control host)

```bash
sudo rpm -i avalanche-benchmark-2026.06.23.x86_64.rpm
cd /opt/avalanche-benchmark
```

Airgap-friendly: the RPM bundles Prometheus + Grafana and the pinned AvalancheGo
build, with no runtime dependencies. (On a non-RHEL control host, extract
`remote-benchmark.tar.gz` and run from the extracted directory instead — the
commands below are identical.)

## Configure

```bash
cp .env.example .env
# then edit .env
```

Fill in the SSH settings and the per-role IP lists (`.env.example` documents every
field and shows alternate shapes):

```ini
SSH_USER=ubuntu
SSH_KEY_PATH=/path/to/your-fleet-key

# Site A — list LENGTH is the count, VALUES are the placement (repeat an IP to
# co-locate). VALIDATOR_IPS >= 3, RPC_IPS >= 2 (a redundant RPC is required so one
# can be snapshotted on restore while its twin serves), SPARE_IPS >= 0.
VALIDATOR_IPS=A1,A2,A3
SPARE_IPS=A4
RPC_IPS=A5,A6

# Site B (backup) — set these to enable two-site mode; same shape as A.
BACKUP_VALIDATOR_IPS=B1,B2,B3
BACKUP_SPARE_IPS=B4
BACKUP_RPC_IPS=B5,B6
```

The counts are whatever you list — the example above is the default 3 validators
+ 1 spare + 2 RPCs. Run `./bin/reconcile endpoints` after editing to print the
resulting per-node layout before deploying. (The legacy positional `NODE_IPS` /
`BACKUP_SITE_NODE_IPS` — exactly 6 each — still works if `VALIDATOR_IPS` is unset.)

---

## The drill

Run everything from the kit root on the control host. Steps 1–4 stand the
network up; steps 5–8 are the drill itself. Use **two terminals** for the drill:
one for the benchmark (left running), one for the failover commands.

### Step 1 — generate secrets and fund the Fuji wallet (one time)

```bash
./00_gen_secrets.sh
./01_fund_wallet.sh
```

`00` generates the per-deploy staking identities + the Fuji fund/fee wallet into
gitignored paths (`staking/`) and writes the NodeID manifest. `01` prints the
wallet's C-chain address: fund it manually at the Fuji faucet
(https://core.app/tools/testnet-faucet/, the faucet is C-chain only), and the
script then moves everything C -> P automatically. Budget: ~0.1 AVAX fees +
1 AVAX per validator.

### Step 2 — create the L1 on Fuji (one time, SPENDS AVAX)

```bash
./02_create_chain.sh
```

Registers the validator identities (the count from `VALIDATOR_IPS`; 3 by default)
on Fuji's public P-chain via `api.avax-test.network` and writes `network.env`
(the new `SUBNET_ID` / `CHAIN_ID`). It pre-flights the staking keys and the
wallet balance before issuing anything. Run once per chain; re-deploys never
repeat this step.

### Step 3 — deploy the L1 across both sites

```bash
./03_deploy_chain.sh
```

Uploads the binaries, plugin, and keys to all 12 nodes and starts the chain from
genesis (block 0): **site A validating, site B tracking**. **Destructive by
design** — it wipes node data and restarts the chain; losing the L1 state is the
point. Re-run any time to reset to a clean chain; the registration on Fuji is
preserved, so you do **not** re-run steps 1–2 (and spend nothing). First boot of
a fresh fleet full-replays Fuji's P-chain: the RPC tier syncs first (~minutes),
then the validators sync through it (serial per hop) — nodes sit in
BOOTSTRAPPING until then, which is normal.

### Step 4 — start monitoring

```bash
./04_monitoring.sh
```

Runs Prometheus + Grafana on the control host and prints the URLs. Two
dashboards are provisioned:

- **Avalanche Benchmark** (`/d/avalanche-benchmark`) — per-node TPS, consensus,
  verification.
- **Avalanche Failover** (`/d/avalanche-failover`) — per-node finalized height,
  the **A→B finalized gap**, node up/down, block-acceptance rate. **This is the
  one to watch during the drill** — you see site A flatline and site B take over
  live.

Grafana is on `:3000`, Prometheus on `:9090` (anonymous, no login). If those
ports aren't open to you, tunnel over SSH:

```bash
ssh -i <key> -L3000:localhost:3000 -L9090:localhost:9090 <user>@<control-host>
# then open http://localhost:3000
```

### Step 5 — confirm health

```bash
./scripts/failover/status.sh
```

Read-only. Expect all 12 nodes `SERVING` and **`validators serving: 3/3`** (the
validators are `m1`–`m3` on site A). `status.sh` reports each node's *actual*
state (`SERVING@block` / `BOOTSTRAPPING` / `DOWN`), so it's the source of truth
throughout the drill.

> A freshly deployed chain sits at `block=0` until Step 6 drives load —
> Avalanche produces blocks on demand, so an idle chain showing `SERVING` at
> `block=0` is healthy, not stuck.

### Step 6 — drive load (terminal 1, leave running)

```bash
./05_benchmark.sh
```

Sends ~**4000 tx/s** to the **pinned archive RPC nodes** (`m5`/`m6` on A, plus
`b5`/`b6` on B) — never to the validators, so consensus stays healthy and
ingress survives a failover. bombard fans sends across every reachable RPC, runs
a watcher per endpoint, and resubmits in-flight transactions, so it rides
straight through the failover and its latency report captures the whole recovery
window. Leave it running — it's your live witness.

### Step 7 — simulate the disaster: fail site A over to site B (terminal 2)

```bash
./scripts/failover/site-failover.sh b
```

This models a **real region outage**, not a graceful drain:

1. **Nuke site A first** — every site-A node is hard-killed (SIGKILL) at once,
   freezing A's tip at the true data-loss boundary before anything else happens.
2. **Site B reconciles itself** — already a live tracker, B forms consensus on
   the blocks it *already holds*. **Zero state is pulled from the dead site.** A
   validator within 100 blocks of the tip is promoted as-is (AvalancheGo
   self-heals the small gap); one further behind is wiped and state-synced from
   site B's **own** archive RPC. The decision is logged per validator.

What you'll see:

- **Failover dashboard** — site A heights flatline; site B picks up production
  and the A→B gap closes.
- **`status.sh`** — `b1`–`b3` become the serving validators (`3/3`); site A
  shows `DOWN`.
- **bombard** (terminal 1) — sends fail over to `b5`/`b6`, in-flight
  transactions resubmit, and throughput recovers at site B's cadence.

> While you are failed over to B (a DR posture) the chain produces at ~10 blk/s
> vs. site A's ~40 blk/s. TPS is unaffected — same load, larger blocks.

### Step 8 — graceful failback to site A

Once the site-A hosts are reachable again:

```bash
./scripts/failover/restore.sh a
```

Rolls the validator set back to site A **one validator at a time**, holding the
chain at ≥2/3 throughout — **no chain downtime, no fork**. It seeds each
recovering node from a live snapshot, waits for it to reach the tip, then moves
one validator key at a time with a health gate between each. The hot profile
(~40 blk/s) resumes on its own once the keys land on A.

> For a *true* site-A outage (A actually gone, its data stale), the hard
> failback is `./scripts/failover/site-failover.sh a` — but read the rollback
> caveat in [two-site-failover.md](two-site-failover.md) first. With both sites
> healthy, always use the graceful `restore`.

### Step 9 — tear down (optional)

```bash
./06_cleanup.sh
```

Stops every node, removes the remote deployment directories, and deletes the
local `network.env`. It does not touch `staking/` (the generated secrets) or
anything on Fuji; the chain remains registered there.

---

## Reading the results as DR metrics

- **RPO (data loss)** — compare the block height where site A froze at the nuke
  to where site B resumed. Nuke-first makes this honest: B never imports a single
  block A produced after the cut, so the gap you measure is the real recovery
  point, not an artifact of letting A drain.
- **RTO (time to recover)** — from issuing `site-failover.sh b` to
  `validators serving: 3/3` on site B with height advancing. Watch it on the
  failover dashboard and confirm with `status.sh`.
- **No fork / no equivocation** — prove it directly:

  ```bash
  ./scripts/failover/verify.sh
  ```

  Read-only proof the live network is **one branch** with a healthy quorum.

---

## Command reference

```bash
./scripts/failover/status.sh             # read-only: actual state of every node
./scripts/failover/verify.sh             # read-only: prove one branch + quorum
./scripts/failover/site-failover.sh b    # hard cutover: nuke A, fail the set to B
./scripts/failover/site-failover.sh a    # hard cutover the other way (true outage)
./scripts/failover/restore.sh a          # graceful rolling failback to A (no downtime)
./scripts/failover/down.sh <m>           # take one machine offline (within-site failover)
./scripts/failover/up.sh <m>             # return a machine to service
./scripts/failover/clean.sh <m>          # wipe one node's chain data and re-bootstrap it
```

## Further reading

- [two-site-failover.md](two-site-failover.md) — design, mechanics, identity
  map, and what is simulated vs. production.
- [failover-recovery-simulation.md](failover-recovery-simulation.md) — the
  single-site failover model (taking validators down/up within one site).
- [throughput-tuning-and-benchmarks.md](throughput-tuning-and-benchmarks.md) —
  the 4000-rps profile and how block cadence drives throughput.
