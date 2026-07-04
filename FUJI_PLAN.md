# Fuji migration plan: anchor the benchmark L1's P-chain on Fuji testnet

Status: PLAN ONLY. No infra changes. Written 2026-07-04 against this repo tip and
avalanchego `containerman17/fde` @ `084401863ba97267ea95ac25c4f285f183b0045c`
(master + the 6 open PRs incl. #5613 p-chain-follow-only), which the Makefile now pins.
After user review, a full Terraform e2e on Fuji is authorized as the next step; the
execution order at the bottom is written to be runnable as-is.

## Target topology

- Validator machines: ZERO external connectivity, only reach RPC machines in the same DC.
- RPC machines: exactly ONE outgoing TCP (avalanche p2p on 9651, not HTTP) to a public
  Fuji P-chain node. They run `--p-chain-follow-only` (PR #5613) to track Fuji's P-chain.
- Validators get P-chain blocks FROM the RPC machines (second hop), also via follow-only.

All avalanchego citations below are paths in the pinned commit `0844018`.

## KEY QUESTION: can a follow-only node serve P-chain blocks to a downstream follow-only node?

**YES. Two-hop chaining works with stock #5613: confirmed by code analysis AND already
proven end-to-end on real Fuji.**

Empirical proof (2026-07-04 e2e): public Fuji node <- follow-only hop 2 <- follow-only
hop 3, each hop firewalled to its upstream only. All criteria passed; both hops
full-bootstrapped Fuji P (~286k blocks) and tracked an organic new block with ~0-2s lag
per hop. An earlier isolated-bank e2e (2026-06-27/07-03) proved the full
isolated-validators-behind-a-relay architecture including relay-outage survival.
Proven recipes:
`~/dotfiles/wiki/avalanchego/how_to_run_isolated_l1_validators_behind_a_pchain_relay_production_recipe.md`
and `~/dotfiles/wiki/avalanchego/follow_only_node_serves_pchain_to_peers_but_reports_not_bootstrapped.md`.

Code evidence chain:

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

Operational gotchas (from the e2e runs, carry these into scripts/monitoring):

- **Health gating**: on a walled follow-only node the OVERALL `/ext/health` is
  `healthy:false` FOREVER (the P check fails "not connected to enough stake: 0% < 80%",
  no public stake is reachable). Gate on `/ext/health/readiness` (only the
  `bootstrapped` check, which flips true once caught up: the subnet is marked
  bootstrapped before the follow-only early return, `bootstrapper.go:765`, check
  registered at `chains/manager.go:1488-1499`) or on the `bootstrapped` health check
  alone. `info.isBootstrapped("P")` also stays FALSE forever (requires `snow.NormalOp`,
  `chains/manager.go:1476-1485`): never gate or watch on it.
- **`platform.*` HTTP API is gated forever** on follow-only nodes ("API call rejected
  because chain is not done bootstrapping"), while P2P serving works fine. So the RPC
  machines can feed validators but can NOT serve P-chain API calls: anything issuing or
  querying P-chain txs goes against the public Fuji API instead (see create-l1 item).
- **Multi-hop bootstrap is SERIAL per hop**: a follow-only node starts serving
  GetAncestors downstream only after its own fetch+execute completes; a downstream
  node's fetch idles at 0 blocks while the middle hop is still executing, then resumes
  by itself. Budget hop-count x per-hop sync time on cold starts.
- **Startup latch**: bootstrapping only starts once >=75% of BEACON weight is connected
  (`chains/manager.go:1355`, `(3*bootstrapWeight+3)/4`). With 2 equal-weight RPC beacons
  a validator needs BOTH connected at (re)start.
- **Latency**: each hop polls every 2s (`bootstrapper.go:40`); measured ~0-2s lag per
  hop. Fine for L1 anchoring, irrelevant to L1 block latency: the e2e also proved the
  L1 keeps mining through a full relay outage (proposerVM uses the last-accepted
  P height the validators already hold).

## KEY POLICY: secrets split by blast radius (user decision)

Rule: a credential whose compromise only affects OUR OWN L1's EVM state may stay
committed; anything that touches Fuji's public P-chain, or that identifies a validator
on it, is generated per deploy and NEVER committed. On Fuji the ConvertSubnetToL1
validationIDs bind our NodeIDs on a public chain, so a committed staking key equals
validator impersonation.

### Inventory of every key/cert currently committed (git-tracked), with classification

| Path | What it is | Classification |
|---|---|---|
| `staking/l1/1..5/{staker.crt,staker.key,signer.key}` (15 files) | Local devnet P-chain primaries' identities | DELETE: role ceases to exist on Fuji (no local primary network) |
| `staking/l1/6..20/{staker.crt,staker.key,signer.key}` (45 files) | L1 validator / spare / RPC node identities (6-8 validators, 9 spare, 10 rpc, 11-20 two-site set per `03_wipe_and_deploy_l1.sh:6` and `.env.example`) | GENERATE per deploy, never committed: these NodeIDs get bound as validationIDs on Fuji's public P-chain |
| `staking/node-ids.env` | NodeID list derived from the certs (consumed by `_common.sh:128-141`, `cmd/create-l1/main.go:210-214`) | Becomes part of the GENERATED manifest (non-secret content, but derived from generated keys, so generated + gitignored with them) |
| `genesis.json` EWOQ alloc (`genesis.json:17-18`, addr `8db97C...F52FC`) | Pre-funded balance on OUR L1's EVM chain | KEEP committed: blast radius is our L1 only |
| `cmd/bombard/main.go:51-52` EWOQ private key constant | Bombard's spend key on OUR L1 | KEEP committed: same, our L1 only |
| Fuji P-chain fund/fee wallet + L1 owner key | Do not exist in the repo yet | GENERATE (gitignored), never committed; see gen-secrets item |

Not keys but checked: `.env.example` / `.env.local.example` contain only IPs/paths
(no secrets); `SUBNET_EVM_ID` (Makefile:13, `_common.sh:113`) is a public VM id.

## Concrete changes, ranked by effort (largest first)

### 1. (L) Node launch rework in `cmd/reconcile` (per-role flags, Fuji network)

The single launch template at `cmd/reconcile/remote.go:440-470` currently starts every
pool node identically with `--network-id=local` (`remote.go:456`) and one shared
bootstrap list (`remote.go:465-466`, fed from `PCHAIN_BOOTSTRAP_IPS/IDS` env,
`remote.go:135-136`). Needed:

- `--network-id=fuji` everywhere (built-in Fuji genesis, no genesis flags needed).
- `--partial-sync-primary-network=true` everywhere so nodes sync ONLY the P-chain and
  skip Fuji's X/C chains (`config/flags.go:273`).
- `--p-chain-follow-only=true` on BOTH tiers (`config/keys.go:150`):
  - RPC tier: follow-only against the public Fuji peer (the two-hop e2e proved
    follow-only <- follow-only works; the 07-03 recipe's stock-partial-sync relay in
    NormalOp is a valid alternative for the RPC tier, but follow-only is what this plan
    targets and what was e2e-proven on 07-04).
  - INSIDE validators: follow-only is REQUIRED, not optional. A prior finding showed a
    stock partial-sync node behind a single peer froze after the bootstrap-to-consensus
    handoff (it enters NormalOp and then needs consensus polling that a single
    non-validator upstream cannot provide). Validators run
    `--p-chain-follow-only --partial-sync-primary-network` with the RPC tier as their
    SOLE P-chain beacons.
- Per-role bootstrap lists (breaks the "identity-agnostic identical start script"
  assumption, hence the L):
  - RPC role: `--bootstrap-ips=18.192.93.241:9651
    --bootstrap-ids=NodeID-2m38qc95mhHXtrhjyGbe7r2NhniqHHJRB` (the ONE allowed
    outgoing TCP; follow-only re-syncs from exactly these peers,
    `config/flags.go:274`). UPSTREAM CHOICE (user decision): no extra machine; the
    upstream is one of the Fuji bootstrap peers hardcoded in avalanchego's
    `genesis/bootstrappers.json` (Ava Labs-operated, implicitly trusted by every
    stock build). Chosen entry: the first Fuji bootstrapper in the pinned commit,
    `genesis/bootstrappers.json:100-103`. The NodeID is pinned by the TLS
    handshake, so a hijacked IP cannot impersonate the peer.
  - Validator/spare role: `--bootstrap-ips/-ids` = our RPC machines (DC-internal IPs,
    port 9651).
- **Sibling peering** (new, from the proven recipe): isolated validators must reach each
  other for L1 consensus, but signed-IP gossip never relays their private IPs. Seed
  siblings via `--state-sync-ids/--state-sync-ips`, NOT `--bootstrap-ids` (adding
  fresh siblings as bootstrap beacons caps the P-chain frontier on fresh DBs). Add
  `--network-allow-private-ips=true` on the isolated tier.
- **Throttling** (from the proven recipe): raise inbound AND outbound at-large +
  bandwidth throttles on BOTH sides, or large Ancestors messages to non-validators get
  dropped and single-beacon bootstrap stalls. `node-config.json` already raises the
  inbound side (`node-config.json:2-5`); add the outbound equivalents and verify under
  simultaneous multi-client bootstrap.
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

- Point the wallet at the public Fuji API (`https://api.avax-test.network`), run
  create-l1 once from the control box. This is MANDATORY, not just convenient: our own
  RPC tier is follow-only, so its `platform.*` API is gated forever (see gotchas above).
- Replace `genesis.EWOQKey` with the generated Fuji wallet key (from gen-secrets, item
  3). Alternative that avoids touching create-l1's wallet code at all: reuse the proven
  platform-cli local-key flow (`~/go/bin/platform-cli`, keystore key `default`; recipe
  in `~/dotfiles/wiki/avalanchego/how_to_create_a_fuji_l1_with_platform_cli.md`,
  worked Fuji example 2026-06-27: subnet + chain + convert for ~0.3 AVAX total with
  3 x 0.1 AVAX validator balances). create-l1's committed-key PoP computation
  (`main.go:216-256`) maps 1:1 onto platform-cli's manual convert mode.
- Fund it from the Fuji faucet (core.app faucet, 2 AVAX per request, coupons give
  more). Budget: tx fees are trivial; the real cost is per-validator continuous-fee
  `Balance`, currently 1 AVAX each (`main.go:253`, `units.Avax`). Measured drain: the
  0.1 AVAX default lasts ~5-6 days on Fuji, so 1 AVAX per validator is ~50-60 days.
  Balance 0 makes the validator INACTIVE and takes the whole L1 down (observed in e2e:
  empty NodeID in L1 health). Top up with `IncreaseL1ValidatorBalanceTx`: anyone can
  fund it, no owner auth needed (platform-cli `l1 add-balance`).
- Owner gotcha to decide at conversion time: a manual convert leaves each validator's
  `remainingBalanceOwner`/`deactivationOwner` empty, which platform-cli cannot later
  mutate (cannot disable/remove validators). Our failover flow never changes the
  P-chain validator set (spares assume committed identities by key swap, P-chain set
  untouched), so empty owners are acceptable for the benchmark; set explicit owners
  only if we ever want on-chain set churn.
- `Weight: units.Schmeckle` (`main.go:252`) is fine on Fuji (weights are L1-internal).
- The 5s "wait for chain" sleep (`main.go:171`) becomes a real poll: Fuji P-chain
  acceptance is not instant-local.

### 3. (M) NEW: `cmd/gen-secrets` (`make secrets`) + remove committed keys from git

A tiny Go binary that generates ALL generate-class secrets into gitignored paths, and a
non-secret manifest that scripts read instead of hardcoded values. Everything else in
the flow stays byte-identical.

- Generates, per deploy:
  - `staking/l1/<idx>/{staker.crt,staker.key,signer.key}` for every identity the
    configured topology needs (reuse the existing generator: `cmd/genstaking` already
    exists and is what produced the current keys, see `_common.sh:195-220`).
  - The Fuji P-chain fund/fee wallet + L1 owner key (one secp256k1 key is enough for
    both roles for the benchmark) into e.g. `staking/fuji-wallet.key` (gitignored).
    If we go the platform-cli route, gen-secrets can instead emit/import the key into
    the platform-cli keystore (`platform-cli keys import`, key-name `default`).
- Emits the manifest (non-secret): keep the EXISTING `staking/node-ids.env` path and
  format (L1_<idx>_NODE_ID=...), extended with BLS pubkey/PoP per identity (needed for
  a platform-cli manual convert) and the wallet's P-chain address. This is the whole
  "scripts read the manifest" design: every consumer already reads that file rather
  than hardcoding NodeIDs (`_common.sh:128-141` `node_id_for_l1_index`,
  `cmd/create-l1/main.go:210-214`), so pointing gen-secrets at the same path means
  ZERO script changes. Repo-wide grep confirms no NodeID literals outside
  `staking/node-ids.env`.
- Git surgery: `git rm -r staking/l1 staking/node-ids.env`, add both to `.gitignore`,
  update `ensure_staking_keys`'s remedy text (`_common.sh:214-217`) to say
  `make secrets`. The `pack`/`rpm` targets already ship `staking/` from the working
  tree (`Makefile:94`), so generated keys flow into the kit unchanged.
- Note: the CURRENT committed keys 6-20 must be treated as burned for Fuji purposes
  (they are public in git history); devnet reuse until the switch is fine.

### 4. (M) Retire the local devnet P-chain and its assumptions in scripts

- `01_bootstrap_primary_network.sh` (the whole script) exists only to run 5 local
  `--network-id=local` primaries on the control box (`01_bootstrap_primary_network.sh:116`,
  count at `:13`). Replaced by: nothing (Fuji IS the primary network). Its slot in the
  run order becomes "wait for RPC tier to catch up to Fuji P tip".
- `06_cleanup.sh:11` (kills the local P-chain validators): drop that section.
- `_common.sh` P-chain helpers (`PCHAIN_NODE_COUNT`/ports at `_common.sh:116-119`,
  `pchain_node_ids_csv` `:153-160`, `pchain_public_ip` `:162-167`,
  `pchain_public_staking_ips_csv` `:169-177`): delete or repoint at the RPC tier.
- Readiness checks: replace every `info.isBootstrapped` gate
  (`01_bootstrap_primary_network.sh:154-158`) with `/ext/health/readiness`; on walled
  follow-only nodes overall `/ext/health` is false forever (see gotchas), so no script
  or watcher may gate on overall health either. reconcile's `consensusHealth` is safe
  as-is: it parses the L1 chain's own check out of the body and ignores overall status
  (`cmd/reconcile/health.go:109-121`).
- `verify_pchain_mesh` (`01_bootstrap_primary_network.sh:178-208`) is meaningless on
  Fuji; replace with a "P height within X of Fuji tip" probe against the RPC tier.
- Comments/docs that say the pool "re-bootstraps the primary network from the dev
  machine" (`cmd/reconcile/remote.go:507-513`, `cmd/reconcile/restore.go:203`) need the
  story updated; behavior itself (wipe L1 data, state-sync L1) is unchanged.

### 5. (M) Firewall / security groups

Current terraform (`terraform-aws-untested/main.tf`) is devnet-shaped: avax ports open
to `0.0.0.0/0` on the control box (`main.tf:100-106`) and blanket egress
(`main.tf:110-116`). Target:

- Validator SG: ingress 9651 from RPC + validator SGs only; egress ONLY to RPC machines
  (9651), sibling validators (9651, for the L1 consensus mesh), and intra-DC NTP/DNS.
  Mind the two indirect external dependencies that break under total isolation:
  public-ip discovery via curl (item 1) and time sync (proposerVM windows care about
  clock skew; point chrony at the RPC boxes or the DC NTP).
- RPC SG: egress rule allowing exactly ONE destination, `18.192.93.241:9651/tcp`
  (avalanche p2p is TLS over TCP on the staking port, satisfying "p2p, not HTTP").
  Annotate the rule with the expected peer identity so the pairing is auditable:
  `NodeID-2m38qc95mhHXtrhjyGbe7r2NhniqHHJRB` (first Fuji entry in the pinned
  commit's `genesis/bootstrappers.json:100-103`; identity enforced by the TLS
  handshake, the SG rule only constrains the destination). Ingress: 9651 from
  validator SG, 9652 (HTTP RPC) from control box / bombard host, metrics from the
  monitoring host.
- Runbook caveat (a), trust: follow-only takes the upstream's frontier at face value
  (the beacon frontier IS what gets fetched and executed,
  `bootstrapper.go:407-418`), so choosing the upstream IS the trust decision. An
  Ava Labs bootstrap peer is the same party every stock avalanchego build already
  trusts to join the network; anything less trusted as upstream would let it feed
  our whole fleet a wrong P-chain view.
- Runbook caveat (b), rotation: the hardcoded bootstrapper IPs can rotate between
  avalanchego releases. On EVERY avalanchego upgrade (any `AVALANCHEGO_COMMIT` bump
  in the Makefile), re-check `genesis/bootstrappers.json` in the new commit and
  update the SG egress rule + the RPC tier's `--bootstrap-ips/-ids` together; a
  stale IP fails closed (RPC tier stops tracking, validators freeze on the last
  P height, the L1 keeps mining per the relay-outage e2e).
- Note the wiki gotcha `docker_embedded_dns_resolver_bypasses_egress_iptables_firewall`
  if the fleet ever moves to containers (it bit the e2e); on plain EC2 SGs with no
  default route it does not apply.
- The pinned public Fuji peer is a single point of failure for the WHOLE fleet's
  P-chain freshness. The e2e showed this is survivable (L1 kept mining through a relay
  outage and recovered on return), but consider allowing a second upstream Fuji peer
  (still p2p-only) for the RPC tier; topology says one TCP, so flagging the tradeoff
  rather than deciding here.

### 6. (S) Initial Fuji P-chain sync (one-time ops cost, ordering)

- The P-chain has no state sync: follow-only does a FULL block fetch + execute of
  Fuji's P-chain history through the bootstrap path (`bootstrapper.go:407-459`).
  Measured on the e2e: ~286k blocks, execute phase ~6m15s per hop, so expect
  minutes-scale per hop (not hours), SERIAL per hop (hop 3 idles until hop 2 finishes
  executing, then resumes on its own).
- Order: bring RPC machines up first, let them reach the Fuji tip, then start
  validators (which also need the RPC beacons connected to clear the 75% startup latch,
  `chains/manager.go:1355`).
- Optional accelerator (probably unnecessary at minutes-scale): sync ONE RPC node, then
  copy its identity-free P-chain db to the other machines before first start.

### 7. (S) Monitoring / Grafana

- No dashboard has any P-chain panel today: zero `avalanche_P_*` references in
  `monitoring/dashboards/*.json` (checked all three). Add a P-chain row: per-node
  P last-accepted height, height lag vs the RPC tier (two-hop lag), peer count,
  and bootstrap-fetch rate during initial sync.
- Any Grafana/alert gate on node health must use the readiness/bootstrapped signals,
  never overall `/ext/health` (false forever on the walled tier, see gotchas).
- Scrape config needs no structural change: `04_monitoring.sh:85-101` already targets
  every pool node from `reconcile endpoints`. The control box's 5 local primaries were
  never scraped, so nothing to remove there.

### 8. (S) Sybil settings: nothing to flip, keep it ON

- Sybil protection is ALREADY on everywhere: no `sybil-protection-enabled` flag exists
  anywhere in the repo (repo-wide grep), so all nodes run the default (enabled), and
  the docs codify it (`docs/throughput-tuning-and-benchmarks.md:58-66`).
- On Fuji this stays exactly as is. Follow-only and partial-sync do not interact with
  sybil protection; our nodes are non-validators of the primary network and validators
  only of our own L1 (registered via the conversion tx, `cmd/create-l1/main.go:159-164`).
- Action item is purely negative: never reintroduce `sybil-protection-enabled=false`
  (see wiki: `why_bombard_the_non_validating_rpc_tracker_not_the_validator_and_it_must_be_sybil_on`).

### 9. (S) Bombard funding: UNCHANGED, confirmed

- Bombard spends the EWOQ key (`cmd/bombard/main.go:51-52`) on OUR L1's C-chain
  (EVM chain), whose genesis we author: `genesis.json:17-18` allocates to
  `8db97C7cEcE249c2b98bDC0226Cc4C2A57BF52FC` (the EWOQ address), and create-l1 passes
  that same `genesis.json` into `IssueCreateChainTx` (`cmd/create-l1/main.go:138-144`).
  The L1 genesis is ours regardless of which P-chain anchors the L1, so bombard funding
  needs NO change for Fuji, per the key policy above (blast radius = our L1 only).
- Endpoint selection also unchanged: bombard targets the RPC role via
  `reconcile endpoints` (`05_benchmark.sh:48-56`).

## Execution order (runnable as-is once items above land)

1. **secrets**: `make secrets` (item 3) generates staking identities + Fuji wallet,
   writes the manifest (`staking/node-ids.env` + wallet address). No network access.
2. **fund**: faucet-fund the wallet's P-chain address (item 2 budget: fees + N x 1 AVAX
   validator balances; ~5 AVAX covers a 3-validator run for weeks). Verify with
   `platform-cli wallet balance` or `platform.getBalance` against the public API.
3. **infra**: terraform apply of the Fuji-shaped SGs + fleet (item 5). Authorized only
   after user review of this plan.
4. **RPC follow-only sync**: deploy + start the RPC tier (item 1 flags) against the
   chosen Fuji bootstrap peer (`NodeID-2m38qc...` @ `18.192.93.241:9651`); gate on
   `/ext/health/readiness` and P height ~= Fuji tip.
5. **validators**: start validator/spare tier (follow-only, beacons = RPC tier,
   sibling seeding via state-sync-ids); serial hop, budget ~10 min; gate on readiness.
   First boot runs without `--track-subnets` (the subnet does not exist yet); this
   pre-syncs the P-chain so step 7's restart is fast.
6. **create-l1 on Fuji**: from the control box against `api.avax-test.network`
   (create-l1 with the generated wallet, or platform-cli manual convert). Writes
   `network.env` (SUBNET_ID/CHAIN_ID) exactly as today.
7. **deploy L1**: `./03_wipe_and_deploy_l1.sh` (reconcile fresh) restarts the pool
   with `--track-subnets=<subnetID>` as the flow already does; validators' pre-synced
   P-chain db carries over, only the L1 state-syncs/bootstraps.
8. **bombard**: `./05_benchmark.sh` unchanged (item 9).
