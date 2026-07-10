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
| `validator` | 100000 | carries consensus |
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
Current state is read fresh from the contract and the P-chain on every
step, so every action derives from observation, never memory: any crash or
timeout is recovered by re-running the same `./fleet weight` command.
`converge` is initiate-and-poll:

1. **Contract ratchet.** For each slot where contract weight differs from
   desired, fire `initiateValidatorWeightUpdate` txs. The contract's churn
   tracker (period 0) caps each op at 20% of the running total weight, so a
   big move is a seesaw of small steps. The churn math is deterministic, so
   `planSeesaw` simulates the whole ratchet locally and fires it as one burst
   of consecutive-nonce txs (a full DC seesaw is ~10 steps in one burst).
   Raises are planned before lowers, so the total never dips mid-move.
2. **Verify (poll).** Re-read everything and demand desired == contract ==
   P-chain and receivedNonce == sentNonce, retrying on an escalating
   schedule (1s, 5s, 10s, 15s, 30s, 1m, then 2m flat; 24 attempts, ~36 min).

Delivering the emitted `L1ValidatorWeight` warp message to the P-chain
(`SetL1ValidatorWeightTx`) and acking it back to the contract
(`completeValidatorWeightUpdate`) is NOT this engine's job anymore: the
standalone warp-courier daemon (github.com/containerman17/warp-courier,
running on the control box) watches the ValidatorManager on the C-chain and
does both, with strict per-validator ordering, its own signature aggregation
and its own retries. The kit's poll finishing is the end-to-end proof the
courier delivered; a poll that stalls with
`waiting on the warp courier to deliver/ack` means the courier is down or
stuck, so check its logs on the control box. The courier pays P-chain and
C-chain fees from its OWN wallet, never the fleet wallet (two senders on one
account race on nonces).

Signature coverage lag is still real (primary-network validators only sign a
C-chain-originated warp message after syncing the block that emitted it, so
coverage climbs past the 67% quorum over minutes); the courier owns retrying
through it, and the chain stays healthy on its current weights the whole
time, since weight only moves when delivery lands.

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
