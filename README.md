# Avalanche for Isolated Networks

Benchmark and failover toolset for Avalanche L1s in isolated networks: no internet egress, fixed validator membership, PoA. Stand up an L1 on Fuji or mainnet, run it airgapped, drive load, and drill data-center failover by moving staking identities (key swap) or stake weight, on one deployment, with no chain re-creation between the two.

Operator manual. Rationale and decision history: **[DESIGN.md](DESIGN.md)**.

## Runbooks

### Fresh chain on a fresh fleet

```bash
cp nodes.ini.example nodes.ini    # edit host= lines, exactly one role=pchain
cp .env.example .env              # NETWORK, PCHAIN_API, FUNDING_PRIVATE_KEY, SSH_*
go run ./cmd/l1 address           # fund the printed P-chain address
go run ./cmd/l1 create            # generates deployment/ if absent, then both L1s
make pack                         # remote-benchmark.tar.gz: binaries + configs, no sources
# ship the archive to control, extract, then from the control host:
./bin/fleet deploy follow         # P-chain tracks the public network
./bin/fleet status                # expect 12x up, MODE synced, L1 STATE complete
./bin/bombard -rps 4000 -duration 5m
```

### Frozen (airgapped) start

`deploy frozen` needs an archive that already contains the conversions, so the P-chain must sync and be captured first.

```bash
./bin/fleet pchain follow         # first-run initializer, P-chain node only
./bin/fleet status                # wait for MODE=synced and L1 STATE=complete
./bin/fleet pchain archive        # writes ./pchain.tar.gz, refuses to overwrite
./bin/fleet deploy frozen         # freezes the P-chain, then starts the L1 fleet
```

Do not substitute `deploy follow` here: it starts the whole L1 fleet before the archive exists.

### Catching the P-chain up after a new create

A frozen P-chain has no upstream, so anything issued after the freeze (a new `create`, a `set-weight`, a `topup`) is invisible to the fleet. Unfreeze, let it replay, then re-freeze.

```bash
./bin/fleet pchain follow         # unfreeze: restores embedded bootstrappers, restarts, replays
./bin/fleet status                # wait for MODE=synced and L1 STATE=complete
rm pchain.tar.gz                  # archive refuses to overwrite
./bin/fleet pchain archive
./bin/fleet pchain freeze
./bin/fleet deploy frozen         # re-deploys the L1 fleet against the new chain
```

Replay from a recent freeze is seconds. A cold sync from an empty database is minutes (Fuji measured about 6). `deploy frozen` will **not** re-seed from the archive while the remote `db/` is nonempty: an existing database is authoritative. Follow-then-freeze is the only way to move a frozen snapshot forward.

Issue weight changes while the P-chain node is following. A transaction submitted while frozen confirms publicly but does not reach the fleet until it follows again.

### Re-creating the chain

```bash
./bin/l1 destroy                  # reclaims every validator balance, removes deployment/
./bin/l1 create                   # regenerates identities, creates both L1s
# then the catch-up runbook above, since the new conversions are past the frozen height
```

### Drills

```bash
./bin/fleet stop 5 6 7 8          # graceful node loss
./bin/fleet stop dc=B             # graceful DC loss
./bin/fleet destroy dc=B          # abrupt loss: SIGKILL plus L1 chain data wipe
./bin/fleet start dc=B            # bring back, re-pushing assigned identities
./bin/fleet place a 5             # key-swap failover, no restart
./bin/fleet apply-placement       # activate the swap by restarting only mismatched nodes
./bin/l1 set-weight d 100000      # weight-change failover
```

## Files

| Path | Written by | Contents |
|---|---|---|
| `nodes.ini` | operator | machines, roles, optional DC tags. No secrets. |
| `.env` | operator | network, P-chain API, funding key, ssh access |
| `deployment/identities/<letter>` | `keygen` | per-identity TLS and BLS keys |
| `deployment/manager/<letter>` | `keygen` | committee BLS signing keys, control-side only |
| `deployment/genesis-funds.key` | `keygen` | EVM key holding the genesis allocation, used by `bombard` |
| `deployment/public.json` | `keygen` | public handover: NodeIDs, PoPs, initial weights, genesis address |
| `deployment/placement.json` | `keygen`, `place` | machine to identity bijection, control-side truth |
| `deployment/genesis.json` | `create` | rendered genesis, stamped with creation time |
| `deployment/network.env` | `create` | subnet, chain, and conversion transaction IDs |
| `pchain.tar.gz` | `pchain archive` | certified P-chain `db/` snapshot |

`deployment/` holds private keys. It is never in the pack artifact and never committed.

## nodes.ini

```ini
# <node-number> host=<address> role=validator|rpc|pchain [dc=<tag>]
1  host=10.0.0.11 role=validator dc=A
5  host=10.1.0.11 role=validator dc=B
9  host=10.0.0.15 role=rpc       dc=A
13 host=10.2.0.10 role=pchain
```

| Rule | Detail |
|---|---|
| Machines are numbers | data roots, unit names, and inventory keys all use `<n>` |
| Identities are letters | `a`, `b`, `c`, assigned to nodes in ascending number order at keygen |
| Valid shapes | 1 validator + 0 RPC (dev), or >= 4 validators + >= 1 RPC (failover). 2 or 3 validators are refused. |
| Exactly one `pchain` | unregistered, stable TLS identity, no BLS signer, never key-swapped |
| Initial weights | first three validators by node number get 100000, the rest 1000 |
| `dc=` | display and maintenance-selector only. Nothing functional depends on it. |
| Co-location | several nodes may share a host. Ports are positional by node order on that host: 9650/9651, 9652/9653, 9654/9655. |
| Weights are not inventory | on-chain weight is the only truth |

## .env

```dotenv
NETWORK=fuji                              # fuji | mainnet, never "testnet"
PCHAIN_API=https://api.avax-test.network  # no query string, see PCHAIN_API_TOKEN
PCHAIN_API_TOKEN=                         # optional rate-limit bypass, secret, never commit
FUNDING_PRIVATE_KEY=                      # 64 hex chars, no 0x, pays P-chain fees
SSH_USER=ubuntu
SSH_KEY_PATH=/home/ubuntu/.ssh/fleet
```

Unknown fields, missing fields, and malformed values fail the command before it does any work. No aliases, no auto-discovery, no silent repair.

`PCHAIN_API_TOKEN` is sent as the `token` query argument to the `PCHAIN_API` host and to no other host. It cannot be appended to `PCHAIN_API`: the AvalancheGo client overwrites the query string, so a token placed there is silently dropped.

`go run ./cmd/l1 keygen-funding` generates a funding key straight into an empty `FUNDING_PRIVATE_KEY`, chmods `.env` to 0600, and refuses once `deployment/network.env` exists.

## Commands

| Command | Effect |
|---|---|
| `l1 keygen [1\|4]` | offline: all private identities + `public.json` + `placement.json`. Requires `deployment/` absent. Argument is committee size. |
| `l1 create` | on-chain: committee L1 then main L1. Runs `keygen` itself if `deployment/` is absent. |
| `l1 address` | funding addresses and spendable P-chain balance |
| `l1 keygen-funding` | generate `FUNDING_PRIVATE_KEY` into an empty `.env` field |
| `l1 weights` | live weights and fee days left, both L1s, read-only |
| `l1 topup <days>` | raise every registered validator to `<days>` of fee runway |
| `l1 set-weight <letter> <1\|1000\|100000>` | weight-change failover through the committee |
| `l1 destroy` | disable every validator, reclaim balances, remove `deployment/` |
| `fleet deploy <frozen\|follow>` | whole inventory, no selectors. Argument picks the P-chain source. |
| `fleet pchain follow` | first-run initializer and unfreeze. P-chain node only. |
| `fleet pchain freeze` | gate on synced + both validator sets, then switch to empty bootstrap lists |
| `fleet pchain archive` | stop, snapshot `db/`, restart, download, validate, publish `./pchain.tar.gz` |
| `fleet status` | read-only, whole inventory, no selectors |
| `fleet start [sel...]` | stop, re-push identities, start, wait for serving |
| `fleet stop [sel...]` | graceful, preserves data, keys, logs |
| `fleet destroy [sel...]` | SIGKILL + delete `chainData/<chain-id>` only. Simulates abrupt loss. |
| `fleet place <letter> <node>` | swap placement, push keys, restart nothing |
| `fleet apply-placement` | restart exactly the nodes whose runtime identity is wrong |
| `bombard -rps N -duration D` | load generator, fans across all `role=rpc` nodes |

Selector is a node number or `dc=<tag>`; multiple form a union; none means all. `deploy` and `status` take no selectors.

Every mutating fleet command runs fleet-wide phases and aborts before the next phase if any node fails. Rerunning converges. Control-side state is written atomically before remote work.

## fleet status

```text
NODE  DC  ROLE       ID  WEIGHT  STATE  HEIGHT
1     A   validator  a   100000  up     812345
9     A   rpc        i   -       up     812344

P-CHAIN  MODE    LOCAL HEIGHT  UPSTREAM HEIGHT  LAG  L1 STATE  READY TO FREEZE
13       synced  290135        290135           0    complete  yes
```

| Column | Source |
|---|---|
| `ID` | `deployment/placement.json` |
| `WEIGHT` | public P-chain API. `1`, `1000`, `100000`, or `-` for RPC. |
| `STATE` | systemd, collapsed to `up`, `down`, `failed`, `not installed` |
| `HEIGHT` | that machine's own accepted L1 height, raw |
| `MODE` | `bootstrapping` (with pct and eta), `catching-up`, `synced`, `frozen` |
| `L1 STATE` | `complete` when both validator sets are visible, else `partial` or `missing` |

`-` means not applicable or deliberately down. `?` means it should exist but could not be observed. The P-chain machine never appears in the node table; the two sections together cover the whole inventory.

Cross-node height differences are normal at 25ms blocks with ~20ms inter-DC latency and are never flagged. Runtime NodeID is checked against placement and drift is a loud error, not a column.

Exit is nonzero for identity drift, an `up` service whose API cannot answer, and a required P-chain failure. `down` and `not installed` are valid drill states and exit zero.

## The local P-chain never answers platform calls

`--p-chain-follow-only` keeps the P-chain permanently in bootstrap by design: it reaches the tip and tracks read-only, but never hands off to consensus. Consequences, none of which are faults and none of which may gate anything:

- `platform.getHeight` and every local `platform.*` returns `chain is not done bootstrapping`, forever.
- `info.isBootstrapped` never turns true.

| Question | Answer from |
|---|---|
| local absolute height | startup `lastAcceptedHeight` in `P.log` + `avalanche_snowman_bs_accepted{chain="P"}` from `/ext/metrics` |
| is it ready | `bootstrapped` check on `/ext/health` |
| replay progress | `executing blocks` in `P.log`, carries `pctComplete` and `eta` every 5s |
| validator sets, weights | public P-chain API |

```bash
ssh -i ~/.ssh/fleet ubuntu@<pchain-host> \
  'sudo tail -f /var/lib/avalanche-benchmark/<n>/logs/P.log | grep --line-buffered "blocks"'
```

## Topology

- The P-chain node is the sole primary-network bootstrap for every validator and RPC, addressed by `(host:staking-port, NodeID)` and verified by TLS at dial. It therefore cannot participate in key placement.
- L1 state-sync peers for each node are all other validator and RPC nodes, never itself and never the P-chain node.
- Ingress goes only to `role=rpc` nodes. Serving transactions on a validator measurably slows its block production.

## place and apply-placement

`place a 5` moves identity `a` to node 5 **and** node 5's previous identity to `a`'s old node. Placement is always a transposition, so the bijection holds by construction. It writes `placement.json` atomically, then rewrites the assigned identity on every inventory node, and restarts nothing.

`apply-placement` reads each running node's NodeID, compares with placement, and restarts only the mismatched set. Nodes already correct are untouched. Nodes deliberately down (inactive and disabled) stay down; inactive but enabled means an interrupted run and gets brought back.

Refused, for correctness not policy: any swap involving an `rpc` or `pchain` node. Their identities are state-sync seeds and unstaked, and moving a validator onto one silently changes its role.

## set-weight

Accepts exactly `1` (dead), `1000` (spare), `100000` (active). Weight `0` and everything else is refused: this is a fixed-membership benchmark, not a membership manager.

It fetches the validationID and nonce, builds the `L1ValidatorWeight` Warp payload in an `AddressedCall` from the committee chain, signs with the committee BLS keys on control, aggregates a `BitSetSignature`, verifies at the 67% quorum, submits `SetL1ValidatorWeightTx`, and polls until the weight reads back.

Warp admission verifies against the P-chain height pinned in the current ACP-181 epoch, not the latest state. `set-weight` derives the management conversion height from its recorded transaction ID and requires `currentEpoch.pChainHeight >= managementConversionHeight`. If the epoch is older it prints the JST boundary, sleeps to it, submits a visible no-op `BaseTx` to nudge the epoch, and rechecks. A quiet P-chain may need a second nudge.

## Consensus

Fixed benchmark input, identical for every topology including a single validator. Fleet commands never derive consensus settings from inventory.

```
k=20  alphaPreference=11  alphaConfidence=11  beta=12  proposerWindow=50ms
```

Block cadence is 25ms: `min-delay-target` in `chain-config.json` and `initialMinDelayMS` in the genesis. The genesis is stamped with creation time; a genesis stamped `0` would sit before the network's Granite activation, leaving Granite inactive at block zero, silently discarding `initialMinDelayMS`, and starting the chain at the 2000ms ACP-226 default.

`proposerWindowMilliseconds` is the cadence floor whenever the scheduled proposer misses its slot, so it bounds the whole benchmark. 50 is the avalanchego minimum (`subnets.MinProposerWindowMilliseconds`); `0` means the 5s default, not "no window". At 100ms, inter-block times quantized to 101/202/602ms and only a third of blocks reached the 25ms target.

## Measured baseline

2x2 topology, 8 validators, 4 RPCs, `bombard -rps 4000`, 12627 blocks over 323s measured with `scripts/tpsdist.py`:

```
per-second tps   mean 3951   p50 4004   stdev 152   CV 4%   min 3288   below-3000 0/323
block delta ms   p50 25   p75 25   p90 26   p99 43   max 170   at the 25ms floor 12354/12626
bombard          p50 68-81ms   p95 114-165ms   in-flight cap not binding
```

Measure with `scripts/tpsdist.py`, not the bombard TUI. The script reads `timestampMilliseconds` off the blocks, so it reports chain truth; the TUI's mined-tps is observer-side and a watcher that falls behind renders a sawtooth with zero-seconds that never happened. Pass `FROM=<block>` when comparing configs, since a window spanning a restart averages two configs together.

## Gotchas

- **Security groups**: open the staking port (9651, positional per host) in **both** directions between nodes, plus ssh and HTTP from control. Cross-region peering uses public IPs but same-VPC traffic arrives from private IPs, so rules listing only public CIDRs silently break intra-region peering.
- **An EC2 instance cannot reach its own public IP.** The P-chain node cannot live on the control host, because fleet commands reach every node at the address its peers use.
- **Registered validators burn a continuous fee forever.** Run `l1 destroy` when abandoning a deployment.
- **`fleet destroy` is not `l1 destroy`.** The first wipes local L1 chain data to simulate machine loss; the second disables validators on the P-chain and reclaims balances.
- **`bombard -resubmit`** must exceed the worst observed block latency, otherwise a slow chain produces a resubmit storm larger than the issued count.
- **The frozen P-chain must be at or above the height `l1 create` landed at.** `l1 create` issues against the public API, so it advances the real P-chain; a snapshot taken before those transactions leaves the first L1 blocks referencing a height the local P-chain never reaches. Nodes that already hold the blocks keep running (accepted blocks are never re-verified), so the fleet looks healthy until a node rejoins from an empty database and dies with `block P-chain height larger than current P-chain height`. Always follow to the tip and re-freeze **after** `l1 create`.
- The pack artifact contains no private keys. Transfer `.env`, `deployment/`, and the ssh key separately.
