# End-to-End Runbook: Two-Datacenter L1 Failover and Recovery

The full drill, install to failback, with the expected output at each step.
Commands and vocabulary are the README's; this adds the step-by-step detail
and what healthy looks like on the way.

Everything runs from the kit root on one **control host**. It holds the
orchestration, the load generator, and the monitoring stack, and runs no L1
node, so it keeps coordinating and recording when a site goes dark.

## Architecture

| Component | Count | Role |
|-----------|-------|------|
| Control host | 1 | `./fleet`, bombard, Prometheus + Grafana. No L1 node. |
| Site A (primary) | 6 | machines 1-6: `a1..a4` stake slots (all four at validator weight), `rpc_a1`/`rpc_a2` pinned RPCs |
| Site B (backup) | 6 | machines 7-12: `b1..b4` stake slots at spare weight, `rpc_b1`/`rpc_b2` pinned RPCs |

All 8 stake slots are registered on Fuji's P-chain once, at chain creation.
Failover moves consensus weight between them via the ValidatorManager
contract on Fuji's C-chain; keys never move, so no drill can fork the chain.
Put the sites in different regions so the cross-site latency is real.

## Prerequisites

- 13 Linux hosts (`linux-amd64`): 1 control host + 12 nodes.
- Control host SSHes to all 12 with one key.
- Open on each node: 22 (SSH), 9652-9653 (L1 RPC / staking). RPC machines
  additionally need one outbound TCP to the pinned public Fuji peer
  (default `18.192.93.241:9651`).
- Grafana `:3000` / Prometheus `:9090` reachable on the control host, or
  tunneled (step 4).

## Install (control host)

```bash
sudo rpm -i avalanche-benchmark-*.rpm
cd /opt/avalanche-benchmark
```

Airgap-friendly: the RPM bundles Prometheus, Grafana, and the pinned
AvalancheGo build. On a non-RHEL host, extract `remote-benchmark.tar.gz`
anywhere and run from there; the commands are identical.

## Configure

```bash
cp .env.example .env
# then edit .env
```

Fill in the SSH settings and the per-role IP lists (`.env.example` documents
every field and shows co-located shapes):

```ini
SSH_USER=ubuntu
SSH_KEY_PATH=/path/to/your-fleet-key

# List LENGTH is the node count, VALUES are the placement (repeat an IP to
# co-locate). VALIDATOR_IPS >= 3, RPC_IPS >= 1 per site.
VALIDATOR_IPS=A1,A2,A3
SPARE_IPS=A4
RPC_IPS=A5,A6

# Site B: set these to enable two-site mode; same shape as A.
BACKUP_VALIDATOR_IPS=B1,B2,B3
BACKUP_SPARE_IPS=B4
BACKUP_RPC_IPS=B5,B6
```

Preview the resulting layout before touching anything:

```bash
./fleet endpoints
```

One tab-separated line per node: name, site, role, host, HTTP port.

## Step 1: secrets and funding (one time)

> Received a secrets bundle (`staking/`, `network.env`, wallet key)? Untar it
> over the kit root and skip to step 3. Your chain already exists on Fuji.

```bash
./setup/00_gen_secrets.sh
./setup/01_fund_wallet.sh
```

`00` generates one permanent staking identity per pool slot plus the Fuji
wallet, all into gitignored `staking/`. `01` prints two faucet targets and
polls until both are funded at https://core.app/tools/testnet-faucet/
(2 AVAX per request, pick the chain per request):

- **P-chain address**: 0.1 AVAX per registered validator (a multi-day
  continuous-fee deposit) plus fees.
- **C-chain address**: gas for the ValidatorManager deploy and every weight
  move.

The script exits on its own once both balances clear.

## Step 2: create the L1 on Fuji (one time, SPENDS AVAX)

```bash
./setup/02_create_chain.sh
```

Creates the subnet and chain on Fuji's public P-chain, deploys the
ValidatorManager to the Fuji C-chain, and registers all 8 stake slots in one
conversion (site A at validator weight, site B at spare weight). Expected
tail:

```
=== L1 Created ===

Subnet ID: <...>
Chain ID:  <...>
Manager:   0x<...> (ValidatorManager on the Fuji C-chain)

Saved to: .../network.env
```

Then bundle the secrets and store the tarball off-machine:

```bash
./setup/03_backup_secrets.sh
```

That tarball is also the operator hand-off: untarring it over a fresh kit
root is the whole restore.

## Step 3: deploy

```bash
./run/01_deploy.sh
```

Wipes every node, uploads binaries/plugin/keys/configs, and starts the chain
from genesis. **Destructive by design**; re-run any time for a clean chain.
The Fuji registration persists, so re-deploys never re-spend AVAX. First
boot of a fresh fleet replays Fuji's P-chain: RPC tier first (~minutes),
then validators sync through them; nodes sit in `BOOTSTRAPPING` until then.
Watch the progress with `./fleet status --watch`. (Later single-machine
recoveries via `./fleet up` do block until the machine is SERVING.)

## Step 4: monitoring

```bash
./run/02_monitoring.sh
```

Starts Prometheus (`:9090`) and Grafana (`:3000`, anonymous admin) on the
control host. Prometheus scrapes three jobs: `avalanche-l1` (every node's
`/ext/metrics` on its per-slot port), `fleet` (the weight exporter on
`:9091`, gauges `fleet_desired_weight` / `fleet_actual_weight`), and
`bombard` (`:9092`). Five dashboards are provisioned:

- **Failover Overview**: per-server serving state and stake tier timelines,
  successful polls %, chain TPS. The one to watch during the drill.
- **Failover Details**: per-node finalized height, the A-to-B finalized gap,
  mempools.
- **Load Generator**: bombard's end-to-end tx latency p50/p95, mined TPS,
  resubmits.
- **Benchmark**: per-node TPS, consensus, verification.
- **Machine Metrics**: CPU, memory, disk, network per box.

Not reachable? Tunnel:

```bash
ssh -i <key> -L3000:localhost:3000 -L9090:localhost:9090 <user>@<control-host>
```

## Step 5: confirm health

```bash
./fleet status
```

Expect every node `SERVING` and the summary line:

```
validators serving: 4/4 (intended up: 4/4)
```

The table shows one row per machine (its number is the CLI handle for
`down`/`up`/`weight`), grouped by DC, with its stake tier (`validator`,
`spare`, `dead`, `rpc`) and reachability (`SERVING block=N`,
`BOOTSTRAPPING`, `DOWN`, or `off (down by intent)`). A fresh chain sits at
`block=0` until load arrives; that is healthy, Avalanche produces blocks on
demand. `./fleet status --watch` refreshes continuously.

## Step 6: drive load (terminal 1, leave running)

```bash
./run/03_bombard.sh
```

One fixed profile: 4000 tx/s, 2000 in-flight cap, 5s resubmit, targeting
every pinned RPC on both sites. bombard broadcasts each tx to all of them,
health-checks each endpoint, drops laggards from rotation and re-adds them,
and resubmits anything in flight, so ingress rides through everything below
without intervention. Leave it running; it is the live witness.

## Step 7: the drills (terminal 2)

Each scenario first restores the ground state (all machines up, 1-4
validating, everything else spare), then executes its failure, so any
scenario runs from any starting point. Recovery from anything is
`./scenarios/00_healthy.sh`.

```bash
./scenarios/01_validator_down.sh          # one validator dies, 3 of 4 remain
./scenarios/02_validator_down_replace.sh  # a site B machine steps in, back to 4
./scenarios/03_datacenter_failure.sh      # site A dies, site B takes over
./scenarios/04_datacenter_failback.sh     # site A returns, consensus moves home
```

What a weight move looks like: each `./fleet weight` prints the tier changes,
then the reconcile against Fuji, e.g.

```
  b1: spare -> validator
[3/3] weights: reconciling via ValidatorManager 0x... (subnet ...)
  weights: firing 2 initiates in one burst:
    b1 -> 801800
    b1 -> 1000000
  weights: b1 deliver weight 1000000 (nonce 4) to the P-chain
  weights: b1 complete (ack nonce 4, weight 1000000)
  weights: converged (contract == P-chain == desired)
```

Fuji's signature coverage for a fresh warp message can take minutes to reach
quorum; the command retries on an escalating schedule (up to ~36 min) and
prints each attempt. The chain stays healthy on its current weights while it
waits. If it does time out, re-running the same `./fleet weight` command
resumes; every step is idempotent. Weight lines that read
`weights match ... nonce ... lags` or a mention of Glacier caching are the
known Fuji-side transients; see
[failover-recovery-simulation.md](failover-recovery-simulation.md).

What to watch during scenario 03:

- **Failover Overview**: site A serving states drop, stake tier timelines
  seesaw, TPS dips and recovers at site B's cadence (~10 blk/s; TPS is
  unaffected, blocks are just bigger).
- **`./fleet status`**: machines 1-6 `off (down by intent)`, `b1`-`b3` at
  tier `validator`, summary `validators serving: 3/3`.
- **bombard** (terminal 1): sends fail over to `rpc_b1`/`rpc_b2`, in-flight
  txs resubmit, throughput recovers.

## Step 8: halt and recover (optional but instructive)

```bash
./scenarios/00_healthy.sh
./fleet down 1 2        # 2 of 4 validators dead: quorum lost, chain HALTS
./fleet status          # prints the quorum WARNING
./fleet up 1 2          # both back: latch clears, chain resumes in seconds
```

The non-obvious part: bringing back only ONE validator leaves the chain
halted. A recovering validator stays `BOOTSTRAPPING` until ~75% of validator
stake is connected (`./fleet up` waits for SERVING and would time out after
10 minutes on a lone machine); `status` prints a HINT naming the threshold.
Bring the machines back together, as above, and they clear the latch as a
group.

## Step 9: tear down (optional)

```bash
./fleet destroy
```

Stops every node and removes the remote deploy dirs. Keeps `network.env`
(the chain identity cost AVAX) and `staking/`; the chain stays registered on
Fuji, so a later `./run/01_deploy.sh` brings the fleet back on the same
chain for free.

## Reading the results as DR metrics

- **RTO**: from issuing the scenario to `validators serving: 3/3` on site B
  with height advancing. Watch it on Failover Overview.
- **RPO**: zero by construction. Site B was at tip when the weight arrived
  and the chain is one branch; nothing finalized is lost. The measurable
  loss is bombard's in-flight window, which its resubmits recover; the
  latency report captures the whole dip end to end.
- **No fork**: keys never moved, so equivocation is structurally impossible.
  Spot-check by comparing `eth_getBlockByNumber` hashes at a common height
  across nodes.
