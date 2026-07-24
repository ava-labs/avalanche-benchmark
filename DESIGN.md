# Design: Avalanche for isolated networks

Finalized 2026-07-22 in a long design dialogue. This file records DECISIONS AND THEIR MOTIVATIONS. Motivations matter more than the decisions themselves: code is cheap to regenerate, the reasoning is not. When changing a decision, first check its motivation still fails to apply.

## What this is

A performance-under-failover benchmark toolset for Avalanche L1s in ISOLATED networks, publishable on main, usable by anyone (never name any specific client in this public repo).

- Renamed from "Avalanche for banks" to "Avalanche for isolated networks". Motivation: isolation is the defining technical trait, banking is just the motivating example. Isolation also IMPLIES PoA: a proof-of-stake chain requires open peering with unknown validators, which an isolated network by definition forbids, so "isolated" already means "PoA, fixed membership".
- The motivating workload (kept as the narrative): a single writer issuing an ordered, linear-nonce stream of high-value transactions; the chain must survive a data-center failover with no lost or reordered transactions; we measure sustained throughput THROUGH the failover, not just steady state. The application backstop (single sender + nonce registry + replay of the last few minutes) makes the majority branch canonical by definition and heals gaps, so consensus-level failover need not be perfectly lossless.
- This release's whole point is BOTH P-chain modes in one toolset with an in-place transition. Motivation: releases already exist for the frozen mode (key-swap failover) and the live mode (weight-change failover) SEPARATELY; a third single-mode release would be redundant. The novel deliverable is the frozen-to-proxied transition.
- Deliverable form: primitives + a runbook + dashboards. NO formal per-run report artifact (decided against a scores file): the dashboards and the drills ARE the deliverable; this is a validation/demo kit. We ship what to trigger, the operator owns automation and triggers.

## The two P-chain modes and the source (the central idea)

There is no separate deployment-mode variable or lifecycle manager. One P-chain source process runs in the foreground on control, and its required start argument selects the bootstrap configuration:

- **Frozen P-chain**: both source bootstrap lists are explicitly empty. The source serves its preserved frontier but accepts no newer P-chain blocks.
- **Following P-chain**: the source runs `--p-chain-follow-only=true` using the packaged AvalancheGo binary's built-in network bootstrappers.

Always true in both modes:
- Every start reuses the same database and NodeID.
- The source and fleet nodes run `--partial-sync-primary-network`; C and X are never synced.
- Validators point at the fleet's RPC nodes. RPC nodes point at the source's stable `(IP, NodeID)`.
- `fleet pchain start <following|frozen>` is the entire source interface. It replaces itself with AvalancheGo and remains in the foreground.

Motivations:
- The client control host already has internet. Therefore initial snapshot shipment, archive import, USB tooling, reset tooling, and a second P-chain process do not need to exist.
- Initial installation and every later refresh use the same sequence: start following until the required state is accepted, stop it, then start frozen.
- Following omits both bootstrap fields so AvalancheGo reads its embedded `genesis/bootstrappers.json`. The benchmark never copies or pins that list, so updating the packaged AvalancheGo binary also updates the defaults. Freeze must instead render both lists explicitly empty.
- The benchmark uses exactly one source on control. Its stable identity lets every downstream node keep the same bootstrap configuration through every following/frozen transition.
- `fleet` does not daemonize, supervise, restart, or report status. Foreground AvalancheGo owns logs, signals, and exit status.
- Already-running fleet nodes continue while the source is stopped. A fleet node starting during that gap cannot pass its configured bootstrap-beacon gate until the source returns, even with an existing local P-chain database.

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
- Initial weights are baked DIRECTLY into `public.json` by `keygen`: first 3 validators by ascending node number 100000, all remaining validators 1000, managers 1000. `create` copies those values verbatim into the conversion transactions. Motivation: one generated handover artifact completely defines the validator sets, and the creation machine cannot drift by independently interpreting `nodes.ini`.
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
- Creation is not freezing. A chain may be pre-created anywhere with P-chain access. The deployment control source later follows beyond both conversions and freezes that accepted state in place.
- The implementation deliberately serializes `public.json` during `keygen` and reloads it during `create`, even when both commands run on one machine. This continuously tests the exact public-only handover used when the two commands run in different trust domains.

## Inventory and naming

ini-style inventory; the shape is the user's, we impose almost nothing ("freestyle"). We deliberately do NOT provide a "do good" binary that decides swaps or weights for the user: primitives, not policy.

- Machines and nodes are NUMBERS. Node identities are immutable lowercase letters (`a`, `b`, ...), stored under `deployment/identities/<letter>`. Manager identities use a separate lowercase-letter namespace under `deployment/manager/<letter>`. Motivation: key swapping inherently makes identity-to-machine placement dynamic, so using numbers for both makes identity `1` and machine `1` dangerously ambiguous as soon as they diverge. Different namespaces expose the distinction instead of pretending the mapping does not exist. The mapping is observed from stable NodeIDs and current placement, not maintained as a second user-authored registry.
- role is a property of the NODE: validator | rpc. The ONE functional field.
- Validators are registered in ascending node-number order. The first three in that order receive weight 100000; all remaining validators receive weight 1000.
- Inventory requires at least four validators and at least one RPC. Three high validators plus at least one low validator are the minimum useful failover shape; an RPC is required for stable ingress and bootstrap anchoring.
- `keygen` freshly generates TLS and BLS staking keys for validators, stable TLS identities without BLS signer keys for RPC nodes, and TLS+BLS identities for the manager committee. No identity is reused between generation attempts.
- Many nodes may co-host on one machine, but the unified experience default is one blockchain node per physical machine, permanently, even under key swaps.
- dc= is an optional freeform tag. If omitted, it remains visibly unset; the tool does not invent one. Display and selector ONLY (fleet status grouping, batch verbs like `down dc=A` to simulate a whole-DC failure, per-DC dashboard panels are a nice-to-have). Nothing functional may ever depend on it.
- Weights are NOT inventory: on-chain weight is the sole truth.
- `deployment/public.json` is the generated public handover and identity registry. It is derived once from the private keys, never user-authored. Before creation it supplies NodeIDs, PoPs, and initial weights. After creation, live weight and active state still come only from the P-chain.
- The public package contains no private keys or generated deployment secrets. The populated `.env`, committee keys, validator staking keys, and generated deployment state are transferred separately as a private handover bundle.
- The committee is not in the inventory; `keygen` generates it separately. It never runs, but must stay funded (a drained committee validator dilutes the quorum).

## The two primitives

- `place <identity> <node>`: set a lettered validator identity's location on a numbered node, for example `place a 5`. It is a SWAP, not a drop: whatever identity currently sits on the target node rides back to the moved identity's old node. Motivation: we always have exactly as many identities as nodes; a swap preserves that bijection automatically (nothing orphaned, nothing duplicated), which makes equivocation STRUCTURALLY inexpressible rather than merely checked. Executed two-pass (stop both nodes, swap keys on disk, start both) so the two identities are never live crossed mid-move. Weight-agnostic.
- `set-weight <identity> <weight>`: set a validator's on-chain weight via a committee-signed SetL1ValidatorWeightTx. Location-agnostic. It accepts exactly `1` (dead), `1000` (spare), or `100000` (active). Weight `1` is the minimum and preserves fixed membership; `0` and every intermediate value are refused.
- Before constructing or submitting a weight transaction, `set-weight` derives the management conversion block height from the already-recorded conversion transaction ID and requires the P-chain height pinned by `proposervm.getCurrentEpoch` to be at least that conversion height. The derived height is never stored. This is the exact ACP-181 visibility gate: current-height reads can show the committee while Warp admission still uses an older epoch snapshot. If the epoch is not yet sealable, the command prints the exact JST boundary and sleeps until it. It then visibly submits and prints a no-op P-chain `BaseTx`, rechecks the epoch, and submits one second visible no-op only when the first block could not advance the epoch. The command constructs and submits the weight transaction exactly once after the gate succeeds; a weight-transaction failure remains an immediate error.

Exactly TWO guards, both correctness, not policy:
1. No identity live on two nodes (equivocation forks/slashes). Enforced structurally by swap-shaped place + two-pass execution.
2. No validator<->RPC swap (see RPC section). Everything else is the operator's freedom.

Failover strategies are compositions the OPERATOR makes: key-swap failover = `place` a dead high identity onto a live node (frozen-mode compatible, no P-chain writes). Weight-change failover = `set-weight` a spare to active and the failed validator to dead without moving identities (proxied mode only; lower disruption, no wipe/resync). Deliberately NOT merged into one clever reconcile abstraction: two small code paths beat one clever one at 3am. The two strategies cannot corrupt each other because weight attaches to the identity, not the node, and place preserves the bijection.

## RPC nodes

An RPC node is: a PINNED (IP, NodeID) that tracks the chain, serves ingress, and anchors bootstrap. Nothing to simplify further.

- Stable identities REQUIRED: bootstrap entries are (IP, NodeID) pairs verified by TLS at dial time. The mesh's rendezvous points must never change identity, or every peer's bootstrap entry for them silently rots. Validators can swap freely precisely because nothing anchors on them.
- Zero stake, never registered on-chain, and no BLS signer key at all (run with an ephemeral signer): one less secret to manage, and `set-weight` cannot even express them.
- `place` refuses them (guard 2). Swapping an RPC identity would break the anchors; swapping a validator identity onto an RPC node would silently de-anchor the mesh.
- Why RPCs exist as a separate role at all: serving tx load on a validator measurably slows its block production, so ingress is kept off validators. RPCs are bombard's ingress and are never promoted.

## Bootstrap topology (two-hop, from the mainnet-committee release)

- Validators bootstrap from the fleet's own RPC nodes.
- RPC nodes bootstrap from the P-chain source: the fleet's one controlled P-chain path.
- state-sync lists = bootstrap lists (one anchor list; two lists that always contain the same nodes is pointless).
- GOTCHA (proven live): empty bootstrap lists do NOT auto-discover peers from P-chain records; lists must be explicit. And every key swap invalidates any (IP, NodeID) bootstrap entry pointing at a swapped VALIDATOR, which is exactly why anchors are RPCs only. (The interim fleet listed all 12 nodes as bootstrap anchors; it survived swaps only because enough unswapped entries remained. Do not copy that.)

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
  chain recreation.

## Deployment simplifications (decided 2026-07-22)

- DELETE the provisioned() existence check; always rsync everything. Motivation: the check treated nodes with STALE keys as provisioned, booting them with wrong NodeIDs (bit us live 2026-07-20); rsync is idempotent and near-instant when unchanged. Deleting the mechanism removes the whole drift-bug class; fixing it would just shrink it.
- NO key pre-staging on nodes. Control holds all keys and pushes the right one at `place` time (~1KB scp). Motivation: one less deploy step, no staging drift, and a compromised box leaks ONE identity instead of eight.
- Keep binaries always rebuilt fresh before every ship (user rule). Aggressive fast iteration: recreate L1s/committees/chains freely, never nurse half-broken on-chain state; the ONE thing preserved rather than redone is the P-chain sync (the expensive part on an isolated fleet).

## Consensus and recovery facts (validated live)

- Wipe-on-down PROVEN (2026-07-18, Fuji, avalanchego 4265498): `down` wipes ONLY the L1 chain data (chainData/<L1chainID> + logs), keeping the P-chain DB; `up` state-syncs the L1 fresh (seconds-to-minutes) and reconverges on the majority branch, no re-fork. Evidence: identical block hashes across all nodes for blocks produced while a node was down+wiped and after rejoin; single branch, quorum 3/3. The June-2026 "must nuke all of data/validator" belief is DISPROVEN.
- Fork recovery for a node that never went down is the same tool: `down` + `up` on any node whose accepted hash diverges from the majority of high validators. Runbook procedure, not automated.
- subnet-config k fix: k (sample size) must be <= validator count. k=20 with 8 validators never finalized (errInsufficientWeight, blocks built but not accepted). Use k=5, alpha=4 for the flat 8-validator topology. Alpha sits just below max so losing ONE high validator (~66%) still finalizes at speed: a single high failover does not halt the chain.
- Isolated-fleet peering needs SG ingress AND egress for the staking port from/to both the intra-region private CIDR and the fleet public IPs (cross-region uses public IPs, same-VPC traffic arrives from private IPs, so public-IP-only rules silently fail intra-region).

## Constraints (standing)

- Never name any specific client in git (public repo). STT note: "C-chain" in dictation almost always means P-chain.
- Never print or commit private keys, funded keys, or API tokens.
- The populated `.env`, committee keys, validator staking keys, and generated deployment state are private handover artifacts: back them up, transfer them separately from the public package, and keep every registered validator funded.
