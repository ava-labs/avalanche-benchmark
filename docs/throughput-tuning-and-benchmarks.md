# L1 Throughput Tuning & Benchmark Findings

Single-issuer throughput study on the 3-validator + 1-tracker L1 (Tokyo
`m6a.4xlarge`, avalanchego fork `configure-genesis-acp226-excess-50ms-window`).
Goal: stable 4000 TPS, and understand every knob that moves (or doesn't move)
the ceiling. All numbers from benchmarks on 2026-06-03.

> **Historical document.** The study predates the current fleet (12 machines,
> Fuji-anchored, weight-based failover) and its topology/keys/script names are
> the old single-site lab's. The knob analysis and the collapse/fork findings
> still hold. The deployed profile today: `subnet-config.json` k=30,
> alphaPreference=16, alphaConfidence=17, beta=12, 100ms proposer window with
> ms-timestamps; `chain-config.json` min-delay-target 25 (uniform, both
> sites); `run/03_bombard.sh` 4000 rps, inflight 2000, ingress = the pinned
> RPC nodes of both sites.

> **Update (2026-06-29): the proposer window is no longer fixed at 1s.** The
> throughput numbers below stand, but the "never go below 1s" guidance was an
> artifact of the old fork's whole-second timestamp grid, not a fundamental
> limit. The fleet now runs `containerman17/fde` (@ `0844018`), which
> makes the window a per-subnet config knob (`proposerWindowMilliseconds`) and
> adds millisecond-granular proposerVM timestamps (`proposerMillisecondTimestamps`).
> Ms-timestamps remove the 1-second slot boundary that used to unlock a competing
> proposer, so a sub-second window is now safe - the two-DC failover fleet runs
> **100ms** to shrink the per-slot failover stall ~10×. See the proposerVM
> section below for the full mechanism.

---

## TL;DR - recommended SAFE config for stable 4000 TPS (historical, 2026-06-03 lab)

| Parameter | File | Value | Why |
|---|---|---|---|
| `proposerWindowMilliseconds` | `subnet-config.json` | **100** (failover) / 1000 (max throughput) | Per-subnet knob on the `containerman17/fde` fork. With `proposerMillisecondTimestamps: true`, sub-1s is safe (no competing proposer) and cuts the per-slot failover stall ~10×. Set `1000` only if you need the last few % of steady-state ceiling. Requires ms-timestamps - never set sub-1s without it. |
| `proposerMillisecondTimestamps` | `subnet-config.json` | **true** | Ms-granular proposerVM timestamps. The enabler for any sub-1s window - without it the slot clock floors to whole seconds and a short window unlocks a second proposer. |
| `beta` | `subnet-config.json` | **20 (default)** | Full finality safety. Do NOT lower for 4000 - unnecessary and the only real fork risk. |
| `alpha` | `subnet-config.json` | **11** | Lowest per-poll latency (must be > k/2=10). |
| `k` | `subnet-config.json` | 20 | Default. |
| `min-delay-target` | `chain-config.json` | **5** | 1ms is a no-op for throughput and added fragility. |
| `min-delay` initial | `genesis.json` `initialMinDelayMS` | **5** | Matches chain-config; set in genesis or it ramps slowly. |
| inflight cap | `bombard -inflight` | **750** | Shallow = smooth. Deep (≥5000) = unpredictable + near-wedge. (Current fleet: 2000, scaled for the 25ms cadence's higher per-tx latency; see `run/03_bombard.sh`.) |
| ingress | benchmark script (now `run/03_bombard.sh`) | **m4 tracker only** | Single funnel = 0% block reject. (Current fleet: all pinned RPC nodes, same non-validating principle.) |
| gasLimit / fee | `genesis.json` | 200M / no throttle | Block size never the limit. |
| tx-pool caps | `chain-config.json` | 131072 acct-slots etc | Far above any single-issuer need. |

This gives a **~4300 TPS smooth ceiling with zero forks and zero wedge risk**, so
4000 sits comfortably under it. (The earlier `WindowDuration=700ms` build also
reached ~4300 and sustained 4278 TPS for 10 min, but was twitchier with less
margin - because 700ms had no ms-timestamps and so paid the competing-proposer
penalty.) These numbers are from the old 1s-pinned build; the fleet now runs a
100ms window with `proposerMillisecondTimestamps` (see the proposerVM section),
which removes that penalty - re-benchmark for a current steady-state ceiling.

**The forbidden combination:** `beta < ~8` **and** a sub-1s window **without
ms-timestamps** **and** overdrive **and** hard SIGKILLs. That is the recipe for a
permanently diverged ("forked") node (see *Fork/stall danger* below). The
competing-proposer half of that risk is what `proposerMillisecondTimestamps`
removes: with ms-timestamps a sub-1s window keeps one proposer per height, so
`100ms + beta 20 + ms-timestamps` is safe. The danger is sub-1s on the *old*
whole-second grid, or stacking it with low beta. The safe config does none of it.

---

## Topology & ingress

- 3 validators (m1-m3, keys 1/2/3) + 1 non-validating tracker (m4, key 4, zero
  weight, sybil-ON, serves chain RPC on 9652) + control box (5 P-chain primaries
  + `bombard`). Sybil protection ON everywhere (dev-network semantics).
- **Bombard the m4 tracker only**, never the validators directly:
  - Tracker funnel = single clean ingress = **0% block reject**.
  - Spraying all validators = each builds competing blocks from the same stream
    = up to **40% block-reject fork churn** at deep inflight. Worse for a single
    issuer (one nonce sequence gains no parallelism from fan-out).
- The tracker MUST run sybil-ON (a sybil-OFF tracker on a sybil-ON network can't
  bootstrap the L1).

---

## Tunable parameters - what each does and whether it's binding

### proposerVM window - THE lever (avalanchego fork)
- Proposer slot window. `slot = floor(timeSinceParent / window)`. Set via
  `proposerWindowMilliseconds` in `subnet-config.json` (was a compile-time
  `WindowDuration` constant on the old fork).
- **Why a short window used to hurt (the old whole-second grid):** the old fork
  truncated block timestamps to whole seconds (`Truncate(time.Second)`). At each
  1-second boundary a window `< 1s` advanced the slot 0→1, which **unlocked a
  second proposer for the same height** the slot-0 proposer was already building →
  competing builds / dropped "wrong-proposer" blocks → throughput throttle. A
  window `≥ 1s` kept the slot at 0 across the whole second → one proposer per
  height → no contention. This is why the 2026-06-03 numbers said "never < 1s".
- **What changed (`containerman17/fde` @ `0844018`, first shipped as `containerman17/benchmark`):**
  `proposerMillisecondTimestamps: true` makes the slot clock millisecond-granular,
  so the slot only advances when a *full window* of real time elapses - not at an
  arbitrary 1-second tick. A sub-second window now keeps **one proposer per
  height** the same way `≥1s` did, so the competing-proposer throttle is gone.
  The throughput-vs-failover tradeoff that forced 1s is **decoupled**.
- **Why this matters for failover:** when a proposer goes down, the chain stalls
  for ~one window before the next validator's slot opens (the "~1s-per-slot"
  failover finding). A rolling restore restarts three validators, so each one's
  slots stall by a full window. 1s window ⇒ ~1s stalls; **100ms window ⇒ ~100ms
  stalls - a ~10× shorter dip**, with no steady-state cost now that ms-timestamps
  remove the contention penalty.
- Measured smooth ceiling on the old fork (inflight 750, tracker): **700ms ≈ 4300;
  1s ≈ 5000-5500; 5s ≈ 5000-5400** - a **threshold at 1s, not a gradient** (1s and
  5s indistinguishable; 700ms strictly worst *because* of the whole-second grid the
  new fork removes). Re-benchmark 100ms-with-ms-timestamps for a current ceiling.
- Failure mode of a too-short window differs: it folds via `slot0%` dropping
  (slot-misses); a long window folds via `txpool_pending` exploding (slot0% stays 100).
- **Recommendation: `proposerWindowMilliseconds: 100` + `proposerMillisecondTimestamps:
  true`** for the failover fleet (this is the deployed 2-DC config). Raise toward
  `1000` only if a pure steady-state throughput run needs the last few percent.
  Never set a sub-1s window without ms-timestamps (= the old fork's failure mode).

### `beta` (subnet-config.json) - finality margin / consensus latency
- Consecutive successful polls required to finalize a block. Default **20**
  (= the ~24 `rounds/accept` observed). Floor is `ConcurrentRepolls` (default 4).
- Sweep (4000 rps, fork-checked): beta 20→8→4 ⇒ rounds 24→11.5→6.5,
  consensus `accept_lat` 16.6→7.5→**4.1ms** (4× lower). Fork-free at every step
  on this topology, and raised the *edge* ceiling ~20%.
- **But it is the safety knob.** Lower beta = thinner margin against finalizing a
  losing block if two blocks ever compete. Safe here only because we keep **one
  proposer per height** - which on the new fork comes from
  `proposerMillisecondTimestamps`, not from the window being ≥1s (so the 100ms
  failover window does not erode this margin). **Do not lower beta for 4000** -
  unnecessary, and dangerous if combined with a sub-1s window *without
  ms-timestamps* or with overdrive.

### `alpha` (subnet-config.json) - per-poll quorum
- Votes-per-poll needed (of k=20). Backwards-compat field setting both
  AlphaPreference and AlphaConfidence. Must be `> k/2` (so ≥ 11).
- (The deployed fleet has since split the two: alphaPreference=16 /
  alphaConfidence=17 at k=30, chosen so a 3-validator quorum survives one
  node down without tracker mis-finalization; the latency tradeoff measured
  below still applies.)
- Does **NOT** change rounds (that's beta). It changes per-poll cost:
  `accept_lat` 16.6 (α=11) → 18.9 (α=12) → 22.7ms (α=15). **α=11 is already
  optimal for latency**; the avalanchego default 15 is worse here.

### `min-delay-target` (chain-config) + `initialMinDelayMS` (genesis) - block cadence
- ACP-226 dynamic min block delay. `Delay = 1ms × e^(excess/2^20)`; floor 1ms.
  `DesiredDelayExcess(5ms) ≈ 1.687e6` (matches observed `mindelay_excess`).
- **NOT binding.** Setting 5→1ms did NOT change the ceiling: blocks already come
  ~7ms apart (consensus round-trip), well above the 1ms floor. Cadence is gated
  by per-block consensus, not min-delay.
- Gotcha: changing chain-config alone only ramps slowly (`MaxDelayExcessDiff=200`
  per block); to apply instantly set genesis `initialMinDelayMS` and recreate the
  chain (`02`). Keep at 5.

### gas / fee (genesis feeConfig) - NOT binding
- gasLimit 200M (~9500 transfers/block), targetGas max-uint64 (no throttle),
  baseFeeChangeDenominator huge, minBaseFee 1, blockGasCost 0.
- Blocks are never gas-limited; at 4000 TPS a block holds ~38 of ~9500 capacity.
  `txs/blk` grows with offered load (38→340) - blocks fill with available supply,
  they are not capped.

### tx-pool / mempool (chain-config) - NOT binding
- `tx-pool-account-slots=131072` (subnet-evm default is **16**, which would
  hard-cap a single issuer; we override). global-slots 262144, account/global
  queue 131072/512000, lifetime 10s. Far above any inflight we use.

### gossip (chain-config / subnet-evm defaults) - NOT the main lever
- `push-gossip-frequency=20ms` (default 100ms), push-gossip-num-validators 100,
  percent-stake 0.9. Spraying to bypass tracker→validator gossip gained only ~5%
  → gossip is not the ceiling. 200ms gossip alone collapses throughput (only
  useful combined with direct spray).

### inflight cap (bombard) - smoothness/stability knob
- Client e2e latency ≈ inflight ÷ throughput (Little's law). Shallow (750) =
  smooth. Deep (2000) = lumpier. **Deep (≥5000) = unpredictable + near-wedge**:
  3 identical runs at inflight=5000 averaged 1155 / 6507 / 5732 TPS and every run
  dropped to 0 TPS at some point. Use 750.

---

## What we learned about the ceiling

- **Dependable single-issuer ceiling ≈ 5500 TPS** (W≥1s, shallow inflight,
  smooth). 4000 is comfortably under it.
- Peaks of 8-9k are achievable with deep-queue overdrive but live in an
  **unpredictable, near-wedge regime** - not a real operating point.
- **Single-issuer is a serial pipeline:** one key = one strict nonce sequence;
  throughput = inflight ÷ latency. Multiple senders would scale (N parallel nonce
  pipelines → fuller blocks) but parallelizing is off the table by constraint.
- **Latency:** consensus accept is only ~4-16ms; the ~85-130ms client p50 is
  queue depth + the tracker hop (~15-30ms), not consensus. You cannot get 4000
  TPS *and* ~50ms on this 3-validator topology - measured tradeoff (validator-
  direct/spray + shallow inflight gets ~30-56ms but sacrifices steady throughput).

### Collapse signature (leading indicators before a fold)
- **Earliest warning:** `txpool_pending` rises a full regime before throughput drops.
- Approaching the ceiling: `accept_lat` climbs (9→44ms) and `poll_duration`
  climbs (1.5→8.6ms) - consensus is the straining subsystem. `gas/s` saturates
  ~220-253 Mgas/s (≈11-12k tx/s = EVM exec headroom, not the limit).
  `blks_processing` and handler queue stay ~0 (not a processing pileup).
- Collapse is a **cliff**, not a slope: `blkAcc/s` holds then drops to ~0 and the
  chain hard-wedges.

---

## Fork / stall danger (read before lowering beta)

- Avalanche finality is **irreversible**: an accepted block is committed to the
  local DB with no rollback. If a node ends up with an accepted block that is not
  on the network's canonical chain, it is **permanently stranded** - restart
  re-loads the bad tip and the subnet VM FATALs ("stuck bootstrapping").
- Two ways to get there, both of which we triggered during stress testing:
  1. **Hard SIGKILL under load** (reconcile/03 use `pkill -KILL`) → proposerVM and
     inner-EVM DBs left at mismatched heights → dead VM on restart.
  2. **Genuine safety fork** - a node finalizes a block the majority orphans.
     Made likely by a sub-1s window **without `proposerMillisecondTimestamps`**
     (competing proposers each second on the old whole-second grid) + low `beta`
     (thin margin) + overdrive timing races. Ms-timestamps remove the
     competing-proposer trigger, so `100ms + ms-timestamps + beta 20` is not in
     this danger zone.
- **Recovery:** a plain restart will not fix it. Wipe that node's chain DB to
  force re-bootstrap from the canonical chain, keeping its identity:
  `./fleet up <m>` (one node) or `./run/01_deploy.sh` (all).
  After a hard wedge, allow ~25s settle + a low-rps warm-up before measuring.
- **Rule:** never stack low-beta + sub-1s-window-without-ms-timestamps + overdrive
  + hard kills. (A sub-1s window *with* `proposerMillisecondTimestamps` is safe.)

---

## How to observe (metrics)

- `bombard` scrapes ~45 node metrics (start/end deltas) and prints a per-node
  panel: consensus (accepted/rejected/polls/accept_lat/rounds), proposer slots,
  handler traffic, subnet-evm execution (txs/gas/blocks), txpool.
- Flags: `-scrape <urls>` decouples the metrics target from the send target
  (send to tracker, observe validators); `-sample 1s` prints compact per-second
  rows (txAcc/s, blkAcc/s, blkRej/s, proc, slot0%, pendMax, pf/s) to catch
  per-second dynamics the start/end panel hides.
- Metric prefixes: L1 EVM metrics are `avalanche_subnetevm_vm_eth_*` (the
  `avalanche_evm_eth_*` series are the C-chain, all zero). subnet-evm
  `execution`/`validation` timers read 0 in the consensus-accept path (they only
  populate on import/sync), so build-time isn't visible there.
- **Fork check** after any risky run: compare `eth_getBlockByNumber` hash at a
  common height across all validators - identical = no fork.

---

## Test log (2026-06-03)

- Wedge near 4k on 20M-gas/20ms → fixed by 200M gas + 5ms delay (cluster spec, not
  the load generator; WS-pool generator did not beat HTTP).
- Tracker funnel vs validator-direct vs spray characterized; tracker = stable.
- Latency shown to be Little's-law/queue + tracker hop, not consensus.
- proposerVM window 700ms → 1s/5s: +20-28% smooth ceiling; root-caused to the
  1-second timestamp grid.
- min-delay 5→1ms: no-op. Block packing: not capped, fills with supply.
- alpha 11/12/15 and beta 20/8/4 swept; beta drives rounds & consensus latency
  (4× lower at beta=4), fork-free on this topology; alpha=11 optimal.
- Collapse signature and deep-queue unpredictability (inflight 5000) documented.
