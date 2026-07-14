# Avalanche L1 Failover Kit

Tooling to run an Avalanche L1 across two data centers and drill site
failover under transaction load (~4000 tx/s with the default profile).
Everything runs from one control host: deploy the fleet, generate load,
watch it in Grafana, kill machines or a whole site, move consensus weight,
recover.

## Architecture

- All validators, both sites, are registered on the L1 once, at chain
  creation, each at an initial weight of 1000. A failover never registers or
  removes anything: it only moves on-chain consensus weight between them.
- The chain's validator manager is recorded on the L1's OWN chain (address
  `0x0000000000000000000000000000000000000001`; no contract exists there).
  The kit holds every validator's BLS key, so `bin/l1` signs each weight
  change locally with all of them, aggregates, and submits the
  `SetL1ValidatorWeightTx` straight to the P-chain. No ValidatorManager
  contract, no courier, no signature aggregator, no C-chain anywhere.
- The only external dependency of the tooling is one P-chain RPC (the public
  Avalanche API by default). Keys never move between machines, so a failover
  cannot fork the chain.
- The L1 anchors on a public Avalanche P-chain (Fuji or mainnet) as a read
  feed. Validators have zero internet connectivity by design; only the RPC
  nodes make ONE outbound TCP connection each, to a pinned public peer,
  and the validators follow the P-chain through them.
- The control host runs no L1 node, so orchestration, load generation and
  monitoring survive the loss of either site.

## Inventory: nodes.ini

The fleet inventory is one file, `nodes.ini`, the single source of truth for
node names, hosts and roles. The kit ships `nodes.ini.example` with
placeholder addresses; copy it to `nodes.ini` and point `host=` at your own
machines. One node per line:

```
# <name> host=<ip> role=validator|rpc [dc=<tag>]
a1     host=10.0.0.1  role=validator  dc=A
rpc_a1 host=10.0.0.5  role=rpc        dc=A
b1     host=10.1.0.1  role=validator  dc=B
```

- The node NAME is the primary key everywhere: fleet verbs (`./fleet up a1`),
  the staking key dir (`staking/l1/a1`), the manifest line in
  `staking/node-ids.env`, and the node's data root `data/a1` on its box.
- `role=validator` nodes are registered as L1 validators at chain creation
  (a "hot spare" is nothing special, just a validator held at low weight).
  `role=rpc` nodes track the chain and serve the load-generator ingress; they
  are never registered and never carry a BLS signer key (they run an
  ephemeral in-memory signer).
- `dc=` is a display-only tag: `fleet status` groups by it and fleet verbs
  accept `dc=A` selectors. Nothing functional depends on it.
- Weights are NOT inventory. `l1 create` registers every validator at 1000;
  the real distribution is applied afterwards with `l1 apply`
  (`scenarios/00_healthy.sh`). The on-chain weight is the only truth.

Ports are positional per host: the k-th node on a host serves HTTP on
9650+2k and staking p2p on 9651+2k, so the one-node-per-box production shape
is uniformly 9650/9651. Co-host several nodes to fit the topology onto fewer
test boxes (each gets its own ports and `data/<name>` root); reordering
nodes that share a host shifts the later nodes' ports, so redeploy that
host's nodes after such an edit. `./fleet endpoints` prints the resolved
layout (name, dc, role, host, port).

The shipped shape is 4 validators + 2 RPC nodes per data center plus one
control host.

## Requirements

- Linux amd64 hosts, all reachable from the control host over SSH with one
  key.
- Open ports on the fleet boxes: 22 (SSH) and 9650-9651 (+2 per extra
  co-hosted node; open 9650-9750 to never think about it again).
- The RPC machines additionally need ONE outbound TCP to the pinned upstream
  peer (default `18.192.93.241:9651` on Fuji, `54.232.137.108:9651` on
  mainnet; override with `FUJI_UPSTREAM_IPS`/`FUJI_UPSTREAM_IDS` in `.env`).
- No Go toolchain needed on the control host: the kit ships prebuilt
  binaries. To build from source: `make`.

Configure once, in the kit root:

```bash
cp .env.example .env             # then set SSH_USER + SSH_KEY_PATH (and API_TOKEN if you have one)
cp nodes.ini.example nodes.ini   # then set every host= to your machines' IPs
```

## Quick start: from a single key

You need two things: the release zip (`avalanche-l1-kit.zip`) and ONE secret,
a funded Avalanche wallet key. That key is the only secret in the entire
workflow. Everything else, every node's TLS staking key and BLS signer key,
the chain identity in `network.env`, is GENERATED on first run. Nothing is
pre-created off-machine.

1. Unzip the kit into an empty directory.
2. `cp .env.example .env` and set `SSH_USER` / `SSH_KEY_PATH` (and
   `API_TOKEN` if you have one).
3. `cp nodes.ini.example nodes.ini` and set every `host=` to your boxes'
   IPs. The node NAMES can stay as they are; only the hosts are yours.
4. Drop your wallet key in place, the one secret:

   ```bash
   mkdir -p staking
   cp /path/to/your-wallet.key staking/fuji-wallet.key   # raw hex secp256k1 key, one line
   ```

   No key of your own? Skip this: step 5's `00` generates a fresh wallet for
   you to fund at the faucet.
5. Generate identities, fund, create the L1, deploy:

   ```bash
   ./setup/00_gen_secrets.sh    # one staking identity per nodes.ini node (BLS keys for validators only); keeps your wallet key, or generates one
   ./setup/01_fund_wallet.sh    # prints the P-chain address, polls until you fund it (Fuji faucet, or your own AVAX with --mainnet)
   ./setup/02_create_chain.sh   # subnet + chain + ConvertSubnetToL1Tx; writes network.env. SPENDS AVAX. Once per chain. --mainnet for mainnet.
   ./run/01_deploy.sh           # wipe + start the whole fleet from genesis (no AVAX spent)
   ```

6. `./run/02_monitoring.sh` brings up Prometheus + Grafana on the control
   host.
7. Start load (`./run/03_bombard.sh`, see Load below), reset to the healthy
   baseline (`./scenarios/00_healthy.sh`), then run the drills
   (`scenarios/01..07`) while watching `./fleet status` and Grafana.

`02` persists the chain identity (including `NETWORK=fuji|mainnet`) to
`network.env`; every later command reads it. Re-running `02` never creates a
second chain: it resumes/verifies the recorded one. `setup/03_backup_secrets.sh`
bundles the generated `staking/` + `network.env` into a tar.gz: keep it
off-machine so a control-host rebuild does not lose control of the chain (the
subnet/chain cost AVAX to recreate). To rebuild a control host, unpack the
kit, untar that backup over the kit root, and resume at `./fleet status` or
`./run/01_deploy.sh`, no re-creation, no re-spend.

First boot of freshly provisioned boxes syncs the anchor P-chain from
scratch (RPC tier first, then validators through them; minutes on Fuji,
hours on mainnet). That cost is paid once per box: every later rebuild,
redeploy or drill preserves the synced P-chain db.

### The single key: what it must do

One secp256k1 key, delivered as raw hex at `staking/fuji-wallet.key`. It must
hold AVAX on the anchor network's **P-chain** (Fuji via the faucet, or real
AVAX on mainnet); `01` prints the exact amount (0.1 AVAX per registered
validator continuous-fee deposit on Fuji, plus fees) and polls until it
clears. Everything pays from the P-chain: chain creation, the per-validator
deposits, and every weight tx. There is no separate C-chain or gas account to
fund, the L1's own genesis prefunds this same key, so the load generator
(`run/03_bombard.sh`) drives the benchmark from it too.

The chain is disposable by design: each validator's continuous-fee deposit
(0.1 AVAX on Fuji, ~5-6 days; 0.15 AVAX on mainnet, ~3 days at the 512
nAVAX/s floor) drains in days, after which validators deactivate and the L1
halts. `bin/l1 status` shows per-validator runway and warns under 7 days;
`bin/fuji-wallet topup [days]` tops every validator up to at least that many
days (default 3), funded from the same wallet.

## Load

```bash
./run/03_bombard.sh              # 4000 tx/s fixed profile; -tps N overrides
```

Ingress is every `role=rpc` node; zero rpc nodes in `nodes.ini` is a hard
error, bombard never falls back to validators. bombard broadcasts each tx to
all of them, health-checks every endpoint, drops one from rotation when it
falls behind and re-adds it when it catches up, and resubmits anything in
flight, so ingress rides through dead nodes and dead sites untouched. It is
single-account: the script refuses to start a second instance (duplicate
nonces would mine nothing). Other knobs (inflight cap 2000, 5s resubmit) are
constants at the top of the script.

## Operating the fleet

Two independent axes: hardware via `./fleet`, stake via `bin/l1`.

Hardware (`./fleet`, nodes addressed by name or `dc=<tag>`):

| verb | effect |
|---|---|
| `up <node...>` | rebuild + start nodes and block until SERVING at the fleet tip. Already-serving nodes are skipped; catching-up/bootstrapping nodes are waited on; a stalled node (frozen height = fork wedge, or no bootstrap progress) is rebuilt in place automatically. |
| `down <node...>` | simulate hardware failure: hard SIGKILL, data left on disk |
| `status` | read-only: per-DC stake tier + node state (wrap in `watch -n5`) |
| `endpoints` | one line per node: name, dc, role, host, HTTP port |
| `fresh` | WIPE every node's L1 data and redeploy the whole fleet from genesis (what `run/01_deploy.sh` runs; P-chain db and on-chain weights untouched) |
| `destroy` | kill everything and remove the deploy dirs; keeps `network.env` (the chain identity costs AVAX to recreate) |
| `exporter` | serve the `fleet_actual_weight` Prometheus gauge on :9091 (started by monitoring) |

Neither `up` nor `down` ever touches on-chain weight: reviving a box must
never depend on anchor-network quorum. A hard-down box (ssh-unreachable)
never blocks operating the rest of the fleet: it is recorded as down with a
warning and only commands that target it specifically fail.

Stake (`bin/l1`):

| verb | effect |
|---|---|
| `apply --weights a1=100000,b1=1,...` | declarative targets, all raises applied before any lower, one tx at a time, each verified on-chain |
| `set-weight --node <name> --weight <w>` | one validator (accepts a name, NodeID or validationID) |
| `status` | the registered set with weights, balances and a fee-runway warning |
| `create` | the one-time chain creation (driven by `setup/02_create_chain.sh`) |

The tiers used by the scenarios: active validator 100000, spare 1000, dead 1.
Weight txs are signed locally and go straight to the P-chain, so they land
even while the L1 itself is halted. On a "signature is invalid" or
set-mismatch rejection just re-run the command: every run refetches the
registered set and re-signs fresh.

Two rules of thumb:

- **Raise before you lower.** A single `l1 apply` does this ordering for
  you, so the fleet never passes through a low-weight window mid-seesaw.
- **Do not raise a node that is behind the fleet tip** (check
  `./fleet status` first): a behind node with more stake wins proposer slots
  on stale heights and can self-finalize a fork. There is no built-in gate.

A typical failover, by hand:

```bash
watch -n5 ./fleet status                   # live per-DC stake tier + node state
./fleet down a1                            # crash a1
./bin/l1 apply --weights b1=100000,a1=1    # promote a spare, retire the dead box
./fleet up a1                              # rebuild a1, it re-syncs and rejoins
```

### Drills (scenarios/)

`00_healthy.sh` is the canonical reset (every node up, a1-a4 at full weight,
site B at spare weight); every other scenario invokes it first, so each can
run from any starting point. Recovery from anything is `00_healthy.sh`.

```bash
./scenarios/00_healthy.sh                  # ground state only
./scenarios/01_validator_down.sh           # one validator dies, chain rides on 3 of 4
./scenarios/02_validator_down_replace.sh   # a1 dies, b1 takes over, back to 4 active
./scenarios/03_datacenter_failure.sh       # site A goes dark, site B takes consensus
./scenarios/04_validator_maintenance.sh    # planned maintenance: drain a1's stake, then power it off
./scenarios/05_2x2.sh                      # 2x2 split: two full-weight validators per DC
./scenarios/06_all_validators.sh           # all eight validators at full weight
./scenarios/07_two_validators_down.sh      # two die at once: chain halts, P-chain weight ops revive it
```

Scenario 04 waits 35s between draining and powering off: the P-chain accepts
the weight change instantly, but proposer selection reads a 30s-lagged
P-chain height (`RecentlyAcceptedWindowTTL`), so the drained node keeps
winning proposer slots at its old weight until the window passes.

### What to expect when validators drop

With 4 active validators at equal weight:

| Action | Result |
|--------|--------|
| 1 of 4 down, replacement promoted | back to 4 active, full speed |
| 1 of 4 down, no replacement | consensus rides through on 3 of 4 |
| 2 of 4 down | connected stake 50% is under the 56.7% polling gate: chain HALTS (expected, recoverable) |
| bring back one validator into a halted chain | still halted, see the 75% latch below |
| bring back the rest | chain resumes within seconds |

A (re)starting validator will not begin bootstrapping the L1 until ~75% of
validator stake is online, so recovering a fully halted chain needs enough
validators up to clear the latch together (`./fleet status` shows them stuck
`BOOTSTRAPPING` until then). The other way out of a halt, without restarting
anything, is draining the dead machines' weight
(`bin/l1 apply --weights a1=1,a2=1`): the txs land on the P-chain while the
L1 is halted, the survivors' connected share rises above the gate, and the
chain resumes (scenario 07).

A validator hard-killed under load can come back wedged (diverged local
chain, stuck `BOOTSTRAPPING`, or `CATCHING UP` at a frozen height after
self-finalizing a sibling block). `./fleet up <name>` detects both and
rebuilds the node in place: wipe only its L1 chain data and bootstrap
backlog, keep the P-chain db and staking identity, restart, state re-sync.

## Monitoring

```bash
./run/02_monitoring.sh    # Prometheus :9090 + Grafana :3000 on the control host, anonymous admin
```

Re-runnable; it regenerates the scrape targets from `nodes.ini`, so run it
again after any inventory edit. If the ports aren't open to you, tunnel:

```bash
ssh -i <key> -L3000:localhost:3000 <user>@<control-host>   # open http://localhost:3000
```

Four dashboards are provisioned: **Failover Overview** (per-node serving
state and stake-tier timelines, successful polls %, chain TPS, plus
bombard's end-to-end latency p50/p95, mined TPS and resubmits), **Failover
Details** (per-node finalized height, the A-to-B finalized gap, mempools),
**Benchmark** (per-node TPS, consensus, verification) and **Machine
Metrics** (per-box CPU, memory, disk, throttle pressure).

## Reference

### Binaries

All prebuilt in `bin/`, all run from the kit root:

- `benchmark-fleet` (via the `./fleet` wrapper): hardware orchestration.
- `l1`: chain creation and on-chain weight moves (self-signed warp).
- `bombard` (via `run/03_bombard.sh`): the load generator.
- `fuji-wallet`: `gen` / `fund` / `topup [days]` for the fund/fee wallet.
- `genstaking`: generates missing staking identities from `nodes.ini`
  (invoked by `setup/00_gen_secrets.sh`).
- `bsclear`: drops a node's L1 bootstrap backlog from its db; runs on the
  boxes, invoked automatically by fleet rebuilds.

### Configuration files

- `.env` (gitignored): `SSH_USER`, `SSH_KEY_PATH`, optional `API_TOKEN`,
  optional overrides `PCHAIN_API`, `FUJI_UPSTREAM_IPS`/`FUJI_UPSTREAM_IDS`.
- `nodes.ini`: the fleet inventory, copied from `nodes.ini.example` (see
  above).
- `network.env` (gitignored, written once by `l1 create`): `NETWORK`,
  `SUBNET_ID`, `CHAIN_ID`, `MANAGER_ADDRESS`. The chain's identity; kept
  even by `fleet destroy`.
- `chain-config.json`: subnet-evm config. `min-delay-target: 25` sets the
  25ms block cadence; throughput is gas-bound, not block-rate-bound (see
  [docs/throughput-tuning-and-benchmarks.md](docs/throughput-tuning-and-benchmarks.md)).
  Re-run `./run/01_deploy.sh` after editing.
- `subnet-config.json`: consensus parameters (below).
- `node-config.json`, `genesis.json`: avalanchego and L1 genesis config.

### Env vars

- `PCHAIN_API`: the one external RPC everything P-chain goes through
  (`bin/l1`, `fuji-wallet`, the fleet's read-only weight reads). Default:
  `https://api.avax-test.network` on Fuji, `https://api.avax.network` on
  mainnet. The kit's own RPC tier is follow-only and never serves
  `platform.*`.
- `API_TOKEN`: optional rate-limit-bypass token for the public API; when
  set, every P-chain/info request carries it as a `token=<value>` query
  param. Runtime-only, lives in the gitignored `.env`, never committed and
  never in any archive.

### Secrets

Never commit, never put in a kit archive: `staking/` (per-node TLS
identities, validator BLS keys, `fuji-wallet.key`), `network.env`, `.env`.
A leaked staking key means validator impersonation; losing `staking/` +
`network.env` means losing control of the chain. The one secret that comes
from outside is the wallet key you drop at `staking/fuji-wallet.key`; every
other file in `staking/` and `network.env` is generated on first run. The
only sanctioned copy of the generated set is the `setup/03_backup_secrets.sh`
tarball, stored off-machine (your own backup, not a handover bundle).
`staking/node-ids.env` (the name-to-NodeID manifest) is the one non-secret
in there.

### Consensus parameters

`subnet-config.json` ships k=30, alphaPreference=16, alphaConfidence=17,
beta=12. Avalanche samples by stake over weight units, not by validator
count, so these values work unchanged for ANY number of validators >= 1
(one validator occupies all 30 sample slots and its single vote counts with
multiplicity; oversampling is deduped on the wire). The two thresholds that
shape the drill results:

- Polling gate: a node drops all consensus queries while its connected stake
  share is below alphaConfidence/k = 56.7% (a node counts itself). This is
  the safe halt in scenario 07, and why 1-of-3 down keeps running (66.7%)
  while 1-of-2 down halts (50%).
- Bootstrap latch: a (re)starting node needs >= 75% of validator stake
  connected before it starts bootstrapping the L1 chain.

### On the boxes

Each node lives under `~/avalanche-benchmark` on its box: `bin/`, `plugins/`
and one `data/<name>` root per hosted node (db, chainData, logs, active
staking identity). Nodes are raw processes with two baked-in guards: the
stdout capture (`data/<name>/logs/avalanchego.out`) is truncated if it
exceeds 2 GiB, and the process tree runs with `GOMEMLIMIT` at 75% of RAM
plus a raised `oom_score_adj`, so a runaway node is GC-throttled and, at
worst, killed by the kernel instead of wedging the machine.

## Migrating from numbered key dirs

Deployments created before the `nodes.ini` inventory keyed staking
identities by NUMBER (`staking/l1/1..12`, `L1_<n>_NODE_ID` manifest lines)
and used `data/validator` as every box's data root. The tools refuse those
layouts with a pointer here. Identities never change and nothing touches the
chain; the migration is renames:

| numbered dir | node name | | numbered dir | node name |
|---|---|---|---|---|
| `staking/l1/1` | `staking/l1/a1` | | `staking/l1/7` | `staking/l1/b3` |
| `staking/l1/2` | `staking/l1/a2` | | `staking/l1/8` | `staking/l1/b4` |
| `staking/l1/3` | `staking/l1/a3` | | `staking/l1/9` | `staking/l1/rpc_a1` |
| `staking/l1/4` | `staking/l1/a4` | | `staking/l1/10` | `staking/l1/rpc_a2` |
| `staking/l1/5` | `staking/l1/b1` | | `staking/l1/11` | `staking/l1/rpc_b1` |
| `staking/l1/6` | `staking/l1/b2` | | `staking/l1/12` | `staking/l1/rpc_b2` |

On the control host: rename the dirs per the table, rewrite
`staking/node-ids.env` as `<name>=<NodeID>` lines (same NodeIDs), delete
`signer.key` from the rpc dirs (rpc identities never carry one), and drop
the retired `*_IPS` topology vars from `.env` in favor of `nodes.ini`. On
each fleet box: `mv data/validator data/<name>` (a same-filesystem rename;
the synced P-chain db is preserved, never copied, never deleted).

## Further reading

- [docs/e2e-runbook.md](docs/e2e-runbook.md): the full end-to-end drill with
  expected output at each step, install to failback.
- [docs/two-site-failover.md](docs/two-site-failover.md): the two-site
  design, block cadence, and what is simulated vs production.
- [docs/failover-recovery-simulation.md](docs/failover-recovery-simulation.md):
  the failover model: weight seesaw, warp message path, halt/recovery theory.
- [docs/throughput-tuning-and-benchmarks.md](docs/throughput-tuning-and-benchmarks.md):
  the throughput study behind the 4000 tx/s profile (historical).
