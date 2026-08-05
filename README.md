# Avalanche for Isolated Networks

This is a benchmark and failover toolset for Avalanche L1s in isolated
networks. An isolated network has no internet egress, a fixed validator set,
and PoA. The toolset creates an L1 on Fuji or mainnet and runs it in
isolation. It puts load on the chain. It does data-center failover drills in
two ways: it moves staking identities (key swap), or it moves stake weight.
Both operate on one deployment. No step creates the chain again.

This document is the operator manual. For the consensus parameters, see
**[CONSENSUS-TUNING.md](CONSENSUS-TUNING.md)**.

## What it needs

You need Linux machines with ssh access from one control machine. You need a
P-chain API endpoint, public or your own, for chain creation and weight
changes. You need nothing else. You do not need root: the default install
runs fully under the ssh user, and it works on locked-down RHEL hosts. The
toolset has no provisioning layer. You supply the machines, `nodes.ini`
describes them, and the toolset deploys onto them.

Our test fleet is EC2 in two AWS regions. Some notes below therefore refer
to AWS behavior. AWS is not a requirement. The toolset does not know about
cloud providers. Bare metal, two physical data centers, or twelve VMs on one
hypervisor also work. The `dc=` tags only label nodes in the `fleet status`
table.

## Layout

The repository has two layers and the docs. The **base layer** is this
manual, `cmd/`, `internal/`, and `monitoring/`. It provisions and operates
the chain: deploy, load, failover, and monitoring. The load generator
`bombard` is part of the base layer, because a failover drill needs load.

**Apps** are in `apps/`. An app is a self-contained business case on top of
the base layer. Each app has its own contracts, services, dashboards, and
runbook. Apps do not depend on each other. The first app is
`apps/settlement-feed/`.

Operational procedures are in `playbooks/`: provision, load test, failover
drill, validator swap, rootless install, monitoring, and the connected
P-chain mode. Ready-made inventory shapes are in `examples/`.

## Runbooks

The P-chain node has two modes, and every deployment picks one:

- **frozen** (the default): the P-chain node runs from a captured snapshot
  with no upstream connection. The fleet is fully isolated after the
  deploy. Weight changes need an unfreeze cycle. This is the mode the
  toolset exists for.
- **follow**: the P-chain node tracks the public network continuously. The
  fleet needs egress to `PCHAIN_API`, and `l1 set-weight` works live. See
  [playbooks/07-connected-pchain.md](playbooks/07-connected-pchain.md).

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

### Frozen (isolated) start

`deploy frozen` needs an archive that contains the conversion transactions.
The P-chain node must synchronize first. Then you capture it.

```bash
./bin/fleet pchain follow         # first-run initializer, P-chain node only
./bin/fleet status                # wait for MODE=synced and L1 STATE=complete
./bin/fleet pchain archive        # writes ./pchain.tar.gz, refuses to overwrite
./bin/fleet deploy frozen         # freezes the P-chain, then starts the L1 fleet
```

Do not use `deploy follow` here. That command starts the full L1 fleet
before the archive exists.

### Update the frozen P-chain after a new create

A frozen P-chain has no upstream. It cannot see a transaction that you issue
after the freeze. This applies to a new `create`, a `set-weight`, and a
`topup`. The procedure is: unfreeze, let the node replay, freeze again.

```bash
./bin/fleet pchain follow         # unfreeze: restores bootstrappers, restarts, replays
./bin/fleet status                # wait for MODE=synced and L1 STATE=complete
rm pchain.tar.gz                  # archive refuses to overwrite
./bin/fleet pchain archive
./bin/fleet pchain freeze
./bin/fleet deploy frozen         # re-deploys the L1 fleet against the new chain
```

A replay from a recent freeze takes seconds. A cold synchronization from an
empty database takes minutes. We measured approximately 6 minutes on Fuji.
`deploy frozen` does not restore the archive while the remote `db/` has
content. An existing database is authoritative. Follow-then-freeze is the
only way to move a frozen snapshot forward.

Issue weight changes while the P-chain node follows. A transaction that you
submit while the node is frozen confirms on the public network. The fleet
does not see it until the node follows again.

### Create the chain again

```bash
./bin/l1 destroy                  # reclaims every validator balance, removes deployment/
./bin/l1 create                   # regenerates identities, creates both L1s
# then do the update runbook above: the new conversions are past the frozen height
```

### Drills

```bash
./bin/fleet stop 5 6 7 8          # controlled node loss
./bin/fleet stop 5 6 7 8 11 12    # controlled site loss (site B, written out)
./bin/fleet destroy 5 6 7 8       # sudden loss: SIGKILL plus L1 chain data wipe
./bin/fleet start 5 6 7 8 11 12   # recover, pushes the assigned identities again
./bin/fleet place a 5             # key-swap failover: converge, move, restart the mismatched
./bin/l1 set-weight d 100000      # weight-change failover
```

After a machine reboot, start the P-chain node first with
`./bin/fleet pchain start`. Then run `./bin/fleet start`. Every validator
and RPC node bootstraps from the P-chain node.

## Files

| Path | Written by | Contents |
|---|---|---|
| `nodes.ini` | operator | machines, roles, optional DC tags. No secrets. |
| `.env` | operator | network, P-chain API, funding key, ssh access |
| `deployment/identities/<letter>` | `keygen` | per-identity TLS and BLS keys |
| `deployment/manager/<letter>` | `keygen` | committee BLS signing keys, control-side only |
| `deployment/genesis-funds.key` | `keygen` | EVM key with the genesis allocation, used by `bombard` |
| `deployment/public.json` | `keygen` | public handover: NodeIDs, PoPs, initial weights, genesis address |
| `deployment/placement.json` | `keygen`, `place` | machine-to-identity bijection, control-side truth |
| `deployment/genesis.json` | `create` | rendered genesis, stamped with the creation time |
| `deployment/network.env` | `create` | subnet, chain, and conversion transaction IDs |
| `deployment/genesis-oracle.json` | `create` | rendered oracle genesis (only with oracle roles) |
| `deployment/oracle-feeder.key` | `keygen` | EVM key funded on every price-feed chain, used by `oracle feed`/`relay` |
| `pchain.tar.gz` | `pchain archive` | validated P-chain `db/` snapshot |

`deployment/` contains private keys. It is never in the pack artifact. It is
never committed.

## nodes.ini

```ini
# <node-number> host=<address> role=validator|rpc|pchain|archive|oracle-validator|oracle-rpc [dc=<tag>]
1  host=10.0.0.11 role=validator dc=A
5  host=10.1.0.11 role=validator dc=B
9  host=10.0.0.15 role=rpc       dc=A
13 host=10.2.0.10 role=pchain
14 host=10.0.0.17 role=archive   dc=A
16 host=10.0.0.18 role=oracle-validator dc=A
17 host=10.0.0.15 role=oracle-rpc       dc=A
```

| Rule | Detail |
|---|---|
| Machines are numbers | data roots, unit names, and inventory keys use `<n>` |
| Identities are letters | `a`, `b`, `c`. Keygen assigns them to nodes in ascending number order. |
| Valid shapes | 1 validator + 0 RPC (development), or 4+ validators + 1+ RPC (failover). The toolset refuses 2 or 3 validators. |
| Exactly one `pchain` | not registered, stable TLS identity, no BLS signer, never key-swapped |
| Initial weights | the first three validators by node number get 100000. The others get 1000. |
| `dc=` | display only, in the `fleet status` table. Nothing functional reads it. It is not a selector. |
| Co-location | several nodes can share one machine. Ports are positional by node order on that machine: 9650/9651, 9652/9653, 9654/9655. |
| Weights are not inventory | the on-chain weight is the only truth |
| `archive` | 0 or 2+. An RPC-shaped main-L1 node with pruning and state-sync off (`chain-config-archive.json`). It must exist from genesis, because an archive cannot state-sync. Deploy it like any other node. |
| Oracle roles come together | `oracle-validator` and `oracle-rpc` declare the optional oracle L1 (`subnet-config-oracle.json`, all weights 1000, no key swaps). Omit both for no oracle chain. Each role requires the other. |

## .env

```dotenv
NETWORK=fuji                              # fuji | mainnet, never "testnet"
PCHAIN_API=https://api.avax-test.network  # no query string, see PCHAIN_API_TOKEN
PCHAIN_API_TOKEN=                         # optional rate-limit bypass, secret, never commit
FUNDING_PRIVATE_KEY=                      # 64 hex chars, no 0x, pays P-chain fees
SSH_USER=ubuntu
SSH_KEY_PATH=/home/ubuntu/.ssh/fleet
REMOTE_DIR=                               # install root; empty = /home/<SSH_USER>/avalanche-benchmark
REMOTE_DATA_DIR=                          # optional: databases on a faster disk
SYSTEM_INSTALL=false                      # true = legacy root install (systemd, sudo)
```

An unknown field, a missing field, or a malformed value stops the command
before it does work. There are no aliases, no auto-discovery, and no silent
repair.

The toolset sends `PCHAIN_API_TOKEN` as the `token` query argument to the
`PCHAIN_API` host, and to no other host. Do not append the token to
`PCHAIN_API`. The AvalancheGo client overwrites the query string, so a token
there is silently dropped.

`go run ./cmd/l1 keygen-funding` writes a new funding key into an empty
`FUNDING_PRIVATE_KEY`. It sets `.env` to mode 0600. It refuses to run when
`deployment/network.env` exists.

The install is user-level by default: everything under
`/home/<SSH_USER>/avalanche-benchmark`, no sudo anywhere. `REMOTE_DIR`
overrides the root, `REMOTE_DATA_DIR` puts the databases on a faster disk,
and `SYSTEM_INSTALL=true` selects the legacy root install (systemd,
restart-on-crash, start-on-boot; needs passwordless sudo). See
[playbooks/05-rootless-install.md](playbooks/05-rootless-install.md).

## Commands

| Command | Effect |
|---|---|
| `l1 keygen [1\|4]` | offline: all private identities + `public.json` + `placement.json`. Requires that `deployment/` is absent. The argument is the committee size. |
| `l1 create` | on-chain: the committee L1, then the main L1. Runs `keygen` itself if `deployment/` is absent. |
| `l1 address` | the funding addresses and the spendable P-chain balance |
| `l1 keygen-funding` | write a new `FUNDING_PRIVATE_KEY` into an empty `.env` field |
| `l1 weights` | live weights and fee days left, both L1s, read-only |
| `l1 topup <days>` | raise every registered validator to `<days>` of fee runway |
| `l1 set-weight <letter> <1\|1000\|100000>` | weight-change failover through the committee |
| `l1 destroy` | disable every validator, reclaim the balances, remove `deployment/` |
| `fleet deploy [frozen\|follow] [--dry-run] [sel...]` | the mode is optional; the default is `frozen`. Without selectors: the full inventory, P-chain first. With selectors: a rolling upgrade, one node through all phases before the next. A preflight tests every machine (ssh, tools, writable paths, disk) before any node stops. It changes nothing. `--dry-run` stops after the preflight. |
| `fleet pchain follow` | the first-run initializer, and the unfreeze. P-chain node only. |
| `fleet pchain freeze` | require synced + both validator sets, then switch to empty bootstrap lists |
| `fleet pchain archive` | stop, snapshot `db/`, restart, download, validate, write `./pchain.tar.gz` |
| `fleet pchain start` | start the P-chain node from its installed unit or run script. It needs only the machine, no upstream API. It works on an isolated frozen fleet. After a machine reboot: this first, then `fleet start`. |
| `fleet pchain stop` | a controlled stop. All data stays. L1 nodes continue; a node that bootstraps needs the P-chain node back. |
| `fleet status` | read-only, the full inventory, no selectors |
| `fleet start [sel...]` | safe to repeat: restarts only nodes that are down, on the wrong identity, or not answering. Returns immediately. Does not wait for the nodes to serve. |
| `fleet stop [sel...]` | controlled stop. Data, keys, and logs stay. |
| `fleet destroy <sel...>` | SIGKILL, then delete `chainData/<chain-id>` only. Simulates sudden loss. Node numbers are required. |
| `fleet place <letter> <node>` | reconcile, swap the placement, reconcile again. One move per call. Does not wait for readiness. The only placement verb. |
| `bombard -rps N -duration D` | the load generator. Sends to all `role=rpc` nodes. |
| `oracle feed <node-url>` | the foreground mock price feeder. With an oracle L1, it submits to the aggregator there. Without one, it publishes rounds to the main chain's Chainlink-shaped aggregator with type-2 priority-fee transactions. |
| `oracle relay <oracle-rpc-url> <rpc-url> <staking-ip:port,...>` | the foreground Warp price relayer. It collects signatures from the validators over ACP-118. Oracle L1 deployments only. |

A selector is a node number. Several selectors form a union. No selector
means all nodes. `destroy` is the exception: it refuses to run without
explicit node numbers, because it deletes data. It prints the full-fleet
command, so you can copy it if you really mean every machine. Separate
arguments and comma-separated lists both work: `fleet stop 1 11 12` is
`fleet stop 1,11,12`. `status` takes no selectors.

There is no `dc=<tag>` selector. One that you pass is a loud error. This is
intentional. One `destroy dc=A` would stop half of a two-site fleet with one
keystroke. Half is the worst number. The state-sync beacon list has weight 1
per entry, and `alpha = count/2 + 1` is computed over the list. When half
the entries are down, every survivor is one beacon short of alpha, and no
node can state-sync for the rest of the incident. This applies also to a
node whose local data is already at the network height. Write a site drill
as node numbers. That also makes you handle the RPC nodes separately from
the validators.

**Binary upgrade on a live fleet.** Run `fleet deploy <mode> <node>` one
node at a time. For the P-chain node, run `fleet pchain freeze` (or
`follow`); that reinstalls its package too. Do not redeploy the full fleet
at once for an upgrade. A restart of several nodes together removes the
peers that serve state-sync summaries. A node that cannot get a summary
replays the full chain from genesis. A node with a summary synchronizes in
seconds.

`fleet start` is safe to repeat. It examines each selected machine first. A
node that serves its assigned identity is not touched: not stopped, not
restarted, no new push. Only a node that is down, that runs the wrong
identity, or that does not answer goes through stop, identity, config,
start. A second run is a no-op. A restart of a healthy node has a cost: the
node loses its peers and enters bootstrap again behind the 75% stake gate.
`fleet start 5 6 7` must not stop three healthy machines to repair nothing.

`fleet start` returns when the services are up. It does not wait for the
nodes to serve. This is intentional. A node completes bootstrap only when
75% of the stake is connected; the avalanchego startup tracker is
`(3*bootstrapWeight+3)/4`. In a multi-node recovery, no node can become
ready until the other nodes also run. A start command that waits would
therefore deadlock: node A waits on node B, and node B has not started
because the command still waits on node A. Start the full set. Then watch
`fleet status` until it converges. `deploy` does wait, because it brings up
a coordinated set. `place` does not wait: it restarts the mismatched
machines and returns.

Every fleet command that changes state runs fleet-wide phases. It stops
before the next phase when any node fails. A repeated run converges. The
control-side state is written atomically before the remote work starts.

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
| `WEIGHT` | the public P-chain API. On a frozen fleet: the deployment records, because the sets froze with the archive and the upstream is not reachable by design. The output names the source. Values: `1`, `1000`, `100000`, or `-` for RPC. |
| `STATE` | systemd, collapsed to `up`, `down`, `failed`, `not installed` |
| `HEIGHT` | that machine's own accepted L1 height, raw |
| `MODE` | `bootstrapping` (with percent and eta), `catching-up`, `synced`, `frozen` |
| `L1 STATE` | `complete` when both validator sets are visible, else `partial` or `missing` |

`-` means not applicable, or down on purpose. `?` means the value must exist
but was not observable. The P-chain machine is not in the node table. The
two sections together cover the full inventory.

Height differences between nodes are normal at 25ms blocks with ~20ms
latency between the sites. Status does not flag them. Status compares the
runtime NodeID with the placement. A difference is a loud error, not a
column.

The exit code is not 0 for: identity drift, an `up` service that does not
answer its API, and a required P-chain failure. `down` and `not installed`
are valid drill states and exit 0. A frozen isolated fleet with every node
in production exits 0. The upstream API is treated as not applicable.
Automation can therefore use `status` as a health probe.

## The local P-chain never answers platform calls

`--p-chain-follow-only` keeps the P-chain node in bootstrap permanently.
This is by design. The node reaches the tip and tracks read-only. It never
hands off to consensus. The consequences are not faults. Do not gate
anything on them:

- `platform.getHeight`, and every local `platform.*` call, returns
  `chain is not done bootstrapping`, forever.
- `info.isBootstrapped` never becomes true.

| Question | Answer from |
|---|---|
| local absolute height | the startup `lastAcceptedHeight` in `P.log`, plus `avalanche_snowman_bs_accepted{chain="P"}` from `/ext/metrics` |
| is it ready | the `bootstrapped` check on `/ext/health` |
| replay progress | `executing blocks` in `P.log`; it has `pctComplete` and `eta` every 5s |
| validator sets, weights | the public P-chain API |

```bash
ssh -i ~/.ssh/fleet <user>@<pchain-host> \
  'tail -f /home/<user>/avalanche-benchmark/data/<n>/logs/P.log | grep --line-buffered "blocks"'
```

On a `SYSTEM_INSTALL=true` fleet, the log is at
`/var/lib/avalanche-benchmark/<n>/logs/P.log` and the read needs sudo.
`fleet status` prints the exact watch command for your install.

## Topology

The shipped example is 8 validators and 4 RPC nodes on two sites, one
P-chain node, and one control machine. Nothing below is provider-specific.
Declared oracle and archive nodes share the same machines with positional
ports.

![Two-site benchmark topology](docs/architecture.png)

```mermaid
flowchart LR
    subgraph CONTROL[control machine]
        F["fleet / l1<br/>holds deployment/<br/>ssh to every node"]
        B["bombard<br/>-rps 4000"]
    end

    UP(["public P-chain API<br/>Fuji or mainnet"])
    P["node 13 &nbsp; role=pchain<br/>--p-chain-follow-only<br/>follow: tracks upstream<br/>frozen: no bootstrappers"]

    subgraph A[site A &nbsp; dc=A]
        VA["validators 1-4<br/>identities a b c d"]
        RA["rpc 9-10<br/>identities i j"]
    end

    subgraph SB[site B &nbsp; dc=B]
        VB["validators 5-8<br/>identities e f g h"]
        RB["rpc 11-12<br/>identities k l"]
    end

    F -.->|"ssh: deploy, place, status"| A
    F -.->|ssh| SB
    F -.->|ssh| P
    F -->|"create, set-weight, weights"| UP
    UP -.->|"follow mode only"| P

    B -->|"tx ingress, role=rpc ONLY"| RA
    B -->|tx ingress| RB

    P -->|"sole primary-network bootstrap"| A
    P -->|bootstrap| SB

    VA <-->|"L1 consensus + state sync"| VB
    RA <--> VA
    RB <--> VB
```

Solid lines carry chain traffic. Dashed lines carry the control plane. The
P-chain node is the fleet's only path to the primary network. In frozen
mode, its upstream link is cut. This is what lets the L1 run with no
internet egress.

- The P-chain node is the only primary-network bootstrap for every
  validator and RPC node. Peers address it as `(host:staking-port, NodeID)`
  and verify it by TLS at dial. It therefore cannot take part in key
  placement.
- The state-sync peers of each node are all other validator and RPC nodes.
  A node never lists itself, and never the P-chain node.
- Transaction ingress goes only to `role=rpc` nodes. A validator that
  serves transactions produces blocks measurably more slowly.

## place

`place a 5` moves identity `a` to node 5, **and** the previous identity of
node 5 to the old node of `a`. A placement change is always a transposition.
The bijection therefore holds by construction. `place` is the only placement
verb. It runs three phases:

| Phase | Action |
| --- | --- |
| before | reconcile the fleet to `placement.json`. Silent when already converged. |
| write | update `placement.json` atomically |
| after | reconcile again. This activates the move. |

One call makes one move. The after phase does not wait for the restarted
node to serve. Both properties are intentional. A batch of moves would
restart several machines in one pass. That takes several machines offline
at the same time, and this disruption is what a key-swap failover must
avoid.

A wait for readiness would deadlock the case that `place` exists for. A
node completes bootstrap only when 75% of the stake is connected. When you
move a quorum one identity at a time, some intermediate placements hold
less stake than that gate. A wait would block forever on stake that only
arrives with the next move. A chain of `place` calls does not escape this:
the next call's before phase would wait on the same node. Move a quorum as
a sequence of single `place` calls. Watch `fleet status` until it
converges.

Reconcile means: read the NodeID of each running node, compare it with the
placement, and restart only the mismatched set. The assigned key is pushed
before each restart. A fresh `node.json` goes to every L1 machine, not only
to the restarted ones. Its `state-sync-ips`/`state-sync-ids` list is the
address book each node uses to find the other nodes. Nothing gossips these
addresses: the nodes are not primary-network validators, and the only
bootstrapper does not track the L1. One moved identity therefore makes
every machine's copy wrong. `start`, `stop`, and `destroy` write the file
fleet-wide in the same way.

The address book lists only the machines that are intended to be up. This
is what keeps state sync alive through a site loss. The list is also the
state-sync beacon set, at weight 1 per entry, with `alpha = count/2 + 1`
computed over the list, not over the nodes that answer. An entry for a dead
machine raises the bar and adds nobody who can clear it. Example: twelve
entries with six down leaves five reachable nodes against an alpha of six,
and no node can state-sync.

Intent comes from the systemd enabled flag. `stop` and `destroy` therefore
record it without extra work. A machine that does not answer counts as
down. `deploy` does not read liveness at all: it installs and starts what
it is given, so it renders the full inventory. The refresh reaches running
machines on purpose. The unit is `Restart=on-failure`, so a crash reloads
`node.json` without the toolset. A machine with an address book full of
dead peers would return from a crash unable to synchronize. `fleet status`
reports the intended-up count, because that input lives on the machines and
not in a file on control. Correct nodes are not touched. Nodes that are
down on purpose (inactive and disabled) stay down. A node that is inactive
but enabled is an interrupted run, and reconcile brings it back.

The before phase is the reason there is no separate apply step. A `place`
onto a fleet with a pending move would put two identity changes into one
restart. A failure would then not be attributable to either move. The
before phase converges first, so every `place` is one isolated transition.
A `place` that changes nothing skips the after phase.

`place` refuses every swap that involves an `rpc` or `pchain` node. This is
correctness, not policy. Their identities are state-sync seeds and hold no
stake. A validator identity moved onto one would silently change its role.

## set-weight

`set-weight` accepts exactly `1` (dead), `1000` (spare), and `100000`
(active). It refuses weight `0` and every other value. This is a
fixed-membership benchmark, not a membership manager.

The command fetches the validationID and the nonce. It builds the
`L1ValidatorWeight` Warp payload in an `AddressedCall` from the committee
chain. It signs with the committee BLS keys on the control machine. It
aggregates a `BitSetSignature` and verifies it at the 67% quorum. It
submits `SetL1ValidatorWeightTx`. It polls until the new weight reads back.

Warp admission verifies against the P-chain height that the current ACP-181
epoch pins, not against the latest state. `set-weight` derives the
management conversion height from its recorded transaction ID. It requires
`currentEpoch.pChainHeight >= managementConversionHeight`. When the epoch
is older, it prints the JST boundary, sleeps to it, submits a visible no-op
`BaseTx` to advance the epoch, and checks again. A quiet P-chain can need a
second nudge.

## The direct price feed

Every main-chain genesis contains a Chainlink-compatible feed for USDC/USD.
A `PriceAggregator` is at `0x00000000000000000000000000000000FeedFacE`. The
generated `deployment/oracle-feeder.key` publishes to it. A
`PriceFeedProxy` is at `0x00000000000000000000000000000000FeedF00d`.
Consumers read from the proxy. The ABI is identical to a Chainlink feed:
`latestRoundData`, `getRoundData`, `decimals`, `description`. The
`IPriceFeed` interface matches Chainlink's `AggregatorV3Interface`
signature for signature. On a deployment without oracle roles, this is the
full price pipeline: one process, no extra chain, no relay.

```bash
./bin/oracle feed http://<rpc>:9650
```

`feed` publishes ten rounds per second as type-2 (EIP-1559) transactions.
The priority fee keeps the updates at the front of each block under load.
The block builder orders by effective tip, and the `bombard` flood pays no
tip. The feed bids `max(2 * eth_maxPriorityFeePerGas, 10 wei)`. It wins the
ordering and pays only `baseFee + tip`.

The feeder also reads `latestRoundData` back through the proxy every 500ms.
The exported on-chain series is therefore exactly what a consumer contract
sees. It exports the feed price, the on-chain price, their delta, and the
submit-to-mined latency. The dashboard
`apps/settlement-feed/dashboards/oracle-direct-dashboard.json` shows all
four. The proxy owner can swap the aggregator behind the stable consumer
address with Chainlink's propose/confirm flow. See
`apps/settlement-feed/docs/oracle-consumer.md`.

## The oracle L1

The oracle L1 is an optional third L1. The inventory roles
`oracle-validator` and `oracle-rpc` declare it; nothing else does. Use it
when a validator set must attest the feed, instead of trust in one
key-to-contract path. It ingests mock price feeds (BTC-USD, USDC-USD). It
exports every update to the main L1 as a Warp message that the oracle
validator set signs. All contracts are pre-deployed in genesis. There is
nothing to deploy at run time.

```bash
./bin/oracle feed http://<oracle-rpc>:9650                                        # terminal 1
./bin/oracle relay http://<oracle-rpc>:9650 http://<rpc>:9650 <staking-ip:port,...>  # terminal 2
```

`feed` submits both assets ten times per second, signed by
`deployment/oracle-feeder.key`. `relay` watches the aggregator's Warp
events over a websocket. It has each message signed at the 67% quorum. It
packs all pending messages into one delivery transaction, with one Warp
predicate per message and a maximum of 16. It confirms the delivery on the
main chain. The batch logic has no timer. A single message ships
immediately. A backlog drains through wider batches, not through higher
latency. The receiver contract accepts only the oracle chain plus
aggregator origin. It skips stale sequence numbers.

The relay holds no BLS keys. It requests each signature from the oracle
validators over ACP-118 on their staking ports. This is the icm-services
signature-aggregator wire protocol. It aggregates the replies at the 67%
quorum. The committee keys that `set-weight` uses stay on the control
machine. Only the oracle chain signs its own messages. The collection
latency is exported as `oracle_relay_sign_latency_seconds`. On the test
fleet, a 3-of-4 quorum across three machines costs approximately 5ms at
p50. That is not visible next to the block cadence. The relay uses only odd
requestIDs. Subnet-evm routes AppRequests with even requestIDs to its
legacy sync handlers, and those drop them silently.

Delivery requires an ACP-181 epoch that pins a P-chain height at or past
the oracle conversion. The first message after creation can therefore wait
for one epoch boundary. The relay prints the exact JST boundary it sleeps
to. A freshly created chain also cannot accept its first block until its
first Granite epoch seals. That takes approximately 5 minutes on Fuji.
Start `feed` and `relay` after that window, or restart them until the
deliveries confirm. In production, the relay job belongs to
[icm-relayer](https://github.com/ava-labs/icm-services). This control-host
relayer is the demo equivalent for isolated networks.

## Consensus

The consensus parameters are a fixed benchmark input. They are identical
for every topology, including a single validator. Fleet commands never
derive consensus settings from the inventory. The shipped values are in
`subnet-config.json`:

```
k=60  alphaPreference=31  alphaConfidence=38  beta=12  proposerWindow=100ms
```

`alphaConfidence` is above `alphaPreference` on purpose. With both at the
same marginal value (11 of 20), a sustained tip race under saturation load
let different nodes finalize different siblings of the same parent. We
reproduced this at 2x overload on the test fleet: two 100k validators
continued on one branch, and the rest of the fleet stopped on the other.
Preference can change cheaply. Confidence feeds finality and demands the
stronger majority. The ratio `alphaConfidence/k = 0.633` also clears the
connected-stake query gate with one heavy validator down. The full
derivation and the measurements are in
[CONSENSUS-TUNING.md](CONSENSUS-TUNING.md). Run `tools/forkcheck.sh` after
every load run.

The block cadence is 25ms: `min-delay-target` in `chain-config.json`, and
`initialMinDelayMS` in the genesis. The genesis is stamped with the
creation time. A genesis stamped `0` would sit before the network's Granite
activation. Granite would then be inactive at block zero, the chain would
silently drop `initialMinDelayMS`, and it would start at the 2000ms ACP-226
default.

`proposerWindowMilliseconds` is the cadence floor whenever the scheduled
proposer misses its slot. It therefore bounds the whole benchmark. 50 is
the avalanchego minimum (`subnets.MinProposerWindowMilliseconds`). `0`
means the 5-second default, not "no window". At 100ms, the inter-block
times quantized to 101/202/602ms, and only a third of the blocks reached
the 25ms target.

## Measured baseline

2x2 topology, 8 validators, 4 RPC nodes, `bombard -rps 4000`, 12627 blocks
over 323s, measured from the block timestamps:

```
per-second tps   mean 3951   p50 4004   stdev 152   CV 4%   min 3288   below-3000 0/323
block delta ms   p50 25   p75 25   p90 26   p99 43   max 170   at the 25ms floor 12354/12626
bombard          p50 68-81ms   p95 114-165ms   in-flight cap not binding
```

Measure from `timestampMilliseconds` on the blocks. That is the chain
truth. Do not measure from the bombard screen: its mined-tps is measured at
the observer, and an observer that falls behind shows a sawtooth with
zero-seconds that did not happen. The Grafana chain-TPS panel
(`rate(avalanche_subnetevm_vm_eth_chain_txs_accepted[1m])`) is the durable
form of the same measurement. When you compare configurations, exclude
windows that contain a restart. Such a window averages two configurations.

## Gotchas

- **Firewalls.** The staking port (9651, positional per machine) must be
  open in both directions between every pair of nodes. Also open ssh and
  HTTP from the control machine. A firewall that filters by source address
  needs care: a node can be reached at one address from the other site and
  at a different address from its own site. A rule that lists only the
  external addresses silently breaks local peering. On AWS, cross-region
  peering arrives from public IPs, and same-VPC traffic arrives from
  private IPs. A security group that lists only public CIDRs breaks
  intra-region peering.
- **A machine often cannot reach its own public address.** This happens
  where the public address is a 1:1 NAT and not an address on the
  interface. AWS EC2 and most cloud NAT setups work this way. The P-chain
  node therefore cannot run on the control machine: fleet commands reach
  every node at the address its peers use.
- **Registered validators pay a continuous fee, forever.** Run
  `l1 destroy` when you abandon a deployment.
- **`fleet destroy` is not `l1 destroy`.** The first deletes local L1 chain
  data to simulate machine loss. The second disables the validators on the
  P-chain and reclaims the balances.
- **`bombard -resubmit`** must be larger than the worst observed block
  latency. A smaller value on a slow chain produces a resubmit storm larger
  than the issued count.
- **The frozen P-chain must be at or above the height of `l1 create`.**
  `l1 create` issues against the public API, so it advances the real
  P-chain. A snapshot from before those transactions makes the first L1
  blocks reference a height the local P-chain never reaches. Nodes that
  already hold the blocks continue, because accepted blocks are not
  verified again. The fleet therefore looks healthy until a node rejoins
  from an empty database and stops with
  `block P-chain height larger than current P-chain height`. Always follow
  to the tip and freeze again **after** `l1 create`.
- **Do not restart a node within a minute of a restart of its state-sync
  sources.** State sync first asks the peers which block to sync to. The
  answer is weighted by stake and needs alpha weight. When no peer can
  answer yet, the only summary on offer is genesis. Subnet-evm then skips
  state sync, because `lastAccepted + state-sync-min-blocks > summaryHeight`
  is trivially true at `0 + n > 0`. The node replays from block 0: 180k
  blocks and half an hour, against approximately 30 seconds with a healthy
  summary. Recover nodes one at a time, or wait until the sources reach the
  tip. The log line is `syncMode: "Skipped"` with `syncableHeight=0`.
- The pack artifact contains no private keys. Transfer `.env`,
  `deployment/`, and the ssh key separately.
