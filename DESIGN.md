# Design: Avalanche for isolated networks

Finalized 2026-07-22 in a long design dialogue. This file records DECISIONS AND THEIR MOTIVATIONS. Motivations matter more than the decisions themselves: code is cheap to regenerate, the reasoning is not. When changing a decision, first check its motivation still fails to apply.

## What this is

A performance-under-failover benchmark toolset for Avalanche L1s in ISOLATED networks, publishable on main, usable by anyone (never name any specific client in this public repo).

- Renamed from "Avalanche for banks" to "Avalanche for isolated networks". Motivation: isolation is the defining technical trait, banking is just the motivating example. Isolation also IMPLIES PoA: a proof-of-stake chain requires open peering with unknown validators, which an isolated network by definition forbids, so "isolated" already means "PoA, fixed membership".
- The motivating workload (kept as the narrative): a single writer issuing an ordered, linear-nonce stream of high-value transactions; the chain must survive a data-center failover with no lost or reordered transactions; we measure sustained throughput THROUGH the failover, not just steady state. The application backstop (single sender + nonce registry + replay of the last few minutes) makes the majority branch canonical by definition and heals gaps, so consensus-level failover need not be perfectly lossless.
- This release's whole point is BOTH P-chain modes in one toolset with an in-place transition. Motivation: releases already exist for the frozen mode (key-swap failover) and the live mode (weight-change failover) SEPARATELY; a third single-mode release would be redundant. The novel deliverable is the frozen-to-proxied transition.
- Deliverable form: primitives + a runbook + dashboards. NO formal per-run report artifact (decided against a scores file): the dashboards and the drills ARE the deliverable; this is a validation/demo kit. We ship what to trigger, the operator owns automation and triggers.

## The two P-chain modes and the relay (the central idea)

There is NO deployment knob for the mode. The architecture is always identical; the mode is purely RELAY STATE:

- **Frozen P-chain** (relay down or absent): nodes serve their last-known P-chain frontier; weight changes cannot propagate in, so failover = key-swap.
- **Proxied P-chain** (relay up): live P-chain flows in through the one DMZ conduit; weight changes propagate, so weight-change failover works.

Always true in both modes:
- Every node bootstraps its P-chain from a delivered snapshot (the frozen seed) and runs `--partial-sync-primary-network`. C and X chains are NEVER synced, forever.
- The relay (follow-only P-chain proxy) is the ONLY path to the outside world.
- The mode transition is literally "turn the relay on". Zero per-node config change: validators always bootstrap from the fleet's RPCs, RPCs always point at the relay's (IP, NodeID); a down relay is just an unreachable peer.

Motivations:
- Isolated networks never get plain internet egress; the only outside connection is a DMZ conduit. So "live mode = open egress" would contradict the product's own premise. The relay IS the product's live mode.
- Snapshot delivery stays mandatory even for relay users: seeding a fleet's P-chain from genesis through one relay is not viable for everybody. Snapshot = initial seed; relay = ongoing feed. This is why frozen delivery is not a "mode" but the universal bootstrap mechanism.
- The production client path: start airgapped (frozen P-chain), connect the DMZ machine to the internet only after their security approves (proxied P-chain). Expected order is key-swap FIRST, then weight-change, as an in-place transition on ONE deployment: same L1, same validators, same committee, no re-creation (unacceptable for a bank to rebuild the chain to go live).
- Benchmark topology: EXACTLY ONE relay, on the control host (the machine that starts the benchmark). Production would run a relay per site; the benchmark is deliberately the quick-test shape. Control models the third site that survives either DC dying, so the benchmark relay placement is even failover-correct.

## Creation

One `l1 create`, run from any designated creation machine with P-chain access. The creation machine does not have to be the client's deployment control machine. Creates BOTH L1s at genesis:

1. Manager L1 (the committee): own subnet + a phantom chain (never deployed, never runs blocks) + ConvertSubnetToL1Tx registering an equal-weight signing committee (staking/manager/m<i>, BLS keys generated and held on control). It is self-managed through its own phantom chain at manager address `0x..01`. Committee size is exactly 1 or 4 (default 1), member weight 1000. One is the minimum, simplest signing authority. Four provides one-signer-loss tolerance with a 3-of-4 quorum. Sizes 2 and 3 add keys without providing that tolerance.
2. Main L1: own subnet + chain + ConvertSubnetToL1Tx registering the fleet's validators, with the recorded validator manager set to the MANAGER L1's chain (address 0x..01). The P-chain verifies this L1's weight changes against the committee's set.

Decisions and motivations:
- Committee ALWAYS, even for a frozen-mode run that never changes weights. Motivation: going proxied must never register anything new on-chain; the committee sits dormant through the frozen mode and becomes the weight authority once proxied. A bank can afford the extra validator.
- Making the main L1 self-managed was DROPPED. Motivation: in a DC failure the high-stake nodes are down; if they are also the signing authority you cannot sign the weight reassignment that recovers from their loss. The separate manager L1 is self-managed through its own phantom chain, while its signing authority remains independent of the fleet that fails.
- Signing is ALWAYS key-only and off-node: the tool holds BLS keys and signs locally, never asks a node to sign. Same motivation: nodes may be dead exactly when signing is needed.
- C-Chain PoA validator-manager contract DROPPED from this toolset. Motivation: the benchmark measures consensus under load, not a Solidity manager; it is client-specific surface. If a client needs it, it is an adapter later (YAGNI).
- Initial weights are baked DIRECTLY into the conversion tx: first 3 validators 100000, all remaining validators 1000. Motivation: (a) a simple opinionated default beats freestyle weight config; (b) writing weights into ConvertSubnetToL1Tx means `create` NEVER touches warp/committee signing, which removes the whole committee-signature dependency from creation and makes create work identically in both modes.
- Weight-change certification at create DROPPED (the old flow registered everyone at 1 and raised 1,2,3 via the committee purely to prove the path). Motivation: the proxied mode exercises weight changing for real; a create-time proof is redundant.
- `genstaking` folded into `create` (generate keys if absent, as create already does for the committee). One command less.
- Configuration comes from `.env`, which every command explicitly loads. `NETWORK` is `fuji` or `mainnet`; Fuji is always called Fuji, never "testnet". `PCHAIN_API` is an optional endpoint override. `MANAGER_COMMITTEE` is `1` or `4`, default `1`. `FUNDING_PRIVATE_KEY` contains the raw 32-byte secp256k1 private key as 64 hex characters with no `0x` prefix. There is no key-file setting.
- `FUNDING_PRIVATE_KEY` is the deployment's only funding identity. Its P-chain address owns creation and validator-balance transactions. Its derived EVM address receives the main L1's genesis allocation and is the transaction sender used by the benchmark. Genesis is rendered from this address during `create`; there is no second key, static pre-funded address, built-in private key, or fallback benchmark account.
- Every registered main and committee validator starts with a 0.1 AVAX continuous-fee balance. `l1 topup <days>` reads the current P-chain fee rate and raises every registered balance to at least that many days of runway. Validators already above the target are left unchanged.
- Creation is not freezing. A chain may be pre-created anywhere with P-chain access. The deployment control machine later syncs beyond both conversions, snapshots its local P-chain state, and ships that snapshot to the isolated fleet.

## Inventory and naming

ini-style inventory; the shape is the user's, we impose almost nothing ("freestyle"). We deliberately do NOT provide a "do good" binary that decides swaps or weights for the user: primitives, not policy.

- Machines and nodes are NUMBERS, not names (numbers go in paths, e.g. data/<n>). Motivation: names added a sync burden (staking dirs, data roots, manifests keyed by name) with no benefit; letters-for-identities were also dropped because `status` already answers "which identity is where".
- role is a property of the NODE: validator | rpc. The ONE functional field.
- Validators are registered in ascending node-number order. The first three in that order receive weight 100000; all remaining validators receive weight 1000.
- `create` generates TLS and BLS staking keys for validators. RPC nodes receive stable TLS identities but no BLS signer keys.
- Many nodes may co-host on one machine, but the unified experience default is one blockchain node per physical machine, permanently, even under key swaps.
- dc= is a freeform tag, default dc1/dcA. Display and selector ONLY (fleet status grouping, batch verbs like `down dc=A` to simulate a whole-DC failure, per-DC dashboard panels are a nice-to-have). Nothing functional may ever depend on it.
- Weights are NOT inventory: on-chain weight is the sole truth.
- There is no generated `registry.json` or `staking/node-ids.env`. NodeID is derived from the TLS certificate; validation ID, weight, and active state come from the P-chain.
- The public package contains no private keys or generated deployment secrets. The populated `.env`, committee keys, validator staking keys, and generated deployment state are transferred separately as a private handover bundle.
- The committee is not in the inventory; create generates it separately. It never runs, but must stay funded (a drained committee validator dilutes the quorum).

## The two primitives

- `place <identity> <node>`: set a validator identity's location. It is a SWAP, not a drop: whatever identity currently sits on the target node rides back to the moved identity's old node. Motivation: we always have exactly as many identities as nodes; a swap preserves that bijection automatically (nothing orphaned, nothing duplicated), which makes equivocation STRUCTURALLY inexpressible rather than merely checked. Executed two-pass (stop both nodes, swap keys on disk, start both) so the two identities are never live crossed mid-move. Weight-agnostic.
- `weight <identity> <weight>`: set a validator's on-chain weight via a committee-signed SetL1ValidatorWeightTx. Location-agnostic. Up or down only: NEVER 0 (zero kicks the validator out of the set) and NO add/remove membership (this is a performance/failover benchmark, not membership management).

Exactly TWO guards, both correctness, not policy:
1. No identity live on two nodes (equivocation forks/slashes). Enforced structurally by swap-shaped place + two-pass execution.
2. No validator<->RPC swap (see RPC section). Everything else is the operator's freedom.

Failover strategies are compositions the OPERATOR makes: key-swap failover = `place` a dead high identity onto a live node (frozen-mode compatible, no P-chain writes). Weight-change failover = `weight` up a low / down a high without moving identities (proxied mode only; lower disruption, no wipe/resync). Deliberately NOT merged into one clever reconcile abstraction: two small code paths beat one clever one at 3am. The two strategies cannot corrupt each other because weight attaches to the identity, not the node, and place preserves the bijection.

## RPC nodes

An RPC node is: a PINNED (IP, NodeID) that tracks the chain, serves ingress, and anchors bootstrap. Nothing to simplify further.

- Stable identities REQUIRED: bootstrap entries are (IP, NodeID) pairs verified by TLS at dial time. The mesh's rendezvous points must never change identity, or every peer's bootstrap entry for them silently rots. Validators can swap freely precisely because nothing anchors on them.
- Zero stake, never registered on-chain, and no BLS signer key at all (run with an ephemeral signer): one less secret to manage, and `weight` cannot even express them.
- `place` refuses them (guard 2). Swapping an RPC identity would break the anchors; swapping a validator identity onto an RPC node would silently de-anchor the mesh.
- Why RPCs exist as a separate role at all: serving tx load on a validator measurably slows its block production, so ingress is kept off validators. RPCs are bombard's ingress and are never promoted.

## Bootstrap topology (two-hop, from the mainnet-committee release)

- Validators bootstrap from the fleet's own RPC nodes.
- RPC nodes bootstrap from the relay upstream: the fleet's ONE allowed external TCP.
- state-sync lists = bootstrap lists (one anchor list; two lists that always contain the same nodes is pointless).
- GOTCHA (proven live): empty bootstrap lists do NOT auto-discover peers from P-chain records; lists must be explicit. And every key swap invalidates any (IP, NodeID) bootstrap entry pointing at a swapped VALIDATOR, which is exactly why anchors are RPCs only. (The interim fleet listed all 12 nodes as bootstrap anchors; it survived swaps only because enough unswapped entries remained. Do not copy that.)

## Deployment simplifications (decided 2026-07-22)

- DELETE the provisioned() existence check; always rsync everything. Motivation: the check treated nodes with STALE keys as provisioned, booting them with wrong NodeIDs (bit us live 2026-07-20); rsync is idempotent and near-instant when unchanged. Deleting the mechanism removes the whole drift-bug class; fixing it would just shrink it.
- NO key pre-staging on nodes. Control holds all keys and pushes the right one at `place` time (~1KB scp). Motivation: one less deploy step, no staging drift, and a compromised box leaks ONE identity instead of eight.
- Keep binaries always rebuilt fresh before every ship (user rule). Aggressive fast iteration: recreate L1s/committees/chains freely, never nurse half-broken on-chain state; the ONE thing preserved rather than redone is the P-chain sync (the expensive part on an isolated fleet).

## Consensus and recovery facts (validated live)

- Wipe-on-down PROVEN (2026-07-18, Fuji, avalanchego 4265498): `down` wipes ONLY the L1 chain data (chainData/<L1chainID> + logs), keeping the P-chain DB; `up` state-syncs the L1 fresh (seconds-to-minutes) and reconverges on the majority branch, no re-fork. Evidence: identical block hashes across all nodes for blocks produced while a node was down+wiped and after rejoin; single branch, quorum 3/3. The June-2026 "must nuke all of data/validator" belief is DISPROVEN.
- Fork recovery for a node that never went down is the same tool: `down` + `up` on any node whose accepted hash diverges from the majority of high validators. Runbook procedure, not automated.
- subnet-config k fix: k (sample size) must be <= validator count. k=20 with 8 validators never finalized (errInsufficientWeight, blocks built but not accepted). Use k=5, alpha=4 for the flat 8-validator topology. Alpha sits just below max so losing ONE high validator (~66%) still finalizes at speed: a single high failover does not halt the chain.
- Isolated-fleet peering needs SG ingress AND egress for the staking port from/to both the intra-region private CIDR and the fleet public IPs (cross-region uses public IPs, same-VPC traffic arrives from private IPs, so public-IP-only rules silently fail intra-region).

## Open items (dev, on the critical path for the proxied mode)

- **Relay (follow-only P-chain proxy) must be built** for the proxied mode: one instance, on control, --p-chain-follow-only pattern (upstream PR #5613).

## Constraints (standing)

- Never name any specific client in git (public repo). STT note: "C-chain" in dictation almost always means P-chain.
- Never print or commit private keys, funded keys, or API tokens.
- The populated `.env`, committee keys, validator staking keys, and generated deployment state are private handover artifacts: back them up, transfer them separately from the public package, and keep every registered validator funded.
