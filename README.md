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

Two L1s, one fleet, one P-chain source:

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
- **The P-chain source**: one foreground AvalancheGo process on the control
  host. Its database and NodeID never change. The required start argument
  selects following or frozen configuration before AvalancheGo starts.

| Mode | Source bootstrap lists | P-chain |
|---|---|---|
| **Following** | fields omitted; packaged AvalancheGo defaults | advances from the public network |
| **Frozen** | IPs and NodeIDs explicitly empty | remains at the last accepted height |

`fleet` does not daemonize, restart, or supervise the source. Stop the
foreground process and start it with the other mode to transition. Omitted
bootstrap flags are not frozen: AvalancheGo loads its built-in network
bootstrappers. Validators bootstrap from the fleet's RPC nodes, and RPC nodes
bootstrap from the stable source `(IP, NodeID)`.

The client control host has internet access, so the supported workflow is
follow, create or update the desired P-chain state, then freeze. There is no
snapshot shipment, archive import, USB workflow, reset command, or second
P-chain process.

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

- Machines and nodes are numbers: inventory keys and data roots use `<n>`.
  Identities are immutable lowercase letters stored under
  `deployment/identities/<letter>`. At creation, `a` starts on the first node
  in ascending numeric order, `b` on the second, and so on. Key swaps change
  placement, never the identity name.
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
SSH_USER=ubuntu
SSH_KEY_PATH=/path/to/fleet-key
```

`NETWORK` is `fuji` or `mainnet`. Fuji is always called Fuji, never
"testnet". `PCHAIN_API` is required. Committee size is an optional argument to
`keygen`, not environment state: omit it for 1 or pass 4.
`FUNDING_PRIVATE_KEY` contains the raw 32-byte secp256k1 private key as 64 hex
characters with no `0x` prefix. It pays P-chain creation and validator fees.
The Genesis allocation belongs to a separate key generated by `keygen`.

To generate a new identity directly into an existing `.env` whose
`FUNDING_PRIVATE_KEY` is empty:

```bash
go run ./cmd/l1 keygen-funding
```

The private key is network-agnostic. `keygen-funding` never prints it, never overwrites
an existing key, and is valid only before `deployment/network.env` exists. This
prevents replacing the identity that owns an existing deployment. It protects
the updated `.env` with mode `0600`, then runs the same inspection as `address`,
using `.env`'s explicit network to show the P-Chain funding address, funding-key EVM
address, and current P-Chain balance.

To inspect the configured identity and its spendable P-chain balance without
submitting a transaction:

```bash
go run ./cmd/l1 address
```

Before creation, `address` remains available so an imported funding key can be
funded. While `deployment/network.env` exists, it fails if that deployment has
no active validators because `destroy` has not finished removing its local
state.

Configuration is strict. Missing required fields, unknown fields, duplicate
node numbers, malformed values, and missing prior-step artifacts stop the
command before it performs work. Errors name the exact field, path, or required
prior command. There are no legacy variable aliases, guessed paths,
auto-discovery, or silent repairs. Every generated artifact and submitted
transaction is reported explicitly.

## Commands

```
l1 keygen [1|4]        offline: fresh private bundle + public inputs; default 1
l1 create              on-chain: committee L1 + main L1 from public inputs
l1 address             show funding addresses and spendable P-chain balance
l1 keygen-funding      generate FUNDING_PRIVATE_KEY directly into an empty .env field
l1 weights             show identity letters, NodeIDs, weights, and fee days left
l1 topup <days>        fund every registered validator to <days> of runway
l1 set-weight <letter> <w> set main identity to 1, 1000, or 100000
l1 destroy             disable every converted L1 validator and reclaim its balance
fleet pchain start <following|frozen>
```

### keygen and create

Run `keygen` on the client's secure machine:

```bash
go run ./cmd/l1 keygen
# go run ./cmd/l1 keygen 4
```

It reads only `nodes.ini`, requires `deployment/` to be absent, and generates
fresh TLS+BLS identities for validators, TLS-only identities for RPCs,
TLS+BLS identities for managers, and `genesis-funds.key`. It writes
`deployment/public.json` with the Genesis EVM address and every public identity,
NodeID, initial weight, and required PoP. RPCs have no signer or PoP because
they are never registered. The first three validators by ascending node number
receive weight 100000, remaining validators receive 1000, and managers receive
1000.

Run `create` on a machine with P-chain access:

```bash
go run ./cmd/l1 create
```

The creation machine needs `.env` and `deployment/public.json`. It does not
need `nodes.ini` or any generated private key. `create` verifies every PoP,
prints the manifest SHA-256, Genesis EVM address, and registered validator
roster before spending anything. It renders `genesis.json`, writes
`network.env` with accepted IDs and transaction IDs, and then issues, in order:
`CreateSubnetTx` + `CreateChainTx` + `ConvertSubnetToL1Tx` for the committee L1
(1 or 4 members, defined by the manifest, weight 1000), then the same for the
main L1 with the committee chain recorded as validator manager. Initial weights
are copied verbatim from `public.json` into the conversions. No Warp message is
constructed at creation time. The committee is not exercised until the first
`set-weight`.

A failed partial creation is abandoned. The operator runs `l1 destroy` to
reclaim any validator balances whose conversion already succeeded and remove
its output before starting another clean attempt. `FUNDING_PRIVATE_KEY` from
`.env` only pays for and owns the P-chain transactions. Every registered
validator starts with a 0.1 AVAX continuous-fee balance. Genesis allocates its
funds to the separate public EVM address in `public.json`.

Before submitting the first transaction, `create` checks that the configured
P-chain address has 0.1 AVAX for every main and committee validator plus a
0.1 AVAX transaction-fee reserve. The reserve is a fail-fast safety margin,
not an additional transfer. An insufficient wallet fails before rendering
Genesis or mutating the P-chain.

Writing and then rereading `public.json` is intentional even when both commands
run on one machine. It tests the exact public-only handover used when client key
generation and chain creation happen on separate machines. Both commands print
the file's SHA-256 so the operator can verify the copy.

Creation does not freeze the P-chain. The L1 may be pre-created on another
machine. On the deployment control host, run `fleet pchain start following`
until the source accepts through both conversion transactions. Stop it, then
run `fleet pchain start frozen`.

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
ordered by identity letter. The report shows the price in nAVAX per second
and its equivalent 30-day cost in AVAX per validator. It submits no transaction
and does not treat generated artifacts as weight truth.

### destroy

`l1 destroy` permanently disables every active main validator, then every
active management validator. It accepts partial creation state and reclaims
whichever converted L1s exist, including a management-only creation that failed
before the main conversion. Every other lifecycle command still requires a
completed creation. Before submitting the first transaction it
verifies that `FUNDING_PRIVATE_KEY` is both the deactivation owner and the
remaining-balance owner of every validator. Each `DisableL1ValidatorTx` stops
that validator's continuous fee and returns its remaining balance to the
funding key's P-Chain address. The command prints one accepted transaction ID
per validator and can be rerun after a partial failure. Height-consistent
zero-balance records are treated as already disabled even if a stale membership
response still lists them. If any transaction or verification fails, the
complete `deployment/` directory stays in place so the operator can rerun
`destroy`. After every balance is reclaimed, the command removes `deployment/`,
including its private keys and transaction state. If the validators were
already disabled but the directory remained from an interrupted or older
cleanup, `destroy` removes it without another transaction. `.env` and
`nodes.ini` are never removed.

After successful destruction, the absence of `deployment/` means the workspace
is ready for a new `create`. There is no local destroyed flag.

### place: key-swap failover

`place a 5` puts identity `a` on node 5 **and the identity previously on node
5 on identity `a`'s old node**. Placement is always a transposition, so the
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

The `IDENTITY` column from `weights` is the lowercase-letter argument to
`set-weight`. `set-weight d 100000` fetches identity `d`'s main L1 validator (validationID,
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

P-chain Warp admission verifies against the P-chain height pinned in the current
ACP-181 epoch, not the latest state returned by `weights`. Before constructing
or submitting a weight transaction, `set-weight` derives the management
conversion's exact block height from its already-recorded transaction ID,
without storing another field. It then makes one `proposervm.getCurrentEpoch`
preflight and requires
`currentEpoch.pChainHeight >= managementConversionHeight`.

If the pinned height is older, no weight transaction is attempted yet. Before
the epoch can seal, the command prints the exact JST boundary and sleeps until
it. It then submits a visible no-op P-chain `BaseTx` from the same wallet and
rechecks the epoch. A quiet P-chain may need a second visible no-op because
epoch advancement tests the new block's parent timestamp. As soon as the epoch
pins the conversion height, `set-weight` continues normally. The readiness gate
is automatic; the weight transaction itself is constructed and submitted only
once, and any weight-transaction failure remains an immediate error.

Run weight changes while the P-chain source is following. A transaction
submitted while the source is frozen can confirm publicly without reaching the
fleet until the source follows again.

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
- RPC nodes point at the **P-chain source**.

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

### P-chain source lifecycle

```bash
./bin/fleet pchain start following
# Ctrl-C after the required P-chain state is accepted
./bin/fleet pchain start frozen
```

`start` writes the selected configuration and replaces `fleet` with the
packaged AvalancheGo binary. AvalancheGo owns the foreground terminal, logs,
signals, and exit status. There is no systemd unit, background process,
automatic restart, separate mode file, or status command.

`following` omits both bootstrap fields, so AvalancheGo reads its own embedded
`genesis/bootstrappers.json`. Updating AvalancheGo updates the defaults without
copying them into this tool. `frozen` writes both bootstrap fields as explicit
empty strings. Both modes reuse `data/pchain-source/`, including its database
and staking identity.

While the source is stopped, already-running fleet nodes continue. A fleet
node that starts while its configured source is unavailable cannot pass its
bootstrap-beacon gate, even if it has an existing local P-chain database.

## Quick start

```bash
cp nodes.ini.example nodes.ini    # edit host= lines
cp .env.example .env              # choose fuji or mainnet and set key paths
# fund the P-chain address derived from FUNDING_PRIVATE_KEY

go run ./cmd/l1 keygen      # fresh private bundle + public inputs, one manager
# go run ./cmd/l1 keygen 4  # four-manager alternative
go run ./cmd/l1 create      # create both L1s from public inputs
make pack
# copy remote-benchmark.tar.gz to the control host and extract it
./bin/fleet pchain start following
```

The P-chain source data lives under `data/pchain-source/`. Do not delete that
directory during mode changes or package updates. It contains the preserved
P-chain database and staking identity.

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
  and reclaim every remaining validator balance. Successful cleanup also
  removes `./deployment`.
