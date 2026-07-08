# Failover Recovery Strategy (Simulation)

Minimal toolset to simulate validator failover on a fixed fleet of machines
running an Avalanche L1. This doc walks the default **3-validator** shape; the
counts are configurable per site in `.env` (`VALIDATOR_IPS` ≥ 3, `SPARE_IPS`,
`RPC_IPS` — length = count, values = placement), and the quorum / rejoin
thresholds below scale with the validator count `N` (quorum `ceil(2/3·N)`, rejoin
latch `ceil(75%·N)`). Hard constraint: the L1 must keep functioning down to a
quorum of live validators. We do **not** add validators to dilute the dead-node
proposer fraction.

## Topology

- **Pool** = the per-role lists (default: 3 validators + 1 hot spare + 2 pinned
  dedicated-RPC nodes). The worked example here uses the smaller 3/1/1 single-site
  shape (machines 1–5):
- Steady state after deploy: `m1=v1` (key `staking/l1/6`), `m2=v2` (key 7),
  `m3=v3` (key 8), `m4=spare` (non-validating key `staking/l1/9`), `m5=rpc`
  (pinned non-validating key `staking/l1/10`).
- **`m5` is the dedicated ingress: bombard `m5` only.** It is *pinned* — the
  reconcile engine never promotes it to a validator (see "Mapping policy"), so
  this clean non-validating RPC path **survives every failover event**. The hot
  spare `m4` is what gets promoted when a validator goes down, which is exactly
  why ingress must not ride on `m4`. (Measured 2026-06-04: a dedicated
  non-validating RPC holds ~4000 TPS glass-smooth; ingress on the validators or
  on the spare degrades.)
- The benchmark (`bombard`) is also **failover-native**: it can be pointed at
  multiple pool RPCs and it fans sends across them, runs one watcher per
  reachable endpoint (first-to-see-in-a-block wins), skips unreachable nodes
  instead of aborting, and resubmits in-flight txs so a node crash that drops the
  mempool self-heals. That multi-endpoint mode is available if you want ingress
  redundancy, but the steady benchmark uses the single pinned `m5`.

## Model: 5 keys, conserved

There are exactly **5 staking identities** in play and every pool machine always
holds exactly one:

- 3 **validator keys** (`staking/l1/6,7,8` = `v1,v2,v3`) — registered on the
  P-chain at `create-l1`, **permanent and IP-agnostic**.
- 1 fixed **non-validating key** (`staking/l1/9`) — never staked. The spare and
  any cordoned machine wear this.
- 1 fixed **pinned RPC key** (`staking/l1/10`) — never staked, worn only by
  `m5`. It is sticky and is **never** handed an orphaned validator key, so it
  cannot be drawn into the validator rotation. This is the only difference the
  5th machine adds to the conserved-keys model; the failover dance among the
  other four is unchanged.

**Linchpin (confirmed):** an L1 validator is identified by **NodeID + BLS key**,
not by IP. NodeID comes from `staker.crt`, the BLS key from `signer.key`. Copy
all three files (`staker.crt`, `staker.key`, `signer.key`) for a validator onto
another machine and start it — that machine **becomes** that validator to the
whole network, which re-discovers its new IP via signed IP gossip. **No P-chain
transaction, no re-registration, no IP/session binding.** The validator set is
fixed at 3 NodeIDs forever; failover only changes *which physical machine hosts
each NodeID*.

Because a validator key exists in exactly one place on the cluster at a time,
**double-signing is structurally impossible** — it is a property of conserving
the keys, not a guard we bolt on. The non-validating keys may sit on several
stopped machines (they are never staked). **One floating non-validating key (9)
is enough** for the spare role: among machines 1–4 (3 validator keys + 1 spare),
at most one is ever left to *run* as a non-validator at a time, so two nodes
never claim NodeID-9 simultaneously (which would collide on the network). The
pinned RPC key (10) is a *separate* identity worn only by `m5`, so it never
collides with the spare's key 9 — that distinctness is exactly why the RPC node
needs its own key rather than reusing 9.

## Intent: cordon

The only declared intent is whether a machine is **cordoned** (standard term for
"operator marked this node out of service"). It is desired state, not observed:

- **Uncordoned** → should be running.
- **Cordoned** → should be stopped.

`down` cordons, `up` uncordons. Cordon is the only thing a *human* sets; the
intended key mapping is *computed* from it (sticky) and both are persisted
together in the intentions JSON (see "Desired state vs observed state").

## One engine, four flavors

There is a single reconcile engine. The "scripts" are thin wrappers that set
intent and call it. Reconcile **absorbs the old `03` deploy** — converging a bare
machine to "binary + configs + key + running process" is just the first
reconcile; upload-if-missing is the same "make actual match desired" move as
placing a key.

- **`03` → `reconcile --fresh`** — clear all cordons, wipe `data/` on every pool
  machine, **force re-upload** of binary/plugin/configs, reset the intentions to
  the **default seed** `{m1:6, m2:7, m3:8, m4:9(nv), m5:10(rpc)}`, start all five.
  A fresh deploy against the existing chain from `02`; a brand-new chain means
  re-running `01`/`02`.
- **`scripts/failover/down.sh <m>`** — cordon machine, then reconcile.
- **`scripts/failover/up.sh <m>`** — uncordon machine, then reconcile.
- **`scripts/failover/failover.sh`** — pure reconcile, no intent change.

What stays separate is **`00`/`01` (secrets + Fuji wallet funding)** and **`02`
(`create-l1` on Fuji)** — those make the chain *exist* and reconcile cannot
reissue them. Reconcile owns all five pool machines forever after, first deploy
and every failover.

No lockfile: the client is trusted to drive from a single terminal session,
serially.

## Desired state vs observed state

The persisted desired state is a single local JSON — **the "intentions"**
(`scripts/failover/intentions.json`, gitignored) — with one entry per machine:
`{cordoned: bool, key: int}`, where `key` is the *intended* identity (`6/7/8`
validator, `9` nv spare, `10` pinned rpc). It is the sole persisted state and the
source of truth for what each machine *should* be. If absent (or on `--fresh`),
it is seeded to the default `{m1:6, m2:7, m3:8, m4:9, m5:10}`.

- **`up`/`down`** mutate `cordoned`, recompute the sticky key mapping (from the
  *previous* intentions), and write the new JSON, then reconcile.
- **`failover.sh`** (pure reconcile) does **not** recompute the mapping — it just
  re-applies the existing JSON to reality. So there's no thrash from reconciling
  against mid-transition on-disk state; the mapping only ever changes on `up`/`down`.

Reconcile then observes reality only to detect **drift from the JSON**:

- **Liveness** — `pgrep -x avalanchego` over SSH: is the process alive? Not HTTP —
  a node mid-bootstrap is alive but not yet serving, and an HTTP readiness check
  would wrongly kill and restart it, wiping its progress. Uncordoned + process
  dead ⇒ reconcile starts it (this is how an accidental crash self-heals).
- **Actual key** — `cat staking/active/key_index` over SSH (placement drops this
  one-line marker; the active key files live in `staking/active/` and the launch
  always points there). Compared against the JSON's intended key to decide whether
  a stop-swap is needed.

Because intent is **written down** (not inferred) and reality is read fresh every
run, **partial failure is not a category**: if reconcile dies mid-apply (SSH hang,
Ctrl-C), just run `failover.sh` again — it re-converges reality to the JSON. No
rollback, no crash-recovery path, nothing to disambiguate.

## The reconcile loop

Reconcile converges reality to the JSON in **two passes: stop-swap, then start**
(after a pass 0 that ensures artifacts exist). There is no free-standing "place a
key" step — **writing a key is only ever the tail of a stop-swap**, so a key is
never written to a live process by construction, not by a rule to remember.

**Pass 0 — ensure provisioned.** Each pool machine must have the avalanchego
binary, the subnet-evm plugin, and the chain/subnet/node configs. Reconcile
uploads these **only if missing** (cheap `test -f` check), so the first-ever
reconcile provisions a bare cluster and later ones skip it. `--fresh` forces
re-upload and wipes `data/`. This never touches `data/` otherwise, so the hot
spare's synced DB survives.

**Pass 1 — stop-swap.** For each machine, stop it (`pkill`) if it is cordoned, or
if its actual on-disk key (`cat`) ≠ its intended key (JSON). Whenever the key must
change, **wipe `staking/active/` first, then copy in the intended committed key**
(`staking/l1/<idx>`) + rewrite the `key_index` marker — all *after* the stop, as
one stop-swap. Wipe-before-write means a crash mid-change leaves the key
**missing, never duplicated**; a re-run simply re-copies the intended key from the
JSON. (Keys are never generated — the four identities are fixed committed key
sets; "swap" = copy the intended one.) Doing every stop-swap before any start is
what makes every permutation (including a full swap/cycle) safe in one pass —
there is never an instant where two live machines hold the same validator key.

**Pass 2 — start.** For each **uncordoned** machine whose **process is dead**
(`pgrep`), start avalanchego pointing at `staking/active/`. By now pass 1 has made
the on-disk key equal the intended key, so the machine comes up as exactly what
the JSON says — a genuine crash comes back unchanged, a stop-swapped machine comes
back as its new identity, a freshly uncordoned machine starts. Start never reasons
about keys — liveness only answers "should be running but isn't." **DB is
preserved** (never wiped) so a hot spare rejoins in seconds; only `--fresh` wipes.

Idempotent: a second run stop-swaps nothing and starts nothing.

**Bootstrap peers:** reconcile derives each slot's `--bootstrap-ips/-ids` by
role — RPC slots follow the pinned public Fuji peer (`FUJI_UPSTREAM_IPS/IDS`);
every other slot follows its own site's RPC slots (pinned home identities, so
the list is static per deploy). Because private IPs are never gossiped, the L1
mesh is seeded explicitly via `--state-sync-ips/-ids`, regenerated from the
current intentions on every start so the seeds track identity moves.

**Startup flags are stored nowhere** — reconcile regenerates the launch invocation
every time, and crucially it is **identity-agnostic**: `--staking-tls-cert/key/
signer` always point at the fixed `staking/active/` path (identity lives in the
*files* there, swapped in pass 1), so the command is byte-identical whether the
machine runs v1, v2, or nv. Only `--public-ip` (the machine's own, from
`NODE_IPS`) and the P-chain bootstrap set (control machine's public IP via
`checkip` + `node-ids.env`) vary, and both come from existing config at runtime.
The generated script is dropped on the node for debuggability but is ephemeral —
overwritten each launch, never read back as truth.

**SSH failures are fatal.** Any `ssh`/`scp` that fails aborts the reconcile loudly
("the simulated up/down went wrong") — no best-effort skipping. Re-running after
fixing the box converges.

## Mapping policy: sticky

Only **move what has to move**. A machine keeps the key it already holds while
that key still needs to be live. Only an **orphaned** validator key — one held by
a **cordoned** machine — is reassigned, to a free uncordoned machine (the current
spare), chosen deterministically (lowest machine number). When there is no free
machine, the orphaned key simply stays uncovered.

**The pinned RPC machine (`m5`, key 5) is excluded from this entirely.** It is
checked first in `ComputeMapping` and always keeps key 5 — it is never counted
as a "free" machine, so an orphaned validator key can never be assigned to it,
even when that key would otherwise go uncovered (it just stays uncovered, exactly
as it would without `m5`). The pin is sticky across cordon/uncordon of `m5`
itself: taking the RPC node down for maintenance and back up does not change its
identity. This is what guarantees ingress on `m5` survives any failover. Note
this costs nothing in availability: a double validator failure already lands at
2/3 (quorum holds) whether or not a would-be second spare exists, and only a
*triple* failure halts — the same as the 4-machine model.

**Orphaning is driven by cordon intent only, never by liveness.** A genuine crash
of an *uncordoned* validator is not a failover: its key is still "kept" (step 1),
and the start phase simply brings the **same** node back with its **own** key (DB
intact, fast) — no key swap, no spare takeover. The spare steps in *only* when you
cordon. If a validator's box is genuinely, unrecoverably dead, the operator
**cordons** it to hand its identity to the spare; cordon is the failover trigger,
full stop. We do not build for unattended genuine-machine-down.

Consequence: when a cordoned machine is later uncordoned, the validators are
already covered, so it takes the non-validating key and **becomes the new spare**.
The validator identity does not migrate back — "machine N" and "validator N"
decouple after the first failover, and the spare role rotates around the fleet.
Minimum churn, no needless restarts of healthy validators.

## Hot spare, not cold

The spare is a **permanently-running non-validating full node that tracks the
subnet and stays tip-synced**. On takeover it restarts with the dead validator's
key but **keeps its `data/validator` DB**, so it rejoins as a validator in
seconds. A cold spare would `rm -rf` and re-bootstrap, and a wiped L1 node
*replays every block* rather than state-syncing — the measured "failover gap"
would be dominated by catch-up, not consensus. Identity lives in the staking
keys, not the DB, so reusing the DB under a new NodeID is correct.

## Quorum: allowed to halt

Never blocked. With the spare, `down.sh 2` keeps all three validator identities
live (v2 moves to m4) — **3/3**. A second cordon (`down.sh 3`) has no spare left,
v3 goes dark — **2/3, quorum holds**. A *third* cordon drops to **1/3 and the
chain halts**. Driving the cluster to a halt to watch quorum recover (uncordon a
machine → orphaned key gets re-covered → chain resumes) is a first-class
scenario. Reconcile prints a plain `live validators: N/3` line and never refuses.

## Worked example

Start: `m1=v1`, `m2=v2`, `m3=v3`, `m4=spare(nv)`. All alive.

`down.sh 2`:
1. Cordon `m2`.
2. Stop-swap: `m2` is stopped (cordoned) and re-keyed to `nv (key9)`; `m4` is
   stopped and re-keyed `nv → v2 (key7)`.
3. Start: `m4` starts as `v2` with its on-disk key (DB preserved → fast). `m2`
   stays down (cordoned).

Result: live = `v1(m1), v3(m3), v2(m4)` = **3/3**. Only real restart: `m4`.

`down.sh 3`: stop `m3`, `v3` orphaned, no free machine → uncovered; `m3 ← nv`.
Live = `v1(m1), v2(m4)` = **2/3**, quorum holds.

`down.sh 1`: `v1` orphaned, no spare → uncovered. Live = `v2(m4)` = **1/3** →
**halt** (expected; this is the quorum-recovery test).

`up.sh 3`: uncordon `m3` → reconcile sees orphaned validator keys and a free
machine → `m3` takes one → back to **2/3**, chain resumes. `m3` is now hosting a
different validator identity than it started with.

## Implementation: Go brain, bash hands

The decision logic is a **pure function** — `(observed state) → (desired key
mapping + stop/place/start actions)` — and it is the only part with real logic
worth getting exactly right (sticky retention, orphaned-key collection,
validator-over-non-validating priority, swaps, double-failure, spare rotation).
It lives in **Go** with table unit tests that run with no cluster (state in,
actions out).

The I/O is the easy part and is bash-shaped: SSH in, `pgrep -x avalanchego` for
liveness, `cat staking/active/key_index` for the actual key, `pkill`, `scp` a key
set into `staking/active/`, start the process. The Go binary shells out for these
(replicating the YubiKey-safe SSH options from `_common.sh`). Thin bash wrappers
(`scripts/failover/{up,down,failover}.sh`, and `03`) source `_common.sh` +
`network.env`, compute the static P-chain bootstrap set, export it, and call the
binary.

**Layout:**
- `cmd/reconcile/plan.go` — pure planner (`ComputeMapping`, `Plan`, `LiveValidators`).
- `cmd/reconcile/plan_test.go`, `state_test.go` — table tests, cluster-free.
- `cmd/reconcile/state.go` — intentions JSON load/save + `retarget` (cordon toggle → sticky remap).
- `cmd/reconcile/remote.go` — SSH/scp I/O, observe, stop, swap, start, provision.
- `cmd/reconcile/main.go` — CLI (`fresh`/`down <m>`/`up <m>`/`apply`) + the 3-pass loop.
- `scripts/failover/{_failover_common,up,down,failover}.sh` — wrappers.
- `run/01_deploy.sh` — `reconcile fresh`. `run/03_bombard.sh` — the load generator (historically bombarded the pinned RPC node `m5` only).
- `bin/reconcile` — `make build` target.
