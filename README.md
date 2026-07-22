# Avalanche for Isolated Networks

Benchmark and failover toolset for Avalanche L1s running inside isolated
networks: no internet egress, fixed validator membership (isolation implies
PoA: permissionless stake requires open peering, which an isolated network
forbids). Stand up an L1 on Fuji or mainnet, run it airgapped, drive
it with transaction load, and exercise data-center failover: first by moving
staking identities, later by moving stake weight, on one deployment, with no
chain re-creation between the two.

Design rationale: **[DESIGN.md](DESIGN.md)**. This README is the operator manual.

## Architecture

Two L1s, one fleet, one relay:

- **Main L1**: the benchmarked chain (subnet-evm). Its validators are your
  fleet. Registered once via `ConvertSubnetToL1Tx` with the management chain set
  to the committee L1's blockchain (manager address `0x…01`).
- **Committee L1**: the validator manager. Its management chain is
  never deployed and never produces blocks; it exists as P-chain BLS-key
  records (1 member by default, or 4 for one-signer-loss tolerance; weight
  1000 each). It is self-managed through its own management chain at manager
  address `0x…01`. Weight changes to the main L1
  are `SetL1ValidatorWeightTx`s carrying a Warp `AddressedCall` sourced from
  the committee chain, signed by an aggregated BLS `BitSetSignature` meeting
  the 67% quorum. The committee's keys live on the control host; **nodes never
  sign management messages**. During a DC outage the dead validators' stake
  must still be movable.
- **The relay**: a follow-only P-chain proxy on the control host, the fleet's
  single allowed external TCP. The fleet's P-chain state has exactly two
  regimes, distinguished only by relay state:

| Mode | Relay | P-chain | Failover mechanism |
|---|---|---|---|
| **Frozen P-chain** | down / absent | last delivered snapshot, served as a frozen frontier | key swap (`place`) |
| **Proxied P-chain** | up | live, streamed through the relay | weight change (`set-weight`) |

There is no per-node configuration difference between the modes. Validators
bootstrap from the fleet's RPC nodes; RPC nodes bootstrap from the relay's
`(IP, NodeID)`. A down relay is just an unreachable peer. **Going live is
starting one process on control.** Nothing is re-registered, re-keyed, or
redeployed.

Every node runs `--partial-sync-primary-network`: the C and X chains are never
synced, in either mode. Each node's P-chain database is seeded from a snapshot
taken on control. This is mandatory in both modes, since bootstrapping a fleet's
P-chain from genesis through one relay is not practical. Snapshot = seed;
relay = feed.

## Inventory: nodes.ini

```ini
# <node-number> host=<address> role=validator|rpc [dc=<tag>]
1  host=10.0.0.11 role=validator dc=A
2  host=10.0.0.12 role=validator dc=A
3  host=10.0.0.13 role=validator dc=A
4  host=10.0.0.14 role=validator dc=A
5  host=10.1.0.11 role=validator dc=B
6  host=10.1.0.12 role=validator dc=B
7  host=10.1.0.13 role=validator dc=B
8  host=10.1.0.14 role=validator dc=B
9  host=10.0.0.15 role=rpc       dc=A
10 host=10.0.0.16 role=rpc       dc=A
11 host=10.1.0.15 role=rpc       dc=B
12 host=10.1.0.16 role=rpc       dc=B
```

- Node numbers are the primary key everywhere: data roots (`data/<n>`),
  identity dirs (`deployment/nodes/<n>`), command arguments.
- Validators are registered in ascending node-number order. The first three
  validators in that order start at weight 100000; the rest start at 1000.
- Inventory requires at least four validators and at least one RPC.
- `role` is the only functional field. `validator` = registered on-chain,
  carries stake, swappable. `rpc` = never registered, no BLS signer key
  (runs `--staking-ephemeral-signer-enabled`), pinned identity, serves
  ingress and anchors bootstrap.
- `dc` is an optional freeform display/selector tag. If omitted, it remains
  visibly unset. Fleet verbs accept `dc=<tag>` selectors and `status` groups by
  it. Nothing functional depends on it.
- Weights are **not** inventory. On-chain weight is the sole truth; `status`
  reads it from the P-chain.
- There is no generated registry or NodeID manifest. NodeIDs are derived from
  TLS certificates; validation IDs, weights, and active state come from the
  P-chain.
- Several nodes may share a host (ports are positional: the k-th node on a
  host gets HTTP `9650+2k`, staking `9651+2k`), but the intended shape is one
  node per machine, permanently. Identities move; nodes do not.

Copy `nodes.ini.example` to `nodes.ini`, then edit its hosts and optional DC
tags. Inventory defines machines and roles only. It does not contain secrets.

## Configuration: .env

Every command explicitly loads `.env` from the repository root. Start with:

```bash
cp .env.example .env
```

The creation settings are deliberately small:

```dotenv
NETWORK=fuji
PCHAIN_API=https://api.avax-test.network
FUNDING_PRIVATE_KEY=
MANAGER_COMMITTEE=1
SSH_USER=ubuntu
SSH_KEY_PATH=/path/to/fleet-key
```

`NETWORK` is `fuji` or `mainnet`. Fuji is always called Fuji, never
"testnet". `PCHAIN_API` is required. `MANAGER_COMMITTEE` accepts only `1` or
`4`; the example explicitly selects `1`.
`FUNDING_PRIVATE_KEY` contains the raw 32-byte secp256k1 private key as 64 hex
characters with no `0x` prefix. The same key pays P-chain creation and
validator fees, and its derived EVM address receives the main L1's genesis
allocation. There is no key-file setting, second key, built-in funded account,
or fallback private key.

To generate a new identity directly into an existing `.env` whose
`FUNDING_PRIVATE_KEY` is empty:

```bash
go run ./cmd/l1 keygen
```

The private key is network-agnostic. `keygen` never prints it, never overwrites
an existing key, and is valid only before `deployment/network.env` exists. This
prevents replacing the identity that owns an existing deployment. It protects
the updated `.env` with mode `0600`, then runs the same inspection as `address`,
using `.env`'s explicit network to show the P-Chain funding address, EVM genesis
address, and current P-Chain balance.

To inspect the configured identity and its spendable P-chain balance without
submitting a transaction:

```bash
go run ./cmd/l1 address
```

Before creation, `address` remains available so an imported funding key can be
funded. Once `deployment/network.env` exists, it fails if that deployment has
no active validators because destruction is a terminal lifecycle state.

Configuration is strict. Missing required fields, unknown fields, duplicate
node numbers, malformed values, and missing prior-step artifacts stop the
command before it performs work. Errors name the exact field, path, or required
prior command. There are no legacy variable aliases, guessed paths,
auto-discovery, or silent repairs. Every generated artifact and submitted
transaction is reported explicitly.

## Commands

```
l1 create              one-time, on-chain: committee L1 + main L1 (see below)
l1 address             show funding addresses and spendable P-chain balance
l1 keygen              generate FUNDING_PRIVATE_KEY directly into an empty .env field
l1 weights             show live identity numbers, NodeIDs, weights, and fee days left
l1 topup <days>        fund every registered validator to <days> of runway
l1 destroy             disable both L1 validator sets and reclaim their balances
l1 reset               provision (unconditional rsync) + seed P-chain + start
                       everything at canonical placement (identity i on node i)
l1 place <id> <node>   SWAP identity <id> with whatever identity node <node> runs
l1 set-weight <id> <w> set main identity <id> to 1 (dead), 1000 (spare), or 100000 (active)
l1 down <n|dc=X>       kill node(s), wipe ONLY the L1 chain data (P-chain kept)
l1 up <n|dc=X>         start node(s); state-sync the L1, wait until at tip
l1 relay start|stop    the P-chain proxy on control = the mode switch
l1 snapshot            sync the selected P-chain on control, produce the seed
l1 status | verify | endpoints        read-only
bombard                load generator (drives the RPC nodes)
```

### create

Run from any designated creation machine with P-chain access. This does not
have to be the client's deployment control machine. It explicitly loads `.env` and
reads validators from `nodes.ini` in ascending node-number order. It generates
fresh TLS and BLS staking keys for validators, stable TLS identities without
BLS signer keys for RPC nodes, and fresh TLS+BLS identities for the manager
committee. It never loads or reuses an existing identity. Before any P-chain
transaction, `create` requires an empty creation-output directory and names the
blocking path if old artifacts are present. That directory is `deployment/`.
It contains `nodes/<n>/`, `manager/<n>/`, the rendered `genesis.json`, and
`network.env` with accepted IDs and transaction IDs. It then issues, in order:
`CreateSubnetTx` + `CreateChainTx` + `ConvertSubnetToL1Tx` for the committee L1
(1 or 4 members, default 1, weight 1000), then the
same for the main L1 with the committee chain recorded as validator manager.
**Initial weights are written directly into the main L1's conversion: the
first 3 validators at 100000, the rest at 1000.** No Warp message is
constructed at creation time. The committee is not exercised until you first
call `set-weight`. A failed partial creation is abandoned. The operator explicitly
removes its output before starting another clean attempt with new identities
and new L1s. It requires `FUNDING_PRIVATE_KEY` from `.env`. Every main and
committee validator starts with a 0.1 AVAX continuous-fee balance. Before creating the main chain, `create`
renders its genesis allocation for the funding key's derived EVM address. A
static pre-funded address is never accepted.

Before generating identities or submitting the first transaction, `create`
checks the configured P-chain address. It requires 0.1 AVAX for every main and
committee validator plus a 0.1 AVAX transaction-fee reserve. The reserve is a
fail-fast safety margin, not an additional transfer. An insufficient wallet
fails before creating `deployment/` or mutating the P-chain.

Creation does not freeze the P-chain. The L1 may be pre-created on another
machine. The client or deployment control machine later syncs past both
conversions and runs `snapshot`; the resulting local P-chain frontier is the
frozen seed shipped to the isolated fleet.

### topup

`l1 topup 20` accepts exactly one positive-integer days argument, reads the
current continuous-fee price from the selected P-chain, and raises every
registered management and main validator to the corresponding balance target.
Before submitting anything, it checks that the funding wallet covers all
shortfalls plus a 0.1 AVAX transaction-fee reserve. It prints exactly one line
per validator: either the accepted transaction ID or `already had X.XX days`.
Balances below the requested minimum are raised to one hour beyond it so API
lag and sequential transaction settlement do not leave the final set just
under the requested minimum.

### weights

`l1 weights` is read-only. It requires a completed `deployment/network.env`,
prints the management and main chain IDs, reads both L1s' current validator
sets from the selected P-chain API, and labels every management and main NodeID
with its live weight and remaining fee balance in days at the current validator
fee price. Management and main validators are printed in separate tables, each
ordered by local identity number. The report shows the price in nAVAX per second
and its equivalent 30-day cost in AVAX per validator. It submits no transaction
and does not treat generated artifacts as weight truth.

### destroy

`l1 destroy` permanently disables every active main validator, then every
active management validator. Before submitting the first transaction it
verifies that `FUNDING_PRIVATE_KEY` is both the deactivation owner and the
remaining-balance owner of every validator. Each `DisableL1ValidatorTx` stops
that validator's continuous fee and returns its remaining balance to the
funding key's P-Chain address. The command prints one accepted transaction ID
per validator and can be rerun after a partial failure. Height-consistent
zero-balance records are treated as already disabled even if a stale membership
response still lists them. The command keeps `deployment/network.env` as the
record of which chain was destroyed.

Destruction is terminal. Repeating `destroy`, or running `address`, `weights`,
or `topup` against the destroyed deployment, fails with `deployment has no
active validators` rather than reporting a successful no-op or stale state.
There is no local destroyed flag. Height-consistent P-Chain state is the only
lifecycle truth, and zero active validators means the chain is destroyed.

### place: key-swap failover

`place 1 5` puts identity 1 on node 5 **and identity-previously-on-5 on
identity 1's old node**. Placement is always a transposition, so the
identity↔node bijection is preserved by construction. An identity can never
be live on two nodes (that's equivocation, and it is structurally
inexpressible, not merely checked). Execution is two-pass: stop both nodes,
swap key material on disk, then start both; the identities are never live
crossed mid-move. Control pushes the key files at swap time (~1 KB over ssh);
nodes hold only their currently-active identity.

Two refusals, both correctness rather than policy:
1. an identity that would end up live twice (cannot be expressed anyway);
2. any swap involving an `rpc` node. RPC identities are bootstrap anchors
   (see below) and unstaked, so moving them breaks the mesh, and moving a
   validator onto an RPC slot silently de-anchors it.

Everything else, including which identity goes where, when, and why, is yours. The tool
ships mechanisms, not failover policy.

### set-weight: weight-change failover

The `IDENTITY` column from `weights` is the numeric argument to `set-weight`.
`set-weight 4 100000` fetches identity 4's main L1 validator (validationID,
nonce) from
the P-chain, builds the `L1ValidatorWeight` Warp payload, wraps it in an
`AddressedCall` from the committee chain, signs with the committee BLS keys
held on control, aggregates to a `BitSetSignature`, verifies locally at the
67/100 protocol quorum, and submits the `SetL1ValidatorWeightTx`. It polls
until the weight reads back from the P-chain.

It accepts exactly three weights: `1` means dead, `1000` means spare, and
`100000` means active. Weight `1` is the minimum. Weight `0` and every other
value are refused, so the tool cannot remove membership or create unexplained
intermediate states. This is a performance/failover benchmark with fixed
membership, not a membership manager.

P-chain Warp admission can verify against an ACP-181 epoch height older than the
latest state returned by `weights`. After a fresh management L1 conversion on a
quiet P-chain, that epoch can predate the entire management validator set. On
the resulting exact empty-set rejection, the weight transaction is not
accepted. `set-weight` visibly submits one no-op P-chain `BaseTx` from the same
wallet to force a new block so the verifier context can advance, prints its
transaction ID, exits with the original rejection, and tells the operator to
rerun the command. It never hides the failure in an automatic retry loop.

In frozen mode the transaction would confirm on the P-chain but never reach your
fleet, so `set-weight` refuses to run when the relay is down.

### down / up: recovery primitive

`down` wipes only `chainData/<L1-chain-id>` and logs, never the P-chain DB
(re-syncing the P-chain costs hours; re-state-syncing the L1 costs
seconds). A downed node therefore can never return on a stale fork: `up`
state-syncs the L1 fresh onto the majority branch. This is also the fork
recovery procedure for a node that diverged without dying: find nodes whose
accepted block hash differs from the majority of high-weight validators
(`verify` does this), then `down` + `up` them.

`down dc=B` / `up dc=B` batch over the tag. That is the whole-DC drill.

## Bootstrap topology

Two-hop, and the reason RPC identities are pinned:

- Validators' `--bootstrap-ips/ids` (and state-sync lists, which are the same list) point
  at the fleet's **RPC nodes**.
- RPC nodes point at the **relay** upstream.

Bootstrap entries are `(IP, NodeID)` pairs verified by TLS at dial time, so
anchors must never change identity. This is exactly why `place` refuses RPC
nodes, and why validators (which nothing anchors on) are free to swap.
Explicit lists are required: with `--partial-sync-primary-network` and no
egress, peers are **not** discovered from P-chain records.

Why RPCs exist at all: serving transaction ingress on a validator measurably
slows its block production. RPCs take the load (`bombard` fans across all of
them and resubmits in-flight transactions, so ingress rides through drills);
validators only validate.

## The benchmark

The scenario is a bank-style integrity workload: a single writer, ordered
linear nonces, high-value transactions, and the requirement that a
data-center failover loses and reorders nothing. The application-level
backstop assumed throughout: because there is one sender with linear nonces
recorded in a registry, the majority branch is canonical by definition, a
nonce gap is detectable, and replaying the last few minutes heals it. An
already-accepted nonce is rejected, only missing transactions land, a fork
cannot forge an unsigned transaction. Consensus-level failover therefore does
not need to be perfectly lossless; it needs to be fast and convergent.

Consensus is tuned so that losing one high-weight validator (~66% of stake
remaining) still finalizes at four-digit TPS: `alpha` sits just below the
maximum, and `k` must not exceed the validator count (`k=5, alpha=4` for the
flat 8-validator shape; `k` larger than the set never finalizes:
`errInsufficientWeight`, blocks build but are never accepted).

There is no scores file. The deliverable is the drill itself plus the live
dashboards: run `bombard`, run a drill, watch throughput, finalized height per
node, and stake placement move in Grafana (`04_monitoring.sh` runs
Prometheus + Grafana on control, scraping every node's `/ext/metrics`).

### Drills

```bash
# Frozen mode: DC-B dies; move its high identities onto DC-A spares.
./bin/l1 down dc=B
./bin/l1 place 1 5        # dead high identity -> live low node (repeat per identity)
./bin/l1 up dc=B          # later: nodes rejoin, state-sync, wear the ridden-back identities

# Proxied mode: same failure, no identity moves at all.
./bin/l1 relay start      # once; this is the mode transition
./bin/l1 down dc=B
./bin/l1 set-weight 5 100000  # promote a spare validator to active
./bin/l1 set-weight 1 1       # demote the failed validator to dead
```

Both drills run under load. The two mechanisms coexist on one deployment and
cannot corrupt each other: weight attaches to the identity wherever it sits,
and `place` preserves the identity↔node bijection.

## Quick start

```bash
cp nodes.ini.example nodes.ini    # edit host= lines
cp .env.example .env              # choose fuji or mainnet and set key paths
# fund the P-chain address derived from FUNDING_PRIVATE_KEY

go run ./cmd/l1 create # fresh on-chain committee + main L1
./bin/l1 snapshot      # client/control syncs the P-chain and builds the seed
./bin/l1 reset         # rsync artifacts + seed P-chain + start all nodes
./04_monitoring.sh     # Prometheus + Grafana on control
./bin/l1 status        # all nodes SERVING, weights read from the P-chain
./05_benchmark.sh      # bombard at the RPC nodes
```

`reset` provisions unconditionally (rsync, idempotent, near-instant when
unchanged; there is deliberately no "already provisioned" check to go stale)
and restores canonical placement: identity `i` on node `i`.

## Operational notes

- Open between nodes: the staking port (default 9651, positional per host)
  in **both** directions, and from control: ssh + HTTP. Cross-region peering
  uses public IPs, but same-VPC traffic arrives from private IPs. SG rules
  listing only public CIDRs silently break intra-region peering.
- The public package contains no private keys or generated deployment secrets.
  Transfer the populated `.env`, committee keys, validator staking keys, and
  generated deployment state separately as a private handover bundle. Nodes
  hold only their active identity.
- Networks: set `NETWORK=fuji` or `NETWORK=mainnet` explicitly and provide the
  matching `PCHAIN_API` explicitly.
- Cleanup: registered L1 validators pay a continuous fee forever. Run
  `go run ./cmd/l1 destroy` when abandoning this deployment to stop the burn
  and reclaim every remaining validator balance.
