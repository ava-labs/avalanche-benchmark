# The Failover Model

How a failover works on this fleet, and why the chain cannot fork through one.
Vocabulary and commands are the README's; this doc covers the mechanics
underneath `./fleet weight`.

## Register once, move weight forever

Every stake slot of BOTH sites (validators and spares, `a1..a4` and `b1..b4`
in the default shape) is registered as an L1 validator exactly once, in the
`ConvertSubnetToL1Tx` at chain creation (`setup/02_create_chain.sh`). RPC
slots are never registered. After conversion the validator set never changes
membership; a failover only changes each slot's consensus weight through the
ValidatorManager contract on Fuji's C-chain.

Weight tiers (one per `./fleet weight` invocation):

| Tier | Weight | Meaning |
|------|--------|---------|
| `validator` | 1000000 | carries consensus |
| `spare` | 1000 | registered, synced, negligible vote |
| `dead` | 1 | retired (weight 0 would deregister; we never remove) |

Ground state after deploy: site A slots at `validator`, site B slots at
`spare` (`slotWeight` in `cmd/create-l1/main.go`, re-seeded by
`seedWeight` in `cmd/reconcile/plan.go`).

Because staking keys never move between machines, no two live nodes can ever
share an identity, so double-signing is structurally impossible. (The old
key-swap design that moved identities between boxes produced forks and was
deleted; see `internal/topo/topo.go` for the one-permanent-key-per-slot rule.)

Each slot's validationID is deterministic: `sha256(subnetID ||
uint32BE(conversion index))`, with the conversion order fixed by
`topo.StakingSlots()`. That is why the engine never scrapes receipts or logs:
everything it needs is recomputable.

## The weight engine (`cmd/reconcile/weights.go`)

Desired weights live in the local intents JSON (written by `./fleet weight`).
Current state is read fresh from the contract and the Fuji P-chain on every
step, so every action derives from observation, never memory: any crash or
timeout is recovered by re-running the same `./fleet weight` command.
`converge` runs four phases:

1. **Contract ratchet.** For each slot where contract weight differs from
   desired, fire `initiateValidatorWeightUpdate` txs. The contract's churn
   tracker (period 0) caps each op at 20% of the running total weight, so a
   big move is a seesaw of small steps. The churn math is deterministic, so
   `planSeesaw` simulates the whole ratchet locally and fires it as one burst
   of consecutive-nonce txs (a full DC seesaw is ~10 steps in one burst).
   Raises are planned before lowers, so the total never dips mid-move.
2. **Deliver to the P-chain.** Each initiate emits an `L1ValidatorWeight`
   warp message from the C-chain. The engine reconstructs the FINAL
   (highest-nonce) message byte-exactly, gets it BLS-aggregate-signed by the
   Fuji primary network, and issues one `SetL1ValidatorWeightTx`. P-chain
   nonce skipping (ACP-77) collapses the ratchet intermediates: only the last
   message needs delivering. Raises are delivered before lowers.
3. **Ack back to the contract.** The P-chain-sourced ack message (its current
   nonce and weight) is aggregated and fed to
   `completeValidatorWeightUpdate`, so the contract's `receivedNonce` catches
   up to `sentNonce`. Pure bookkeeping; consensus weight already moved in
   step 2.
4. **Verify.** Re-read everything and demand desired == contract == P-chain
   and receivedNonce == sentNonce.

Delivery is keyed on nonces, not weights: `minNonce <= sentNonce` means
undelivered even when the weights already match (an abandoned
demote-then-repromote seesaw leaves weights equal with the contract nonce
ahead, and the ack can never be signed until that nonce lands). A
weight-equal delivery is legal and exists exactly to advance `minNonce`.

## Signature aggregation

The P-chain accepts a `SetL1ValidatorWeightTx` only with a BLS aggregate
signature covering 67% of Fuji primary-network stake. Aggregation
(`internal/valmgr/warp.go`) uses our private signature-aggregator
(avaplatform/signature-aggregator on fly.io, scale-to-zero) as primary and
Glacier's public aggregator as fallback. Each backend gets 3 tries with 3s
pauses: the private one usually errors once on a cold start and succeeds on
the immediate retry, and falling back to Glacier on that first error would
land on Glacier's cache (below).

Before issuing, `precheckQuorum` runs the P-chain's exact warp verification
(signer weight AND the full BLS aggregate) against the current
proposed-height Fuji validator set. A rejected precheck costs nothing: the
message stays undelivered and the next attempt re-checks.

## Failure modes hit live (2026-07-07/08) and their mitigations

- **Fuji coverage lag.** Fuji validators only sign a C-chain-originated warp
  message after syncing the block that emitted it. Right after an initiate,
  coverage sits below the 67% quorum (measured ~52%) and climbs over minutes.
  Mitigation: the converge loop retries on an escalating schedule (1s, 5s,
  10s, 15s, 30s, 1m, then 2m flat; 24 attempts, ~36 min) and the chain stays
  healthy on its current weights the whole time, since weight only moves when
  delivery lands.
- **Glacier's per-message cache.** Glacier returns a cached aggregate for a
  given unsigned message (identical bytes hours apart; no cache buster), and
  does not error on under-quorum. The signer bitset indexes the canonical
  validator set at aggregation time, so as the Fuji set drifts the cached
  bitset maps to different validators (seen: the same signature summing 40.6%
  then 67.7% of stake). Mitigation: the private aggregator (fresh aggregation
  per request) is primary, and the full-Verify precheck catches a stale
  cached aggregate that a weight-only check would pass.
- **Nonce lag.** Weights match but the P-chain nonce trails the contract,
  wedging acks. Mitigation: delivery keyed on nonces as described above.

## Halt and recovery theory

Weights only move when you ask: a dead box keeps its weight, and its silent
vote counts against quorum until you `weight dead` it. With 4 equal
validators:

- 1 down: quorum holds (3 of 4), consensus rides through.
- 2 down: quorum lost, the chain HALTS. Expected and recoverable; nothing is
  lost, the chain simply stops finalizing.
- Recovery is either bringing the machines back (`./fleet up`) or seesawing
  weight to live machines (`weight validator <live spares>` first, then
  `weight dead <dead boxes>`). The seesaw works while halted: the
  ValidatorManager lives on Fuji's C-chain, not on the halted L1.

One latch complicates halted-chain recovery: a (re)starting validator cannot
finish bootstrapping until ~75% of validator stake is connected
(avalanchego's startup latch, `(3*W+3)/4`; surfaced by `./fleet status` as a
node stuck `BOOTSTRAPPING` plus a HINT line). Normal failover never hits it
because the surviving validators are still running. Recovering a fully
halted chain means bringing enough validators up together; they clear the
latch as a group, then finalize within seconds.

**Raise before lower, always.** `weight validator <new>` first,
`weight dead <old>` second. The engine orders raises first within one
command too, but across commands only you enforce it. Lower-first passes the
fleet through a low-total-weight window where the churn cap also gets
tighter (20% of a smaller total), making the recovery seesaw slower exactly
when you are most exposed.
