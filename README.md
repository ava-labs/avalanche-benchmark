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

Two L1s, one fleet, one P-chain node:

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
- **The P-chain node**: exactly one inventory node running AvalancheGo as a
  systemd service. It has a fresh stable TLS identity, owns the fleet's P-chain
  state, and is not registered on either L1. Initial `fleet deploy` requires an
  explicit `frozen` or `follow` mode. Deploy waits until the P-chain node sees
  both converted L1 validator sets before touching any validator or RPC
  service. Every validator and RPC then uses the P-chain node's inventory
  address and generated NodeID as its sole P-chain bootstrap.

Switching the deployed P-chain node between following and frozen is a separate
lifecycle step after initial deployment.

## Inventory: nodes.ini

```ini
# <node-number> host=<address> role=validator|rpc|pchain [dc=<tag>]
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
13 host=10.2.0.10 role=pchain
```

- Machines and nodes are numbers: inventory keys and data roots use `<n>`.
  Identities are immutable lowercase letters stored under
  `deployment/identities/<letter>`. At creation, `a` starts on the first node
  in ascending numeric order, `b` on the second, and so on. Key swaps change
  placement, never the identity name. `deployment/placement.json` is the
  generated control-side source of truth mapping each machine to its current
  identity. Only `fleet place` changes it.
- Validators are registered in ascending node-number order. Up to the first
  three validators in that order start at weight 100000; the rest start at
  1000.
- A development inventory may contain exactly one validator and no RPC. A
  failover inventory requires at least four validators and at least one RPC.
  Two or three validators are refused because they match neither supported
  purpose.
- `role` is the only functional field. `validator` = registered on-chain,
  carries stake, swappable. `rpc` = never registered, no BLS signer key
  (runs `--staking-ephemeral-signer-enabled`), pinned identity, serves
  ingress and anchors L1 access. `pchain` = exactly one unregistered P-chain
  follow-only node with a stable TLS identity and no BLS signer.
- `dc` is an optional freeform display/selector tag. If omitted, it remains
  visibly unset. Maintenance verbs accept `dc=<tag>` selectors and `status`
  groups by it. Nothing functional depends on it.
- Weights are **not** inventory. On-chain weight is the sole truth; `status`
  reads it from the P-chain.
- `deployment/public.json` is generated from the private identities and is the
  public NodeID, PoP, and initial-weight handover. Validation IDs, current
  weights, and active state come from the P-chain.
- Several logical nodes, including the P-chain node, may share one host and
  IP. Nodes on that host are ordered by node number. The first uses HTTP 9650
  and staking 9651, the second 9652/9653, the third 9654/9655, and so on.
  Every node has its own data directory, logs, configuration, identity, and
  systemd unit. The intended failover shape remains one blockchain node per
  machine, permanently. Identities move; nodes do not.

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
fleet deploy <frozen|follow>
fleet pchain archive
fleet pchain freeze
fleet pchain follow
fleet start [<node>|dc=<tag> ...]
fleet stop [<node>|dc=<tag> ...]
fleet destroy [<node>|dc=<tag> ...]
fleet status [<node>|dc=<tag> ...]
fleet place <identity-letter> <node>
fleet apply-placement
```

### keygen and create

Run `keygen` on the client's secure machine:

```bash
go run ./cmd/l1 keygen
# go run ./cmd/l1 keygen 4
```

It reads only `nodes.ini`, requires `deployment/` to be absent, and generates
fresh TLS+BLS identities for validators, TLS-only identities for RPCs and the
P-chain node,
TLS+BLS identities for managers, and `genesis-funds.key`. It writes
`deployment/public.json` with the Genesis EVM address and every public identity,
NodeID, initial weight, and required PoP, plus `deployment/placement.json`
with the initial machine-to-identity bijection. RPCs have no signer or PoP
because they are never registered. The P-chain node also has no signer or PoP. Up to
the first three validators by ascending node number receive weight 100000,
remaining validators receive 1000, and managers receive 1000.

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
machine. `fleet deploy frozen` restores the certified P-chain archive, while
`fleet deploy follow` obtains the conversion results from the public network.
Both wait until the P-chain node sees both results before starting the L1 nodes.

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

### deploy, start, stop, and status

Mutating fleet commands reconcile explicit intent. They never assume a previous
invocation reached its final step. Control-side state is written atomically
before remote work, and rerunning the same command repeats all required pushes
until the remote fleet converges. There is no cordon file or hidden node state.
Systemd is the source of up/down intent: start enables and starts a unit; stop
disables and stops it.

All managed machines remain reachable from control. Node and DC loss drills
stop or kill AvalancheGo processes on those reachable machines. A genuinely
unreachable machine fails the command and is outside the benchmark scope.

`fleet deploy` always targets the complete inventory. Its one required argument
chooses the P-chain node's initial source: `frozen` or `follow`. There is no
default. Deployment is not a node-maintenance interface.
For `start`, `stop`, `destroy`, and `status`, a selector is a node number or
`dc=<tag>`; multiple selectors form a union, and no selector targets every node.

`fleet deploy` first deploys the sole P-chain node through strict stop,
package, systemd-unit, identity, optional archive restore, and start phases.
Both modes set `--p-chain-follow-only=true`. `follow` omits bootstrap fields so
AvalancheGo uses its embedded network peers. `frozen` explicitly sets both
bootstrap lists empty. A frozen deploy requires `pchain.tar.gz` in the working
directory. The archive must contain one non-empty top-level `db/` directory.
Deploy validates it before any remote mutation. With the P-chain service
stopped, deploy restores it only when the remote database is empty. It extracts
and validates `db/` in a temporary sibling location, then atomically renames it
into place. Rerunning discards interrupted staging. Once a nonempty database
exists, it is authoritative and preserved without transferring the archive.
Before any L1 service is touched, deploy queries the P-chain node and requires
the complete management and main validator sets recorded by `public.json` and
`network.env`.

A half-synced existing database is still preserved and frozen, then fails the
validator-set acceptance gate before any L1 node is touched. Resume it with
`fleet pchain follow`, then wait for `fleet status` to report readiness.
Alternatively, stop its service and deliberately remove the remote P-chain
database before rerunning `fleet deploy frozen` to seed it from the archive.
There is no reset command or local completion marker.

It then runs fleet-wide phases for every validator and RPC node: stop every
service, rsync the current binaries and rendered configuration, install and
enable every systemd unit, push each node's initial generated identity, start
every service, then wait for all nodes to serve the L1. Every validator and RPC
uses the P-chain node's inventory host and generated NodeID as its sole P-chain
bootstrap, while its state-sync list contains the other validator and RPC nodes
only. Phasing lets all members of a cold fleet start before the readiness wait
requires quorum. Deploy includes start. There is no provisioned check or local
deploy-state flag.

When logical nodes share a host, node number order assigns HTTP/staking ports
`9650/9651`, then `9652/9653`, and so on. Each logical node has a separate
systemd unit, data directory, log directory, configuration directory, and
identity directory.

`fleet start` uses the same fleet-wide pattern: stop all selected services,
wait for all to become inactive, re-push every assigned identity, start all,
then wait for all to serve the L1. This is intentional convergence, not an
optimization opportunity: stale keys on a machine must never win over
control's placement state. Every multi-phase fleet command aborts before its
next phase if any node fails.

`fleet stop` disables and gracefully stops the selected services, waits for
inactivity, and preserves their databases, logs, installed files, and current
keys. `fleet status` is read-only and reports the node number, DC, role,
assigned identity, NodeID, systemd intent and runtime state, L1 serving state,
and accepted height.

P-chain status remains available when no validator or RPC has been deployed.
It reports:

```text
P-CHAIN  MODE    LOCAL HEIGHT  UPSTREAM HEIGHT  LAG  L1 STATE  READY TO FREEZE
13       follow  289700        289700           0    complete  yes
```

The upstream height is sampled immediately before the local height. Following
is `synced` when local height reaches that sample and `catching-up` otherwise.
Frozen mode reports `frozen`, its local height, and the upstream delta instead
of calling an intentionally frozen node unsynchronized. `READY TO FREEZE=yes`
requires both synchronization and local visibility of the complete management
and main validator sets. `fleet pchain freeze` runs the same check and refuses
to freeze when either condition is missing.

`fleet destroy` sends SIGKILL to every selected AvalancheGo process, prevents
systemd from restarting it, and verifies every selected unit is inactive
before deleting anything. If any kill or inactivity check fails, it deletes
nothing. It then deletes only `chainData/<L1-chain-id>` for the benchmark L1
on those nodes. SIGKILL is intentional: this command simulates abrupt machine
loss. Normal `fleet stop` remains graceful. The P-chain database, identity,
logs, configuration, binaries, and systemd unit remain. The next
`fleet start` rebuilds the L1 while reusing the expensive P-chain state. This
command changes only local node data. It is unrelated to `l1 destroy`, which
disables validators on the P-chain and reclaims their balances.

Every inventory node, including the P-chain node, uses systemd because it
must survive a terminal disconnect and machine restart.

### place: key-swap failover

`fleet place a 5` puts identity `a` on node 5 **and the validator identity
previously on node 5 on identity `a`'s old node**. Placement is always a
transposition, so the identity↔node bijection is preserved by construction.
The command atomically updates `deployment/placement.json`, then rewrites the
assigned identity files on every inventory node, including unchanged nodes.
It does not stop or restart any process. Rewriting the complete fleet every
time makes disk state deterministic and makes a rerun a full reconciliation,
not a delta. If identity `a` is already assigned to node 5, rerunning
`fleet place a 5` leaves the mapping unchanged but still rewrites every key.

`fleet apply-placement` reads every running node's NodeID and compares it with
the NodeID assigned by `deployment/placement.json`. It snapshots every
mismatched running node, stops that complete set, and starts that same set only
after every stop succeeds. If any stop fails, none are started. Nodes already
running their assigned identity are untouched. Nodes that were down when the
command began remain down, even if their on-disk identity differs. This
explicit command activates keys written by `place` without sounding like a
whole-fleet restart.

Two refusals, both correctness rather than policy:
1. an identity that would end up live twice (cannot be expressed anyway);
2. any swap involving an `rpc` node. RPC identities are L1 state-sync seeds
   (see below) and unstaked, so moving them breaks the mesh, and moving a
   validator onto an RPC slot silently changes its role.

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

Run weight changes while the P-chain node is following. A transaction
submitted while the P-chain node is frozen can confirm publicly without
reaching the fleet until the P-chain node follows again.

## Bootstrap topology

The P-chain node is the sole primary-network bootstrap for every validator
and RPC node. Its `(host:staking-port, NodeID)` comes from `nodes.ini` and the
generated public identity manifest. Bootstrap entries are verified by TLS at
dial time, so the P-chain node identity is stable and cannot participate in key
placement.

Each validator and RPC receives every other validator and RPC inventory node
as its L1 state-sync peers. The list excludes itself and the P-chain node.
The P-chain node does not track or serve the L1 and therefore must never appear
in a downstream state-sync list.

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

Consensus parameters are verified benchmark inputs. Every topology, including
one validator, uses the shipped `subnet-config.json` unchanged: `k=30`,
`alphaPreference=16`, `alphaConfidence=17`, `beta=12`, and a 100ms proposer
window. Sampling is with replacement. Fleet commands never derive or rewrite
consensus settings from inventory.

There is no scores file. The deliverable is the drill itself plus the live
dashboards: run `bombard`, run a drill, watch throughput, finalized height per
node, and stake placement move in Grafana (`04_monitoring.sh` runs
Prometheus + Grafana on control, scraping every node's `/ext/metrics`).

### P-chain node modes

Initial deployment is explicit:

```bash
fleet deploy frozen
fleet deploy follow
```

`frozen` requires `pchain.tar.gz`, restores it when the P-chain node has no
database, and starts with explicit-empty bootstrap lists. Produce the archive
from any deployed, running P-chain node:

```bash
fleet pchain archive
```

The command refuses to overwrite `./pchain.tar.gz`, stops the managed P-chain
service, creates a consistent archive of its non-empty `db/`, restarts the
service in its existing mode, downloads and validates the archive, and
atomically publishes it locally. The service is restarted before the large
download begins. If remote archive creation fails, restart is still attempted.

`follow` requires no archive. It preserves any existing database and omits
bootstrap fields so AvalancheGo follows its embedded network peers. The
systemd service and numbered data directory preserve the P-chain node identity,
mode, and P-chain state.

The following-mode lifecycle command is implemented:

```bash
fleet pchain follow
```

`fleet pchain follow` is also the first-run initializer. It stops the P-chain
service if present, reconciles its AvalancheGo package, systemd unit, stable
identity, and following configuration, enables and starts it, verifies the
service is running, then returns. It never starts or changes a validator or RPC
node. Rerunning it repeats the full P-chain-node reconciliation and preserves
the existing database. Use `fleet status` to observe catch-up; the future
`fleet pchain freeze` command owns the readiness check.

The frozen-mode lifecycle command is designed but not implemented yet:

```bash
fleet pchain freeze
```

`fleet pchain freeze` will render empty upstream bootstrap lists and restart
only the P-chain node. Downstream configurations always point to the same
P-chain node and never change. A machine reboot retains the last rendered
P-chain configuration and therefore retains its mode.

Starting frozen from an empty fleet is therefore:

```bash
fleet pchain follow
fleet pchain archive
fleet deploy frozen
```

The first command obtains the newly created chain state without starting the
L1 fleet. `archive` restarts the P-chain node in following mode after producing
the artifact. `deploy frozen` then freezes it before starting any validator or
RPC. Do not substitute `fleet deploy follow`: that starts the complete L1 fleet
in following mode before the frozen archive exists.

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
# choose exactly one initial P-chain source:
./bin/fleet deploy follow
./bin/fleet pchain archive  # optional: produce pchain.tar.gz for frozen testing
# or place pchain.tar.gz here and run: ./bin/fleet deploy frozen
```

To start frozen when this control directory has no P-chain archive yet, use the
three-step flow in the P-chain modes section instead of `fleet deploy follow`.

The deployed P-chain state lives in the P-chain node's numbered data directory.
`fleet deploy` preserves that directory across package updates.

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
