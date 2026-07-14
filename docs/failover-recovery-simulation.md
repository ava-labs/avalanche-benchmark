# The Failover Model

How a failover works on this fleet, and why the chain cannot fork through one.
Vocabulary and commands are the README's; this doc covers the mechanics
underneath `bin/l1 apply`.

## Register once, move weight forever

Every `role=validator` node in `nodes.ini` (`a1..a4` and `b1..b4` in the
shipped inventory) is registered as an L1 validator exactly once, in the
`ConvertSubnetToL1Tx` at chain creation (`setup/02_create_chain.sh`, which
runs `bin/l1 create`), at its `weight=` tag. role=rpc nodes are never
registered. After conversion the validator set never changes membership; a
failover only changes each validator's consensus weight.

The model in a few sentences: the conversion records the L1's validator
manager as living on the L1's OWN chain, at address
`0x0000000000000000000000000000000000000001`. No contract exists there and
none is needed; the P-chain only ever compares that (chainID, address) pair
against the source of each weight-change warp message, and then verifies the
message's BLS aggregate signature against the L1's OWN validator set. We hold
every validator's BLS signer key (`staking/l1/<name>/signer.key`), so `bin/l1`
signs each weight message with all of them locally (100% of stake, always
past the 67% quorum), aggregates, and submits the `SetL1ValidatorWeightTx`
straight to the P-chain. There is no ValidatorManager contract, no courier
daemon, no signature aggregator service, and no C-chain anywhere in the loop;
the only external dependency is one P-chain RPC.

Weight tiers:

| Tier | Weight | Meaning |
|------|--------|---------|
| `validator` | 100000 | carries consensus |
| `spare` | 1000 | registered, synced, negligible vote |
| `dead` | 1 | retired (weight 0 would deregister; we never remove) |

Ground state after chain creation: DC A validators at `validator`, DC B at
`spare` (the `weight=` tags in `nodes.ini`, consumed once by `l1 create`;
reset any time with `scenarios/00_healthy.sh`).

Because staking keys never move between machines, no two live nodes can ever
share an identity, so double-signing is structurally impossible. (The old
key-swap design that moved identities between boxes produced forks and was
deleted; every node wears its one permanent `staking/l1/<name>` identity.)

## The weight flow (`cmd/l1`)

`bin/l1 apply --weights a1=100000,...,b1=1,...` is declarative: it reads the
registered set fresh from the P-chain, drops the no-ops, and applies the
remaining changes one `SetL1ValidatorWeightTx` at a time, ALL RAISES FIRST,
then lowers, verifying each new weight on-chain before planning the next tx.
Every tx is built from a fresh read (current `minNonce`, current set) and
signed fresh, so any crash or rejection is recovered by re-running the same
command: already-applied steps read back as converged and are skipped.
`bin/l1 set-weight --node <name> --weight <w>` is the single-validator form.

Two P-chain realities the tool absorbs:

- Warp signatures verify at the proposer's P-chain height, which can lag the
  tip. A "signature is invalid" or set-mismatch rejection is transient;
  `apply` retries each step with a freshly fetched set, and a manual re-run
  is always safe.
- The public API is load-balanced and individual backends can serve stale
  state (right after conversion one read transiently returned an empty set).
  Set reads retry until they look sane, and `apply` re-reads between txs.

There is no churn cap: the 20% per-op limit was ValidatorManager contract
policy, and the P-chain imposes none, so a full DC seesaw is exactly one tx
per validator (raises first).

## Halt and recovery theory

Weights only move when you ask: a dead box keeps its weight, and its silent
vote counts against quorum until you drain it. With 4 equal validators:

- 1 down: quorum holds (3 of 4), consensus rides through.
- 2 down: quorum lost, the chain HALTS. Expected and recoverable; nothing is
  lost, the chain simply stops finalizing.
- Recovery is either bringing the machines back (`./fleet up`) or seesawing
  weight to live machines (`bin/l1 apply --weights b1=100000,...,a1=1,...`).
  The seesaw works while halted: the weight txs are P-chain txs verified
  against the registered set, not against the (halted) L1.

One latch complicates halted-chain recovery: a (re)starting validator cannot
finish bootstrapping until ~75% of validator stake is connected
(avalanchego's startup latch, `(3*W+3)/4`; surfaced by `./fleet status` as a
node stuck `BOOTSTRAPPING` plus a HINT line). Normal failover never hits it
because the surviving validators are still running. Recovering a fully
halted chain means bringing enough validators up together; they clear the
latch as a group, then finalize within seconds.

**Raise before lower, always.** A single `apply` enforces this internally
(raises first); if you script individual `set-weight` calls, raise the
replacements first yourself. Lower-first passes the fleet through a
low-total-weight window exactly when you are most exposed.

**Check node health before raising.** The old contract engine gated raises on
the target node's sync state; `bin/l1` deliberately does not probe nodes (it
talks only to the P-chain), so that check is yours now: do not raise a node
that `./fleet status` shows behind the fleet tip or unreachable. A
catching-up node with freshly raised stake can win a proposer slot on a stale
height and self-finalize a sibling block (the documented fork wedge). Lowers
are always safe: taking weight off a sick node is the failover direction.

**Balances matter.** A validator whose continuous-fee balance drains goes
INACTIVE: its weight still counts in the total but it cannot sign, which
dilutes the quorum our local signatures must clear. `bin/l1 status` warns
when any validator is under 7 days of runway; top up with
`bin/fuji-wallet topup [days]`.
