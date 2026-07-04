# Fuji migration plan: anchor the benchmark L1's P-chain on Fuji testnet

Status: PLAN ONLY. No infra changes. Written 2026-07-04 against this repo tip and
avalanchego `containerman17/fde` @ `084401863ba97267ea95ac25c4f285f183b0045c`
(master + the 6 open PRs incl. #5613 p-chain-follow-only), which the Makefile now pins.

## Target topology

- Validator machines: ZERO external connectivity, only reach RPC machines in the same DC.
- RPC machines: exactly ONE outgoing TCP (avalanche p2p on 9651, not HTTP) to a public
  Fuji P-chain node. They run `--p-chain-follow-only` (PR #5613) to track Fuji's P-chain.
- Validators get P-chain blocks FROM the RPC machines (second hop), also via follow-only.

All avalanchego citations below are paths in the pinned commit `0844018`.

## KEY QUESTION: can a follow-only node serve P-chain blocks to a downstream follow-only node?

**YES. Two-hop chaining works with stock #5613, no extra mechanism needed.** The chain of
evidence:

1. A follow-only chain stays in the `snow.Bootstrapping` state forever: after catching up
   it never hands off to consensus and just re-arms a 2s poll timer
   (`snow/engine/snowman/bootstrap/bootstrapper.go:767-775`), and every timer fire
   restarts bootstrapping (`bootstrapper.go:802-812`). The poll interval is 2s
   (`bootstrapper.go:36-40`).
2. Incoming sync requests are dispatched to whichever engine matches the chain's CURRENT
   state (`snow/networking/handler/handler.go:528`), and in `Bootstrapping` that is the
   bootstrapper (`snow/networking/handler/engine.go:28-29`).
3. The bootstrapper embeds a full `common.AllGetsServer`
   (`snow/engine/snowman/bootstrap/config.go:17`), wired to the SAME snowman getter the
   consensus engine uses (`chains/manager.go:1403` for the bootstrapper,
   `chains/manager.go:1379` for the engine). So while permanently bootstrapping it still
   answers `GetAcceptedFrontier`, `GetAccepted`, `GetAncestors`, and `Get`.
4. That getter reads straight from the VM, with no consensus dependency:
   `GetAcceptedFrontier` returns `vm.LastAccepted`
   (`snow/engine/snowman/getter/getter.go:140-147`), `GetAncestors` serves stored blocks
   (`getter.go:185-209`), `Get` likewise (`getter.go:211`).
5. The upstream follow-only node's `LastAccepted` ADVANCES because follow-only bootstrap
   fetches and executes blocks, then records the new tip
   (`bootstrapper.go:748-771`). So the frontier it serves downstream moves with Fuji.
6. The downstream node fetches from any connected peer, validator or not: the bootstrap
   `PeerTracker` has no validator filter, it tracks every peer that connects
   (`network/p2p/peer_tracker.go:274-297`, constructed with only an ignore-self set at
   `chains/manager.go:1314-1319`).

Requirements and caveats for the second hop:

- The downstream node must list the upstream RPC machines in `--bootstrap-ips` /
  `--bootstrap-ids`: follow-only explicitly re-syncs from the peers listed there
  (`config/flags.go:274`).
- Startup latch: bootstrapping only starts once >=75% of BEACON weight is connected
  (`chains/manager.go:1355`, `(3*bootstrapWeight+3)/4`). With 2 equal-weight RPC beacons
  a validator needs BOTH connected at startup; list both RPCs but be aware of this, or
  accept that a validator restart while one RPC is down wedges until it returns.
- Latency: each hop polls every 2s (`bootstrapper.go:40`), so validators see Fuji
  P-chain blocks roughly 2-4s after the RPC tier does. Fine for L1 anchoring
  (validator-set changes and proposerVM height lookups), irrelevant to L1 block latency.
- Health semantics differ from today: the node-level `bootstrapped` health/readiness
  check PASSES for a caught-up follow-only node (the subnet is marked bootstrapped
  before the follow-only early-return, `bootstrapper.go:765`, check at
  `chains/manager.go:1488-1499`), but `info.isBootstrapped("P")` stays FALSE forever
  (it requires `snow.NormalOp`, `chains/manager.go:1476-1485`). Any script that gates on
  `info.isBootstrapped` must switch to `/ext/health/readiness`.

## Concrete changes, ranked by effort (largest first)

### 1. (L) Node launch rework in `cmd/reconcile` (per-role flags, Fuji network)

The single launch template at `cmd/reconcile/remote.go:440-470` currently starts every
pool node identically with `--network-id=local` (`remote.go:456`) and one shared
bootstrap list (`remote.go:465-466`, fed from `PCHAIN_BOOTSTRAP_IPS/IDS` env,
`remote.go:135-136`). Needed:

- `--network-id=fuji` everywhere (built-in Fuji genesis, no genesis flags needed).
- `--partial-sync-primary-network=true` everywhere so nodes sync ONLY the P-chain and
  skip Fuji's X/C chains (`config/flags.go:273`); without it every box replays Fuji's
  C-chain.
- `--p-chain-follow-only=true` everywhere (`config/keys.go:150`): none of our machines
  are Fuji primary-network validators, all of them only track.
- Per-role bootstrap lists (this breaks the "identity-agnostic identical start script"
  assumption, hence the L):
  - RPC role: `--bootstrap-ips=<fuji-node-ip>:9651 --bootstrap-ids=<fuji-NodeID>`
    (the ONE allowed outgoing TCP; the peer's NodeID must be known and pinned).
  - Validator/spare role: `--bootstrap-ips/-ids` = our RPC machines (DC-internal IPs,
    port 9651).
- `--public-ip` for validators must be the DC-internal address; today it is the public
  `in.host` (`remote.go:452`) and `_common.sh:166` / `01_bootstrap_primary_network.sh:92`
  discover it via `curl checkip.amazonaws.com`, which an isolated validator cannot reach.
- Kill the `PCHAIN_BOOTSTRAP_*` plumbing that points at the control box's 5 local
  primaries (`scripts/failover/_failover_common.sh:41-42`, `_common.sh:116-177`).
- Keep `--track-subnets` as is (`remote.go:464`).

### 2. (L) `cmd/create-l1` on Fuji: real key, real fees, faucet funding

Today it is hardwired to the local devnet: P-chain API `http://127.0.0.1:9650`
(`cmd/create-l1/main.go:30`) and the pre-funded local-network EWOQ key for both the
wallet and the subnet owner (`main.go:109`, `main.go:117-120`). Needed:

- Make the P-chain URI a flag/env. Simplest path: run create-l1 ONCE from the control
  box (or laptop) pointed at the public Fuji API (`https://api.avax-test.network`).
  That sidesteps the open question of whether a follow-only node relays
  `platform.issueTx` (it never reaches `NormalOp`, so do not rely on it for issuance).
- Replace `genesis.EWOQKey` with a key loaded from env (e.g. `PCHAIN_PRIVATE_KEY` in
  `.env`, gitignored). This key becomes the L1 owner: guard it.
- Fund it from the Fuji faucet (core.app faucet, 2 AVAX per request, coupon codes give
  more). Budget: CreateSubnetTx + CreateChainTx + ConvertSubnetToL1Tx fees are small
  (well under 0.1 AVAX post-Etna dynamic fees), but each L1 validator carries a
  continuous-fee `Balance`, currently 1 AVAX each (`main.go:253`, `units.Avax`).
  For N validators plan roughly N+1 AVAX up front and a top-up path
  (`IncreaseL1ValidatorBalanceTx`) for long-running fleets: at the Fuji L1 validator
  fee rate 1 AVAX lasts on the order of weeks, verify before long benchmarks.
- `Weight: units.Schmeckle` (`main.go:252`) is fine on Fuji (weights are L1-internal).
- The 5s "wait for chain" sleep (`main.go:171`) becomes a real poll: Fuji P-chain
  acceptance is not instant-local.

### 3. (M) Retire the local devnet P-chain and its assumptions in scripts

- `01_bootstrap_primary_network.sh` (the whole script) exists only to run 5 local
  `--network-id=local` primaries on the control box (`01_bootstrap_primary_network.sh:116`,
  count at `:13`). Replaced by: nothing (Fuji IS the primary network). Its slot in the
  run order becomes "wait for RPC tier to catch up to Fuji P tip".
- `06_cleanup.sh:11` (kills the local P-chain validators): drop that section.
- `_common.sh` P-chain helpers (`PCHAIN_NODE_COUNT`/ports at `_common.sh:116-119`,
  `pchain_node_ids_csv` `:153-160`, `pchain_public_ip` `:162-167`,
  `pchain_public_staking_ips_csv` `:169-177`): delete or repoint at the RPC tier.
- Readiness checks: `info.isBootstrapped` for chain P
  (`01_bootstrap_primary_network.sh:154-158`) never returns true on a follow-only node;
  use `/ext/health/readiness` (see health semantics above). `verify_pchain_mesh`
  (`01_bootstrap_primary_network.sh:178-208`) is meaningless on Fuji, replace with a
  "P height within X of Fuji tip" probe against the RPC tier.
- Comments/docs that say the pool "re-bootstraps the primary network from the dev
  machine" (`cmd/reconcile/remote.go:507-513`, `cmd/reconcile/restore.go:203`) need the
  story updated; behavior itself (wipe L1 data, state-sync L1) is unchanged.
- Staking keys `staking/l1/1-5` (the local primaries' identities, consumed via
  `node_id_for_l1_index` for indexes 1..5) become unused; keys 6+ (L1 validators, spare,
  RPCs) are untouched, registration stays by committed NodeID
  (`cmd/create-l1/main.go:218-256`).

### 4. (M) Firewall / security groups

Current terraform (`terraform-aws-untested/main.tf`) is devnet-shaped: avax ports open
to `0.0.0.0/0` on the control box (`main.tf:100-106`) and blanket egress
(`main.tf:110-116`). Target:

- Validator SG: ingress 9651 from RPC + validator SGs only; egress ONLY to RPC machines
  (9651) and intra-DC NTP/DNS. No default route needed for avalanchego itself.
  Mind the two indirect external dependencies that break under total isolation:
  public-ip discovery via curl (item 1) and time sync (proposerVM windows care about
  clock skew; point chrony at the RPC boxes or the DC NTP).
- RPC SG: egress rule allowing exactly ONE destination, `<fuji-node-ip>:9651/tcp`
  (avalanche p2p is TLS over TCP on the staking port, satisfying "p2p, not HTTP").
  Ingress: 9651 from validator SG, 9652 (HTTP RPC) from control box / bombard host,
  metrics from the monitoring host.
- Note the wiki gotcha `docker_embedded_dns_resolver_bypasses_egress_iptables_firewall`
  if the fleet ever moves to containers; on plain EC2 SGs this does not apply.
- The pinned public Fuji peer is a single point of failure for the WHOLE fleet's
  P-chain freshness. Consider allowing two upstream Fuji peers (still p2p-only) for the
  RPC tier; topology says one TCP, so flagging the tradeoff rather than deciding here.

### 5. (M) Initial Fuji P-chain sync (one-time ops cost, ordering)

- The P-chain has no state sync: follow-only does a FULL block fetch + execute of Fuji's
  P-chain history through the bootstrap path (`bootstrapper.go:407-459`). That is
  millions of blocks through one upstream peer: expect hours on first boot, per node.
- Order: bring RPC machines up first, let them reach the Fuji tip, then start
  validators (which also need the RPC beacons connected to clear the 75% startup latch,
  `chains/manager.go:1355`).
- Cheap accelerator: sync ONE RPC node, then copy its P-chain db to the other machines
  before first start (all nodes are identical followers; the db is identity-free).
  Worth scripting if we expect to re-provision often.

### 6. (S) Monitoring / Grafana

- No dashboard has any P-chain panel today: zero `avalanche_P_*` references in
  `monitoring/dashboards/*.json` (checked all three). Add a P-chain row: per-node
  P last-accepted height, height lag vs the RPC tier (two-hop lag), peer count,
  and bootstrap-fetch rate during initial sync.
- Scrape config needs no structural change: `04_monitoring.sh:85-101` already targets
  every pool node from `reconcile endpoints`. The control box's 5 local primaries were
  never scraped, so nothing to remove there.

### 7. (S) Sybil settings: nothing to flip, keep it ON

- Sybil protection is ALREADY on everywhere: no `sybil-protection-enabled` flag exists
  anywhere in the repo (repo-wide grep), so all nodes run the default (enabled), and
  the docs codify it (`docs/throughput-tuning-and-benchmarks.md:58-66`, "Sybil
  protection ON everywhere", "the tracker MUST run sybil-ON").
- On Fuji this stays exactly as is. Follow-only and partial-sync do not interact with
  sybil protection; our nodes are non-validators of the primary network and validators
  only of our own L1 (registered via the conversion tx, `cmd/create-l1/main.go:159-164`).
- Action item is purely negative: never reintroduce `sybil-protection-enabled=false`
  (see wiki: `why_bombard_the_non_validating_rpc_tracker_not_the_validator_and_it_must_be_sybil_on`).

### 8. (S) Bombard funding: UNCHANGED, confirmed

- Bombard spends the EWOQ key (`cmd/bombard/main.go:51-52`) on OUR L1's C-chain
  (EVM chain), whose genesis we author: `genesis.json:17-18` allocates to
  `8db97C7cEcE249c2b98bDC0226Cc4C2A57BF52FC` (the EWOQ address), and create-l1 passes
  that same `genesis.json` into `IssueCreateChainTx` (`cmd/create-l1/main.go:138-144`).
  The L1 genesis is ours regardless of which P-chain anchors the L1, so bombard funding
  needs NO change for Fuji. (Fuji AVAX never touches the L1's EVM token.)
- Endpoint selection also unchanged: bombard targets the RPC role via
  `reconcile endpoints` (`05_benchmark.sh:48-56`).

## Suggested execution order

1 and 2 in parallel (code), then 4 (terraform, reviewed but not applied), then 3
(script cleanup), then a staged bring-up: RPC tier follow-only against Fuji (verify
height tracks tip), validators follow-only against RPC tier (verify two-hop), create-l1
on Fuji, `03_wipe_and_deploy_l1.sh`, then 5/6 during the first live run.
