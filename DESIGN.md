# Design: Avalanche for isolated networks

Finalized 2026-07-22 in a long design dialogue. This file records DECISIONS AND THEIR MOTIVATIONS. Motivations matter more than the decisions themselves: code is cheap to regenerate, the reasoning is not. When changing a decision, first check its motivation still fails to apply.

## What this is

A performance-under-failover benchmark toolset for Avalanche L1s in ISOLATED networks, publishable on main, usable by anyone (never name any specific client in this public repo).

- Renamed from "Avalanche for banks" to "Avalanche for isolated networks". Motivation: isolation is the defining technical trait, banking is just the motivating example. Isolation also IMPLIES PoA: a proof-of-stake chain requires open peering with unknown validators, which an isolated network by definition forbids, so "isolated" already means "PoA, fixed membership".
- The motivating workload (kept as the narrative): a single writer issuing an ordered, linear-nonce stream of high-value transactions; the chain must survive a data-center failover with no lost or reordered transactions; we measure sustained throughput THROUGH the failover, not just steady state. The application backstop (single sender + nonce registry + replay of the last few minutes) makes the majority branch canonical by definition and heals gaps, so consensus-level failover need not be perfectly lossless.
- This release's whole point is BOTH P-chain modes in one toolset with an in-place transition. Motivation: releases already exist for the frozen mode (key-swap failover) and the live mode (weight-change failover) SEPARATELY; a third single-mode release would be redundant. The novel deliverable is the frozen-to-proxied transition.
- Deliverable form: primitives + a runbook + dashboards. NO formal per-run report artifact (decided against a scores file): the dashboards and the drills ARE the deliverable; this is a validation/demo kit. We ship what to trigger, the operator owns automation and triggers.

## P-chain node

The inventory contains exactly one `pchain` role. It is a real AvalancheGo
process with its own numbered machine slot, stable TLS identity, P-chain
database, ports, configuration, logs, and systemd unit. It is never registered
on an L1 and has no BLS signer or proof of possession.

Initial `fleet deploy <frozen|follow>` requires the operator to choose the
P-chain node's source explicitly. Both modes use follow-only mode. `follow`
omits bootstrap fields so AvalancheGo reads its built-in network peers.
`frozen` requires a local `pchain.tar.gz` containing one top-level `db/`
directory and renders explicit-empty bootstrap lists. Frozen deploy validates
the local archive before any remote mutation. With the service stopped, it
restores only when the remote database is empty: extraction and nonempty `db/`
validation happen in a temporary sibling location, followed by an atomic rename
into place. Every rerun discards interrupted staging. Once a nonempty database
exists, it is authoritative and preserved. Deploy starts the P-chain node and
does not touch any validator or RPC service until its local API contains the
complete management and main validator sets recorded by `public.json` and
`network.env`. Therefore a preserved but half-synced database remains frozen
and fails at this acceptance gate. The operator either resumes following until
the state is ready, or stops its service and deliberately removes the remote
database before rerunning frozen deploy to seed it from the archive. There is no
reset command or completion marker. Every validator and RPC then uses the
P-chain node's inventory address and generated NodeID as its sole
primary-network bootstrap.

Motivations:
- The P-chain state owner is explicit in inventory and receives the same
  systemd, identity, co-location, and deployment treatment as every other
  long-running node.
- The P-chain node's NodeID is derived from generated identity state. It is not copied
  into `.env` or another registry.
- Waiting on the P-chain node's local validator view proves that the exact
  P-chain state needed by the L1 is present before L1 nodes start.
- Initial deploy has two explicit modes because generic users may need to sync
  from the public network while isolated delivery starts from a certified
  archive. Neither is a safe universal default, so omitting the mode is an
  error. This required decision is different from optional deployment
  selectors, which were removed because they only added mental load.
- `fleet pchain follow` is both the first-run initializer and the later mode
  transition. It reconciles only the P-chain node's package, systemd unit,
  identity, and following configuration, starts it, verifies the service is
  running, and returns. It never starts or changes a validator or RPC node.
  Catch-up is observable through `fleet status`; the future `fleet pchain
  freeze` command owns the readiness gate. On an existing deployment, follow
  restores AvalancheGo's embedded upstreams and restarts only the P-chain node.
- `fleet pchain freeze` renders empty upstream bootstrap lists and restarts only
  the P-chain node. Downstream configurations never change. Reboots retain the
  last rendered mode. These transitions support the frozen-to-following
  benchmark and later P-chain state visibility, including ICM-related state.
- `fleet pchain archive` is the only archive producer. It requires the managed
  P-chain service to be running, stops it, creates a consistent archive of its
  `db/`, restarts it in the unchanged mode before downloading, validates the
  download, and atomically publishes `./pchain.tar.gz`. It refuses to overwrite
  an existing local archive. Owning the stop and restart avoids unsafe copies
  from arbitrary running database directories.
- Starting frozen from an empty fleet is deliberately three explicit steps:
  `fleet pchain follow`, `fleet pchain archive`, then `fleet deploy frozen`.
  The first step obtains the newly created chain state without starting the L1
  fleet, the second produces the portable artifact, and the final step freezes
  the P-chain node before any validator or RPC starts. `fleet deploy follow` is
  not a substitute because it starts the complete L1 fleet in following mode.

## Creation

Two explicit steps split private-key generation from P-chain access:

1. `l1 keygen [1|4]` runs on the client's secure machine. The optional argument is the management committee size and defaults to 1. It reads only `nodes.ini`, generates all private identities plus a separate Genesis-funds key, and writes their public chain-creation inputs to `deployment/public.json`.
2. `l1 create` runs on any designated creation machine with P-chain access. It reads `.env` plus `deployment/public.json`, never any generated private key. It creates BOTH L1s at genesis:

1. Management L1 (the committee): own subnet + management chain (never deployed, never runs blocks) + ConvertSubnetToL1Tx registering an equal-weight signing committee (`deployment/manager/<letter>`, BLS keys generated and held on control). It is self-managed through its own management chain at manager address `0x..01`. Committee size is exactly 1 or 4 (the shipped example selects 1), member weight 1000. One is the minimum, simplest signing authority. Four provides one-signer-loss tolerance with a 3-of-4 quorum. Sizes 2 and 3 add keys without providing that tolerance.
2. Main L1: own subnet + chain + ConvertSubnetToL1Tx registering the fleet's validators, with the recorded validator manager set to the management L1's chain (address 0x..01). The P-chain verifies this L1's weight changes against the committee's set.

Decisions and motivations:
- Committee ALWAYS, even for a frozen-mode run that never changes weights. Motivation: going proxied must never register anything new on-chain; the committee sits dormant through the frozen mode and becomes the weight authority once proxied. A bank can afford the extra validator.
- Making the main L1 self-managed was DROPPED. Motivation: in a DC failure the high-stake nodes are down; if they are also the signing authority you cannot sign the weight reassignment that recovers from their loss. The separate management L1 is self-managed through its own management chain, while its signing authority remains independent of the fleet that fails.
- Signing is ALWAYS key-only and off-node: the tool holds BLS keys and signs locally, never asks a node to sign. Same motivation: nodes may be dead exactly when signing is needed.
- C-Chain PoA validator-manager contract DROPPED from this toolset. Motivation: the benchmark measures consensus under load, not a Solidity manager; it is client-specific surface. If a client needs it, it is an adapter later (YAGNI).
- Initial weights are baked DIRECTLY into `public.json` by `keygen`: up to the first 3 validators by ascending node number receive 100000, all remaining validators receive 1000, and managers receive 1000. A one-validator chain therefore starts with its only validator active. `create` copies those values verbatim into the conversion transactions. Motivation: one generated handover artifact completely defines the validator sets, and the creation machine cannot drift by independently interpreting `nodes.ini`.
- Weight-change certification at create DROPPED (the old flow registered everyone at 1 and raised 1,2,3 via the committee purely to prove the path). Motivation: the proxied mode exercises weight changing for real; a create-time proof is redundant.
- `keygen` always generates every validator, RPC, and manager identity fresh. It never loads or reuses an existing identity and requires `deployment/` to be absent. A failed partial creation is abandoned; `destroy` reclaims any converted balances and removes its output. No resume path.
- `keygen` reads `nodes.ini` only. It never reads `.env` and requires no network access. Committee size is a `keygen` argument, not environment state: omit it for 1 or pass 4 for one-signer-loss tolerance.
- `create` reads `.env` explicitly. `NETWORK` is `fuji` or `mainnet`; Fuji is always called Fuji, never "testnet". `PCHAIN_API` is required and explicit. `FUNDING_PRIVATE_KEY` contains the raw 32-byte secp256k1 key that pays for and owns P-chain creation and validator-balance transactions.
- The Genesis funds use a different secp256k1 key generated at `deployment/genesis-funds.key`. Only its EVM address appears in `public.json` and Genesis. Motivation: the P-chain funding key stays with the creation operator, while the client retains the benchmark transaction funds.
- `l1 keygen-funding` writes `FUNDING_PRIVATE_KEY` into an empty field in `.env`. It is valid only before `deployment/network.env` exists. Replacing the funding identity after creation would disconnect local configuration from the on-chain owners, so it fails before mutating `.env` whenever deployment state exists.
- `public.json` contains the Genesis EVM address and, for every numbered node, its identity letter, role, NodeID, initial weight, and BLS proof of possession when registered. It also contains every manager identity, NodeID, weight, and proof. PoPs are public. RPCs have no BLS signer or PoP because they are never registered.
- `keygen` and `create` both print the SHA-256 of `public.json`. Before its first transaction, `create` verifies every PoP and prints the Genesis address and complete registered validator roster. The digest is the explicit integrity check across the handover.
- `create` requires `deployment/public.json` and requires `deployment/network.env` to be absent. It does not read `nodes.ini`, `staker.key`, `signer.key`, or `genesis-funds.key`.
- Fail-fast is a feature. Commands validate all required configuration and prior-step artifacts before performing work. A failure names the missing field, file, or prerequisite step. The tool never guesses paths, falls back to legacy variables, auto-discovers omitted configuration, silently repairs state, or continues with partial input. A command may generate only the artifacts that its documented step owns, and it reports each generated path and on-chain transaction.
- Every registered main and committee validator starts with a 0.1 AVAX continuous-fee balance. `l1 topup <days>` reads the current P-chain fee rate and raises every registered balance to at least that many days of runway. Validators already above the target are left unchanged.
- `l1 destroy` accepts complete or partial creation state, disables every converted main validator first and management validator last, and reclaims whichever balances exist. It verifies the funding key controls deactivation and receives remaining balances before submitting anything. If any operation fails, `deployment/` remains intact for an explicit rerun. Only after every balance is reclaimed does `destroy` remove `deployment/`, including its obsolete private keys and transaction state. An already-destroyed leftover directory is removed too. Creation never resumes. There is no local destroyed flag: height-consistent P-Chain state determines whether more balances remain, while presence of `deployment/` determines whether local creation state exists. Other lifecycle commands remain strict and fail on incomplete creation. `l1 address` remains available before creation because an imported key must be funded first.
- Creation is not freezing. A chain may be pre-created anywhere with P-chain access. The inventory P-chain node can receive a certified archive containing both conversions or follow the public network beyond them.
- The implementation deliberately serializes `public.json` during `keygen` and reloads it during `create`, even when both commands run on one machine. This continuously tests the exact public-only handover used when the two commands run in different trust domains.

## Inventory and naming

ini-style inventory; the shape is the user's, we impose almost nothing ("freestyle"). We deliberately do NOT provide a "do good" binary that decides swaps or weights for the user: primitives, not policy.

- Machines and nodes are NUMBERS. Node identities are immutable lowercase letters (`a`, `b`, ...), stored under `deployment/identities/<letter>`. Manager identities use a separate lowercase-letter namespace under `deployment/manager/<letter>`. Motivation: key swapping inherently makes identity-to-machine placement dynamic, so using numbers for both makes identity `1` and machine `1` dangerously ambiguous as soon as they diverge. Different namespaces expose the distinction instead of pretending the mapping does not exist.
- `keygen` writes the initial machine-to-identity bijection to `deployment/placement.json`. It is generated state, never user-authored. `deploy` and `start` read it to decide which key belongs on each node; `place` is the only command that changes it. Motivation: a key swap makes placement dynamic, so control needs one explicit source of truth. Inferring desired placement from running processes would make stopped nodes ambiguous and turn remote drift into control-side intent.
- role is a property of the NODE: validator | rpc | pchain. The ONE functional field. Exactly one P-chain node is required.
- Validators are registered in ascending node-number order. Up to the first three in that order receive weight 100000; all remaining validators receive weight 1000.
- Two inventory shapes are valid. A one-validator development deployment may contain exactly one validator and no RPC. A failover deployment requires at least four validators and at least one RPC. Counts of two or three validators are refused because they are neither the minimal single-node setup nor the benchmark's three-active-plus-spare failover shape.
- `keygen` freshly generates TLS and BLS staking keys for validators, stable TLS identities without BLS signer keys for RPC and P-chain nodes, and TLS+BLS identities for the manager committee. No identity is reused between generation attempts.
- Many logical nodes, including the P-chain node, may share one physical host and IP. Nodes on the same host are ordered by node number. The first uses HTTP 9650 and staking 9651; each additional node adds 2 to both ports (9652/9653, 9654/9655, and so on). Every co-located node has its own data directory, logs, configuration, identity, and systemd unit. The normal failover topology still defaults to one blockchain node per physical machine, permanently, even under key swaps.
- dc= is an optional freeform tag. If omitted, it remains visibly unset; the tool does not invent one. Display and selector ONLY (`fleet status` grouping and selectors such as `dc=A`). Nothing functional may ever depend on it.
- Weights are NOT inventory: on-chain weight is the sole truth.
- `deployment/public.json` is the generated public handover and identity registry. It is derived once from the private keys, never user-authored. Before creation it supplies NodeIDs, PoPs, and initial weights. After creation, live weight and active state still come only from the P-chain.
- The public package contains no private keys or generated deployment secrets. The populated `.env`, committee keys, validator staking keys, and generated deployment state are transferred separately as a private handover bundle.
- The committee is not in the inventory; `keygen` generates it separately. It never runs, but must stay funded (a drained committee validator dilutes the quorum).

## The two primitives

- `fleet place <identity> <node>`: set a lettered validator identity's location on a numbered node, for example `fleet place a 5`. It is a SWAP, not a drop: whatever validator identity currently sits on the target node rides back to the moved identity's old node. Motivation: we always have exactly as many validator identities as validator nodes; a swap preserves that bijection automatically (nothing orphaned, nothing duplicated), which makes equivocation STRUCTURALLY inexpressible rather than merely checked. It atomically updates `deployment/placement.json`, then rewrites the identity files on EVERY inventory node from that complete mapping. It does not stop or restart anything. Rewriting the whole fleet, including unchanged assignments, makes disk state deterministic and makes rerunning the command a complete reconciliation rather than a delta. If the requested identity is already on the requested node, the mapping remains unchanged but every key is still rewritten.
- `fleet apply-placement`: compare every RUNNING node's runtime NodeID with the NodeID assigned by `deployment/placement.json`. Snapshot the mismatched running set first, stop all of it, and start that same set only after every stop succeeds. A failure in any phase aborts before the next phase. Nodes whose runtime identity already matches are untouched. Nodes that were stopped before the command remain stopped even when their on-disk identity differs. This is the explicit step that activates a placement change without pretending to restart the fleet.
- `set-weight <identity> <weight>`: set a validator's on-chain weight via a committee-signed SetL1ValidatorWeightTx. Location-agnostic. It accepts exactly `1` (dead), `1000` (spare), or `100000` (active). Weight `1` is the minimum and preserves fixed membership; `0` and every intermediate value are refused.
- Before constructing or submitting a weight transaction, `set-weight` derives the management conversion block height from the already-recorded conversion transaction ID and requires the P-chain height pinned by `proposervm.getCurrentEpoch` to be at least that conversion height. The derived height is never stored. This is the exact ACP-181 visibility gate: current-height reads can show the committee while Warp admission still uses an older epoch snapshot. If the epoch is not yet sealable, the command prints the exact JST boundary and sleeps until it. It then visibly submits and prints a no-op P-chain `BaseTx`, rechecks the epoch, and submits one second visible no-op only when the first block could not advance the epoch. The command constructs and submits the weight transaction exactly once after the gate succeeds; a weight-transaction failure remains an immediate error.

Exactly TWO guards, both correctness, not policy:
1. No identity live on two nodes (equivocation forks/slashes). Enforced structurally by swap-shaped placement plus the stop-all-before-start-any apply-placement barrier.
2. No validator<->RPC swap (see RPC section). Everything else is the operator's freedom.

Failover strategies are compositions the OPERATOR makes: key-swap failover = `place` a dead high identity onto a live node (frozen-mode compatible, no P-chain writes). Weight-change failover = `set-weight` a spare to active and the failed validator to dead without moving identities (proxied mode only; lower disruption, no wipe/resync). Deliberately NOT merged into one clever reconcile abstraction: two small code paths beat one clever one at 3am. The two strategies cannot corrupt each other because weight attaches to the identity, not the node, and place preserves the bijection.

## RPC nodes

An RPC node is a pinned identity that tracks the chain and serves ingress.

- Stable identities remain required because RPC nodes are fixed ingress endpoints. Validators can swap freely because their machine placement is not an external endpoint.
- Zero stake, never registered on-chain, and no BLS signer key at all (run with an ephemeral signer): one less secret to manage, and `set-weight` cannot even express them.
- `place` refuses them (guard 2). Swapping an RPC identity would move an ingress endpoint and swapping a validator identity onto an RPC node would change its role.
- Why RPCs exist as a separate role at all: serving tx load on a validator measurably slows its block production, so ingress is kept off validators. RPCs are bombard's ingress and are never promoted.
- The one-validator development shape is the deliberate exception. Its validator also serves RPC and points at the P-chain node for primary-network bootstrap. It has no failover or independent L1 state-sync source. `stop` and `start` preserve its L1 data; losing that data requires restoring a backup or recreating the development chain.

## Bootstrap topology

- Every validator and RPC uses the sole P-chain node's `(host:staking-port, NodeID)` only as its explicit primary-network bootstrap.
- L1 state-sync peers are every other validator and RPC inventory node, excluding the node itself and the P-chain node. The P-chain node never tracks the L1 and therefore must never appear in downstream state-sync lists.
- The P-chain node follows the public P-chain without running consensus. It does not track or serve the benchmark L1.
- The P-chain node identity is pinned. It cannot participate in validator placement.

## The oracle L1 (added 2026-07-23)

An optional third L1 that accepts mocked price feeds, aggregates them in a
genesis-baked contract, and exports each update to the main L1 as a Warp
message. Opt-in through the inventory: `oracle-validator` / `oracle-rpc`
roles. A `nodes.ini` without them produces exactly the two-L1 deployment
above — no oracle chain, no feeder key, no new files.

- Managed by the SAME committee as the main L1 (its `ConvertSubnetToL1Tx`
  records the management chain at `0x..01`). Motivation: one signing
  authority; a second committee adds keys without adding loss tolerance, and
  the committee exists precisely to outlive fleet failures.
- Creation order is committee → oracle → main. Motivation: the main chain's
  receiver contract only trusts Warp messages whose source chain ID is the
  oracle chain, and that ID exists only after the oracle `CreateChainTx` is
  accepted. Baking it into the main Genesis removes a post-deploy
  configuration step and the drift class that comes with it.
- Both contracts are DEPLOYED bytecode in Genesis allocs at fixed addresses
  (aggregator `0x…FEED` on the oracle chain, receiver `0x…FeedED` on main),
  written without constructors or immutables so configuration is explicit
  storage slots (slot 0 feeder / source chain ID, slot 1 aggregator address).
  Motivation: the creation machine needs no solc; every configured value is
  visible in `genesis*.json` and recorded in `network.env`. Embedded runtime
  hex lives in `internal/oraclecontracts/`; sources in `contracts/`.
- One feeder key (`deployment/oracle-feeder.key`), funded on BOTH chains at
  Genesis. It signs feed transactions on the oracle chain and delivery
  transactions on main. Motivation: a single, purpose-named demo key;
  deliberately not the Genesis funds key so benchmark funds stay separate.
- The relayer runs on control and signs each Warp message with ALL oracle
  validator BLS keys, which control already holds. Same motivation as
  `set-weight`: signing is key-only and off-node, and the demo must run in an
  isolated network with zero additional binaries. In production this job
  belongs to icm-relayer; the control-host relayer is the airgap-friendly
  demo equivalent, not a replacement.
- Oracle validators all get weight 1000 (flat). Motivation: oracle failover
  is not the benchmark's subject, and equal weights keep the Warp quorum
  arithmetic trivial (all keys held → always 100%).
- Own consensus parameters (`subnet-config-oracle.json`, k=4/alpha=3 for the
  shipped 4-validator example) and own Genesis template
  (`oracle-genesis-template.json`, 10ms initial min delay, small gas limit).
  Motivation: the chain carries tiny price transactions and is tuned for
  latency; the k<=validator-count rule applies per L1.

## Archive nodes (added 2026-07-23)

- New `archive` role: main-L1 nodes with pruning AND state-sync disabled
  (`chain-config-archive.json`), pinned identities like RPCs, never
  registered. Motivation: historical queries need full state; the benchmark
  RPCs deliberately became light state-sync nodes (2026-07-01) and cannot
  serve them.
- Archives must exist from Genesis. Motivation: an archive cannot state-sync
  by definition; joining later means re-executing the whole chain, which is
  the same restore failure class that broke recovery in June 2026. Present
  from block 0, they never need to.
- 0 or at least 2, enforced at inventory load. Motivation: a single archive
  is an unverifiable single point of failure for history — while it
  re-executes after a loss there is no replica to serve or cross-check.

## Oracle live-run findings (proven on Fuji, 2026-07-24)

Full end-to-end validated on a 4-box/9-node Fuji fleet: feed → aggregator →
Warp → control-host signing → receiver state on main, ~0.5s relay latency at
2 updates/s per asset. Facts that only the live run could produce:

- subnet-evm COERCES a zero network-upgrade timestamp back to the network's
  default ("0 is treated as nil", params/extras SetDefaults) to prevent
  premature activations. Therefore `durangoTimestamp: 0` cannot legalize a
  warp precompile at timestamp 0; `warpConfig.blockTimestamp` must instead be
  a real post-Durango time. The templates use 1709740800 (mainnet Durango,
  past on Fuji and mainnet, so one template serves both).
- Fuji/mainnet nodes silently refuse to DIAL RFC1918 addresses:
  `--network-allow-private-ips=true` is required for private-IP bootstrap
  entries, or the node logs only a generic beacon-connect failure. This is
  the flip side of the public-IP SG trap already recorded above.
- A freshly created, idle main chain never rolls its ACP-181 epoch (epochs
  advance only with blocks), so the FIRST relay after creation stalls at the
  visibility gate until any transaction mints a block. One nudge suffices.
- The public Fuji API cannot back the relay: `platform.getValidatorsAt` at
  historical heights is rejected and the endpoint rate-limits hard. Point
  `PCHAIN_API` at a synced fleet RPC node once one exists. The P-chain
  source cannot back it either: follow-only mode never reports bootstrapped
  and rejects platform API calls (why fleet height derivation reads runtime
  state).
- The receiver's staleness check compares `updatedAt` (oracle block
  timestamp, SECOND resolution), so feeds faster than 1/s per asset produce
  equal-freshness messages. The relay's freshness gate skips them instead of
  burning guaranteed on-chain reverts; second resolution is thus the
  freshness floor. Sub-second freshness would need a monotonic sequence
  number in the payload instead of block.timestamp — a contract change, so a
  chain recreation. (Done 2026-07-25: payloads carry a per-asset seq.)
- `initialMinDelayMS` seeds the ACP-226 min block delay ONLY when Granite is
  active at the genesis block's own timestamp — and a zero `graniteTimestamp`
  is coerced to the network's real activation date, so a zero genesis
  `timestamp` (1970) is pre-Granite, the seed is skipped, and the chain
  starts at the hardcoded ~2000ms `InitialDelayExcess`, converging toward
  `min-delay-target` at 200 excess units (~0.02%) per block: ~23k blocks to
  reach 25ms. This masqueraded as a fixed ~2s block cadence immune to every
  fee/delay setting and to 585 TPS of load (blocks packed instead of
  speeding up). Fix: genesis `timestamp` set to a fixed post-Granite instant
  (2026-07-01). Proven live: with the seed applied, bombard mined 1000 TPS at
  91–117ms tx p50 on the same fleet, and long-lived chains explain why the
  earlier release saw 25ms "from genesis" — they had converged over days.
- **subnet-evm drops even-requestID AppRequests (found 2026-07-27).** The
  relay's optional p2p signing mode (`relay ... p2p=<ip:port,...>`) speaks
  ACP-118 `SignatureRequest` to validators over their staking ports using
  `peer.StartTestPeer` — no icm-services dependency, no peer discovery
  (the inventory is static). First attempt answered exactly every other
  request: subnet-evm partitions the inbound AppRequest requestID space by
  parity — even IDs route to its legacy sync-handler network, which silently
  drops unrecognized payloads (no AppError, nothing logged at info); odd IDs
  reach the SDK router that owns the ACP-118 handler. Probed empirically:
  even IDs time out, odd IDs answer in <1ms, at any send rate. The p2p signer
  therefore issues odd requestIDs only.

## Deployment simplifications (decided 2026-07-22)

- Every mutating fleet command is a reconciliation to an explicit end state,
  not a sequence that assumes its previous invocation finished. It writes
  control-side intent atomically before remote work, revalidates remote state,
  and repeats idempotent pushes on every rerun. A process may have stopped,
  failed, or timed out after any individual action; rerunning the same command
  must converge rather than invert or duplicate the previous partial work.
- There is no cordon registry or hidden node state. Systemd is the process
  manager and the only up/down intent: start enables and starts a unit; stop
  disables and stops it. Commands that apply identity placement inspect the
  running set at entry and never start a node that was already down.
- All managed machines remain reachable from control. Simulated node and DC
  loss stops or kills AvalancheGo processes on reachable machines. A genuinely
  unreachable machine fails the command and is outside this benchmark's
  operating model.
- `fleet deploy <frozen|follow>` is software deployment, not machine provisioning. Its required argument selects only the initial P-chain node source; it takes no node selectors and always deploys the complete inventory. It deploys the sole P-chain node first through stop, package, systemd, identity, optional archive restore, and start barriers, then waits for its local P-chain API to contain both converted validator sets. Only then does it run the same barriers for every validator and RPC node, followed by one readiness barrier after all services have started. Deploy includes start. There is no separate provisioned check or deploy-state flag.
- Logical nodes sharing a host are ordered by node number and receive HTTP/staking ports `9650/9651`, then `9652/9653`, and so on. Every logical node has separate data, logs, configuration, identity, and systemd unit paths.
- `fleet start [selectors...]` uses the same selector rules and fleet-wide phases: stop all selected services, wait for all to become inactive, push every currently assigned identity again, start all, then wait for all to serve the L1. A failure in any phase aborts before the next phase. Re-pushing on every start is intentional: local placement is authoritative, and a stale remote key must never survive a restart.
- `fleet stop [selectors...]` disables and gracefully stops the selected systemd services and waits for inactivity. It preserves every database, log, installed artifact, and current remote key.
- `fleet destroy [selectors...]` uses the same selectors and is deliberately local, unlike `l1 destroy`. It sends SIGKILL to every selected AvalancheGo process, prevents systemd from restarting it, and requires every selected unit to be inactive before deleting any data. If any kill or inactivity check fails, it deletes nothing. It then deletes only that L1's `chainData/<L1-chain-id>` on the selected nodes. It preserves the complete P-chain database, identities, logs, configuration, binaries, and systemd units so the next `fleet start` can rebuild the L1 without repeating the expensive P-chain sync. SIGKILL is intentional: this command simulates an abrupt machine loss. Normal `fleet stop` remains graceful.
- `fleet status [selectors...]` is read-only and works when only the P-chain node has been initialized. It reports node number, DC, role, assigned identity, NodeID, systemd intent and runtime state, L1 serving state, and accepted height. With no selector it reports the entire inventory, grouped by DC when `dc` is present.
- P-chain status reports mode, local height, an upstream height sampled immediately before the local read, block lag, visibility of both converted validator sets, and `READY TO FREEZE yes|no`. Following is `synced` when local height is at least the sampled upstream height and `catching-up` otherwise. Frozen state is reported as `frozen` with its local height and upstream delta, never mislabeled unsynchronized. Ready-to-freeze requires both synchronization and both local validator sets. `fleet pchain freeze` runs the same gate and fails with the exact heights or missing set instead of freezing an unknown state.
- Every inventory node process, including the P-chain node, is a systemd service because it must survive the control terminal and machine restarts.
- DELETE the provisioned() existence check; always rsync everything during deploy. Motivation: the check treated nodes with STALE keys as provisioned, booting them with wrong NodeIDs (bit us live 2026-07-20); rsync is idempotent and near-instant when unchanged. Deleting the mechanism removes the whole drift-bug class; fixing it would just shrink it.
- NO key pre-staging on nodes. Control holds all keys and pushes only the identity selected for that node during `deploy`, `start`, or `place` (~1KB over ssh). Motivation: one less deploy step, no staging drift, and a compromised box leaks ONE identity instead of every identity.
- Keep binaries always rebuilt fresh before every ship (user rule). Aggressive fast iteration: recreate L1s/committees/chains freely, never nurse half-broken on-chain state; the ONE thing preserved rather than redone is the P-chain sync (the expensive part on an isolated fleet).

## Consensus and recovery facts (validated live)

- Wiping only the L1 chain data while keeping the P-chain DB was proven to state-sync a node fresh onto the majority branch without a re-fork (2026-07-18, Fuji, AvalancheGo 4265498). The June-2026 belief that recovery must delete all validator data is disproven. `fleet destroy` exposes exactly this operation. It is deliberately not hidden inside `stop` or `start`: both preserve data.
- Consensus parameters are verified benchmark inputs, not inventory-derived settings. Every topology, including one validator, uses the shipped `subnet-config.json` unchanged: k 30, alpha preference 16, alpha confidence 17, beta 12, and proposer window 100ms. Fleet tooling must never calculate, rewrite, or otherwise adjust consensus parameters from the node count. Sampling is with replacement; reducing k for a small roster is incorrect and produces forks.
- Isolated-fleet peering needs SG ingress AND egress for the staking port from/to both the intra-region private CIDR and the fleet public IPs (cross-region uses public IPs, same-VPC traffic arrives from private IPs, so public-IP-only rules silently fail intra-region).

## Constraints (standing)

- Never name any specific client in git (public repo). STT note: "C-chain" in dictation almost always means P-chain.
- Never print or commit private keys, funded keys, or API tokens.
- The populated `.env`, committee keys, validator staking keys, and generated deployment state are private handover artifacts: back them up, transfer them separately from the public package, and keep every registered validator funded.
