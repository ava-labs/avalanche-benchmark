# Avalanche L1 Failover Benchmark

Tools to stand up an Avalanche L1, drive it with transaction load, and **simulate
validator failover** — taking validators "offline" and recovering them — on a
fixed pool of machines, without ever adding or removing machines from the fleet.

**Pool:** 5 remote machines run the L1 as **3 validators + 1 hot spare + 1
dedicated RPC node**. Validator identities (staking keys) move between machines
on failover, so the L1 keeps quorum when a machine goes offline. The 5th machine
(`m5`) is a **pinned non-validating RPC node** — the load generator's ingress
target; it is never promoted to a validator, so the benchmark ingress survives
failover events. The local dev machine runs 5 P-chain (primary network)
validators that the L1 bootstraps against.

## Ports

Open the following ports on your remote nodes:

| Port | Service | Required | Notes |
|------|---------|----------|-------|
| 22 | SSH | Yes | Remote access |
| 9652-9653 | AvalancheGo | Yes | L1 HTTP (RPC) / staking ports |

The five local P-chain validators run on the dev machine on ports `9650/9651`,
`9660/9661`, `9670/9671`, `9680/9681`, and `9690/9691`.

## Build

Binaries are built from source on a Linux machine (requires Go and git):

```bash
make          # builds pinned avalanchego + subnet-evm + local tools
```

## Configure

```bash
cp .env.example .env
# Edit .env:
#   SSH_USER=ubuntu
#   NODE_IPS=1.2.3.1,1.2.3.2,1.2.3.3,1.2.3.4,1.2.3.5   # exactly 5: m1-3 validators, m4 hot spare, m5 dedicated RPC
#   BACKUP_SITE_NODE_IPS=...                            # optional: 5 more IPs = backup site (b1-b4 sync, b5 RPC)
```

Setting `BACKUP_SITE_NODE_IPS` enables **two-site mode**: a backup data center
of zero-weight syncing trackers the validator set can be swapped onto when the
whole primary site goes down (`./scripts/failover/site-failover.sh b`), and
back (`... a`). Single-site behavior is unchanged when it is unset. See
[docs/two-site-failover.md](docs/two-site-failover.md).

## Full Walkthrough

A complete cycle: **start the chain → benchmark → fail a validator over → stall
the chain and recover it → wipe the slate clean.** Run from the dev machine.

### 1. Start the local P-chain

```bash
./01_bootstrap_primary_network.sh
```
Starts 5 local primary-network validators. The L1 validators bootstrap through
these, so leave them running for the whole session.

### 2. Create the L1 (one time per chain)

```bash
./02_create_l1.sh
```
Registers the 3 validator identities (`staking/l1/6,7,8`) on the P-chain and
writes `network.env` with the new `SUBNET_ID` / `CHAIN_ID`.

### 3. Deploy the validator pool

```bash
./03_wipe_and_deploy_l1.sh
```
Uploads binaries/plugin/keys to all 5 pool machines and starts **3 validators + 1
hot spare + 1 dedicated RPC node**. **Destructive:** it wipes node data and (re)starts the chain from
genesis (block 0). Re-run any time to reset to a clean chain — the L1's P-chain
registration is preserved, so you do **not** re-run `01`/`02`. Editing
`chain-config.json` and re-running this is how you apply a new chain config.

### 4. Check health

```bash
./scripts/failover/status.sh
```
Read-only. Expect all five nodes `SERVING` (m1-3 validators, m4 spare, m5 rpc) and `validators serving: 3/3`.

### 5. Run a benchmark

```bash
./05_benchmark.sh            # fixed failover target
```
Sends load to the dedicated RPC node `m5` (pinned non-validator, never promoted),
so ingress is unaffected by validator failover. See
[Benchmark Script](#benchmark-script).

### 6. Fail a validator over (safe — chain keeps running)

```bash
./scripts/failover/down.sh 2   # take machine 2 offline; the hot spare assumes its identity
./scripts/failover/status.sh   # still 3 validators serving — the identity moved to the spare
./scripts/failover/up.sh 2     # machine 2 returns to service as the new spare
```
The chain keeps producing throughout. You can run `05_benchmark.sh` during a
failover to measure the impact. See
[Recovering From a Stalled Chain](#recovering-from-a-stalled-chain) for what
happens as you remove more validators.

### 7. Stall the chain, then recover it

```bash
# After a fresh deploy the layout is m1=v1, m2=v2, m3=v3, m4=spare.
./scripts/failover/down.sh 1   # spare (m4) takes over v1 -> still 3/3
./scripts/failover/down.sh 2   # no spare left -> 2 of 3 (slower, still alive)
./scripts/failover/down.sh 3   # 1 of 3 -> chain HALTS (expected)
./scripts/failover/status.sh   # validators serving: 1/3, HALTED warning

# Recover: you must bring ALL THREE validators back online.
./scripts/failover/up.sh 1
./scripts/failover/up.sh 2     # the chain resumes once the third validator connects
./scripts/failover/up.sh 3     # restores the hot spare
./scripts/failover/status.sh   # back to 3/3 SERVING
```
**Important:** once the chain has halted, bringing back only *one* validator is
**not** enough and is **not** a bug — a restarting validator needs ~75% of the
validator set (all 3) online before it can bootstrap. Watch `status.sh`: a node
waiting on this shows as `BOOTSTRAPPING`. Full explanation below.

### 8. Wipe the slate clean

```bash
./06_cleanup.sh
```
Stops every node (local P-chain and all pool machines), removes the remote
deployment directories, and deletes the local `network.env`. To start over, go
back to step 1.

## End-to-End Failover Test Under Constant Load

This is the full acceptance test: drive the L1 with steady traffic, then fail
validators down to a halt and recover them, watching the benchmark react in real
time and confirming no validator identity is ever duplicated. Use **two terminals**
on the dev machine.

Start from a healthy cluster (`status.sh` shows `3/3 SERVING`, layout
`m1=v1, m2=v2, m3=v3, m4=spare`).

**Terminal A — constant load (leave running the whole time):**

```bash
./05_benchmark.sh
```
The script runs a fixed ~1000 TPS target at 100ms blocks, with `-resubmit 3s`
set internally. That keeps the resubmit interval above the worst-case proposer
stall so a single down validator doesn't trigger a resubmit storm. The benchmark
backs off on its own (in-flight cap) and resubmits in-flight txs, so it is the
live witness for chain health.

Current limit: run failover tests at **1000 transactions per second**. Higher
rates are not reliable through failover right now; above this target, the
benchmark or chain can fail to recover cleanly during the failover sequence.

**Terminal B — the failover sequence:**

```bash
# 1. Fail one validator. The hot spare assumes its identity -> still 3/3.
./scripts/failover/down.sh 1
./scripts/failover/status.sh        # 3/3 SERVING; v1's identity now on the old spare
#   Terminal A: a brief stall while the spare boots into the validator role
#   (chain is at 2/3 during that window), then load resumes. Degradation here
#   is expected.

# 2. Fail a second validator. No spare left -> 2 of 3.
./scripts/failover/down.sh 2
./scripts/failover/status.sh        # 2/3 SERVING; chain keeps producing, slower
#   Terminal A: lower TPS / higher tail latency, but still landing blocks.

# 3. Fail the third validator -> 1 of 3 -> chain HALTS (expected).
./scripts/failover/down.sh 3
./scripts/failover/status.sh        # 1/3, "HALTED" warning; block height frozen
#   Terminal A: minedTps -> 0, "no landings this tick". The benchmark is stalled
#   because the chain is, not because of a bug.

# 4. Bring back ONE validator. NOT enough -> chain stays halted.
./scripts/failover/up.sh 1
./scripts/failover/status.sh        # the rejoining node shows BOOTSTRAPPING,
                                    # "validators serving: 1/3 (intended up: 2/3)"
#   Two validators online is only 66% < the 75% a rejoining validator needs to
#   bootstrap, so it sits in BOOTSTRAPPING and the chain does NOT recover.
#   Terminal A: still stalled. This is expected — see "Recovering From a Stalled
#   Chain" below.

# 5. Bring back a second machine. The 3rd validator clears the latch.
./scripts/failover/up.sh 2
./scripts/failover/status.sh        # 3/3 SERVING; block height jumps forward
#   Terminal A: the chain resumes within seconds and the benchmark drains its
#   backlog back to ~1000 TPS. (Txs that waited out the halt carry a large
#   one-time latency tail as they finally land — that drains and clears.)

# 6. Restore the hot spare to return to the canonical baseline.
./scripts/failover/up.sh 3
./scripts/failover/status.sh        # back to 3/3 SERVING + spare
```

`up.sh` reassigns each rejoining machine the lowest orphaned **validator** key
automatically (so a returning machine becomes a validator, not a second spare) —
this is the "switch the keys to make it a validator" behavior.

**Verify no duplicate identities** (each staking key on exactly one live machine,
all NodeIDs distinct):

```bash
source network.env
for ip in $(echo "$NODE_IPS" | tr ',' ' '); do
  curl -s -X POST --data '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' \
    -H 'content-type:application/json' "http://$ip:9652/ext/info" \
    | grep -o '"nodeID":"[^"]*"'
done | sort | uniq -c
```
Every NodeID should appear exactly once. During a halt, multiple **down** machines
may hold the spare key on disk, but only one node ever runs a given key at a time —
the failover tool never starts two nodes on the same identity.

> **Note on the benchmark as a witness:** `05_benchmark.sh` is a single-issuer
> load generator that sends to the pinned RPC node `m5`. Because `m5` is never
> promoted, its endpoint stays up across validator failover — but while the chain
> itself is mid-transition (a validator going down, the spare booting into its
> role) block production briefly stalls, so the strict nonce line can look
> "stuck" momentarily even after the chain is healthy again. Treat `status.sh` /
> node health as the source of truth for chain state during transitions; the
> benchmark fully recovers once the chain is back to `3/3`. (`bombard` can also be
> pointed at multiple RPCs for ingress redundancy, in which case a downed
> endpoint just drops out of the fan-out.)

## Failover Commands

`scripts/failover/` simulates validators going offline and recovering on the
fixed 4-machine pool by moving validator identities between machines. See
`docs/failover-recovery-simulation.md` for the design.

```bash
./scripts/failover/down.sh <m>   # cordon machine m (take it "offline")
./scripts/failover/up.sh <m>     # uncordon machine m (return it to service)
./scripts/failover/failover.sh   # re-apply current intent (recover an interrupted run)
./scripts/failover/status.sh     # read-only: show each node's ACTUAL state
./scripts/failover/clean.sh <m>  # wipe machine m's chain data and re-bootstrap it clean
```

`status.sh` reports every node as `SERVING@block` / `BOOTSTRAPPING` / `DOWN` and
an honest `validators serving: X/3`, so you see what is *actually* happening
rather than what was *intended*.

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

## Benchmark Script

`05_benchmark.sh` intentionally accepts no command-line flags. The failover demo
uses one fixed profile:

- target rate: `1000 rps`
- resubmit interval: `3s`
- ingress: the dedicated RPC node `m5` (pinned non-validator, never promoted)

To change the profile, edit the constants in `05_benchmark.sh`.

### Failover Throughput Limit

1000 rps is the current supported target for the failover workflow. Do not run
the failover demo above 1000 transactions per second and treat higher-rate
failover attempts as unsupported until the throughput path is fixed.

If you pushed too hard and need to restart, wait 60 seconds for the mempool to clear before starting a new benchmark (mempool expiration is set to 1 minute).

### Block Time

Genesis is configured with ACP-226 excess gas parameters (`graniteTimestamp: 0`,
`initialMinDelayMS: 5`) for fast block production from the start. The chain
config sets `min-delay-target: 5`, and the packaged AvalancheGo build pins a
1s proposer window branch. To tune further, edit `min-delay-target` in
`chain-config.json` and re-run `./03_wipe_and_deploy_l1.sh` (which resets the
chain to genesis — see step 3).

## Topology

- **Local dev machine:** 5 P-chain (primary network) validators using committed
  `staking/l1/1..5`.
- **Pool (remote machines 1–4):** 3 L1 validators using committed
  `staking/l1/6,7,8` plus 1 hot spare using `staking/l1/9`. Validator identities
  move between these 4 machines on failover.
- **Machine 5:** the dedicated RPC node, a pinned non-validating tracker using
  committed `staking/l1/10`. Never promoted to a validator, so the benchmark
  ingress survives failover. (In production this tier has 2+ RPC machines; here
  it is one.)
- **Benchmark traffic:** `05_benchmark.sh` sends to the `m5` dedicated RPC (port
  `9652`) and skips any node that is offline.

### Reference Benchmark

On the 4-machine failover pool with 3 validators + 1 hot spare, a constant
`./05_benchmark.sh` run recovered to ~990-1014 mined TPS after the full
down/down/down/up/up/up sequence, with steady-state p50 ~= 82ms, p95 ~= 129ms,
and p99 ~= 137ms.
