# Bombard Design Notes

This spec captures the current implemented design for:

```sh
./bombard
```

The goal is to drive sustained transaction load against the L1 from the
benchmark host and report throughput and latency, in either continuous TUI
mode or one-shot timed mode.

The native-transfer benchmark below is V1 and should stay aligned with the
current implementation. Future contract/state workload ideas belong in the
V2 draft section near the end of this file until they are implemented.

## Re-Entry Notes For Future Agents

This spec supersedes the older flag-heavy `cmd/bombard` design. If the code
still reads inventory files, accepts many flags, or prints periodic stats
to stdout while running, refactor it to this design before adding anything
else.

The core load-generation algorithm is intentionally unchanged from the
reference implementation. Only the input shape, the endpoint failover
wrapper, and the results display are new.

Reference implementation for the unchanged core:

- `/home/ubuntu/avalanche-benchmark-bombard-ws-worker-pool/local/cmd/bombard/main.go`
- `/home/ubuntu/avalanche-benchmark-bombard-ws-worker-pool/local/cmd/bombard/keys.go`
- `/home/ubuntu/avalanche-benchmark-bombard-ws-worker-pool/local/cmd/bombard/watcher.go`
- `/home/ubuntu/avalanche-benchmark-bombard-ws-worker-pool/local/cmd/bombard/wspool.go`

The core that must be preserved verbatim:

- Deterministic worker-wallet derivation from a single seed key.
- Funder = the well-known local-network genesis-allocated key
  (`0x8db97C7cEcE249c2b98bDC0226Cc4C2A57BF52FC` and its known private key).
- Funding model: per-worker balance check, fund only those below threshold,
  one funding tx per under-funded worker.
- Worker pool sizing: `numWorkers = tickerTime / workerDelay`, with
  `tickerTime = 90s` and `workerDelay = 50ms` (so 1800 workers).
- Per-batch math: `batchSize = TPS * workerDelay / 1s` (so 200 at 4000 TPS).
  In continuous mode, workers read the current target TPS each round before
  computing `batchSize`; in one-shot mode the target stays fixed.
- WebSocket pool for tx submission and block scanning. HTTP is used only for
  endpoint health probes.
- Confirmation discovery: scan incoming blocks for our submitted tx hashes;
  do not poll `eth_getTransactionReceipt` per tx.
- Per-tx latency tracking: send / mined / confirm / total durations. This
  project reports percentiles over the last 10 seconds of landed txs, not a
  fixed-size sample ring.

The default starting TPS target stays `4000`, but benchmark runs must be
able to start at another TPS target.

The default WebSocket connection count stays `runtime.NumCPU() * 10`.

## Step Goals

`bombard` must:

- accept exactly three flags and no others:
  - `--rpcs=URL1,URL2,...` (required, comma-separated chain RPC URLs);
  - `--time=DURATION` (optional; when present, runs one-shot timed mode);
  - `--starting-tps=N` (optional; default `4000`);
- run continuous TUI mode when `--time` is omitted;
- run one-shot timed mode when `--time` is set;
- use the same load-generation, probing, and failover mechanics in both
  modes; the mode only changes rendering/output and stop condition;
- pick the first URL in `--rpcs` that responds to `eth_chainId` as the
  active node;
- use the active node for both tx submission and block scanning through
  WebSocket connections derived by replacing the URL scheme with `ws://` and
  keeping the same host+path;
- submit to exactly one active node at a time; `--rpcs` is for failover and
  status, not load balancing;
- probe every URL in `--rpcs` once a second with `eth_chainId` and label
  each `alive` or `DOWN`;
- on active-node failure (probe fails, or submit fails, or WS disconnects),
  re-scan `--rpcs` from the start and switch to the first URL currently
  alive;
- fund worker wallets before starting measured load (in one-shot mode, the
  `--time` countdown starts only after funding completes);
- in continuous mode, render the TUI defined below and refresh once a
  second;
- in continuous mode, show setup/funding progress before measured load
  starts; a simple progress bar is preferred if the TUI library makes it
  easy;
- in continuous mode, let the operator raise/lower target TPS while the
  run is active;
- in one-shot mode, print the final result block defined below exactly once,
  at the end of the run, and exit;
- handle Ctrl-C cleanly: continuous mode exits without printing the table,
  one-shot mode prints what it has and exits non-zero.

## Implementation Progress

- [x] Replace old flags with `--rpcs`, `--time`, and `--starting-tps`.
- [x] Preserve the native-transfer core: deterministic workers, funding,
  fixed worker pool shape, WS submission pool, block scanning, and per-tx
  latency tracking.
- [x] Implement one-shot mode first: quiet setup, timed run, final
  percentile table plus counters only.
- [x] Add active URL probing and failover across the `--rpcs` list.
- [x] Add continuous TUI mode with node status, plain TPS/latency text, and
  live target TPS controls.
- [x] Add latest observed block number to the continuous TUI.
- [x] Add/update operator wrapper script that reads `.env` and
  `runtime-data/l1.env`, assembles RPC URLs, and SSH-runs bombard on the
  benchmark host.
- [x] Verify first one-shot slice with `go test ./...`, `make bombard`, and
  one short live one-shot run against the current L1.
- [x] Verify final implementation with `go test ./...`, `make`, and one short live one-shot run
  against the current L1.

Current verified one-shot smoke test:

```sh
./bin/bombard \
  --rpcs=http://50.18.34.131:9650/ext/bc/2czFCRf5YdYrd4UdGuPa1jiMF4iKajdm74UaFmsUZSeEeSgEAs/rpc \
  --time=1s \
  --starting-tps=100
```

Result: `submitted=100 landed=100 timeouts=0 pending=0`, Total P50 `14ms`,
P95 `16ms`, P99 `18ms`. Stdout contained only the final result block;
stderr was empty.

Wrapper smoke test also passed:

```sh
./scripts/04_bombard.sh --time 1s --starting-tps 100
```

Result: `submitted=100 landed=100 timeouts=0 pending=0`, Total P50 `14ms`,
P95 `19ms`, P99 `20ms`.

Final verification:

- `go test ./...`: passed.
- `bash -n scripts/*.sh`: passed.
- `make`: passed.
- Rebuilt `bombard`, copied only that binary to the benchmark host, and
  reran `./scripts/04_bombard.sh --time 1s --starting-tps 100`:
  `submitted=100 landed=100 timeouts=0 pending=0`, Total P50 `13ms`,
  P95 `15ms`, P99 `22ms`.
- Continuous TUI smoke test with `./scripts/04_bombard.sh --starting-tps 100`
  started successfully, showed live plain-text TPS/latency/node status, and
  exited cleanly on `q`.
- Startup active-node selection smoke test with a dead first RPC and live
  second RPC passed: one-shot still produced `submitted=100 landed=100
  timeouts=0 pending=0`.

## V1 Non-Goals

In the current native-transfer V1, `bombard` must NOT:

- read `.env`, `runtime-data/l1.env`, `staking/node-ids.env`, or any other
  inventory file;
- learn which endpoint is a validator vs RPC node;
- look up subnet IDs, chain IDs, or NodeIDs;
- SSH anywhere;
- start, stop, or restart any AvalancheGo process;
- mutate the P-Chain or L1 validator set;
- accept any flag other than `--rpcs`, `--time`, and `--starting-tps`;
- expose `--ws-conns`, `--erc20`, `--confirm-source`, `--watch-rpc`,
  `--data-dir`, or any other override that the reference implementation has.
- send ERC20-like contract calls; V1 sends native transfers only.

Anything that lives outside the core load-generation algorithm and outside
the display layer is the operator's responsibility, not bombard's.

## Inputs

The only inputs are the three flags. Example invocations:

```sh
./bombard --rpcs=http://10.0.0.1:9650/ext/bc/abc.../rpc,http://10.0.0.2:9650/ext/bc/abc.../rpc

./bombard --rpcs=http://10.0.0.1:9650/ext/bc/abc.../rpc,http://10.0.0.2:9650/ext/bc/abc.../rpc --time=30s

./bombard --rpcs=http://10.0.0.1:9650/ext/bc/abc.../rpc,http://10.0.0.2:9650/ext/bc/abc.../rpc --time=30s --starting-tps=1000
```

URLs are opaque to bombard. Each URL must be a full chain RPC endpoint.
The WebSocket URL for tx submission and block scanning is derived by
replacing `http` with `ws` and keeping the same path. No separate WS flag.

## Active-Node Failover

State: one `activeIndex` into the `--rpcs` slice.

At startup:

- probe each URL in order;
- the first URL that responds becomes `activeIndex`;
- if none respond, exit with a clear error.

While running, once per second:

- probe every URL with `eth_chainId`, regardless of which is active;
- record each URL's last result as `alive` or `DOWN`.

Failover triggers:

- a probe for the active URL fails;
- a tx submission to the active URL returns a WebSocket transport error;
- the watcher cannot dial or query the active URL over WebSocket.

On any trigger, scan `--rpcs` from index 0 and set `activeIndex` to the
first URL currently `alive`. Do not advance round-robin from the current
position. Do not prefer the previous active. Just take the first live one.

If no URL is alive after a previously successful startup, keep retrying
probes every second and do not exit. The TUI shows everything `DOWN`.
One-shot mode keeps counting time and reports whatever landed by the end.
Current implementation detail: if the active endpoint fails and later the
same URL is the only endpoint that recovers, automatic pool rebuild is not
guaranteed until a failover to a different live URL is possible. This edge
case needs a code fix before claiming full all-down/all-up recovery.

This only applies after startup. At startup, if no URL is alive, exit with
a clear error.

Workers keep attempting submissions against the current active endpoint
even while failover/health is bad. This is deliberate: overload and outage
behavior should be visible as pending submissions, errors, timeouts, and
latency rather than hidden by a polite pause.

When the active URL changes, tear down the WS pool against the old URL
and rebuild it against the new URL. Workers use the new WebSocket pool on
their next send round.

## Continuous TUI Layout

The TUI is rendered once a second. It is intentionally plain text; do not
add charts, sparklines, or box drawing for the live view. Layout:

```
TPS: actual <current> target <target> [-] [+]
Block: latest <n>
Latency(10s): P50 <n> ms  P95 <n> ms
Counts: sub <n> land <n> to <n> pend <n> fail <n>

> node 1  <host:port>  active alive
  node 2  <host:port>         alive
  node 3  <host:port>         DOWN
...

+/- adjust target TPS   q or Ctrl-C exits
```

Specifics:

- TPS displays landed-per-second over the last 1-second bucket;
- target TPS starts from `--starting-tps`;
- `--starting-tps` must be within `100..6000` or bombard exits before
  setup/funding;
- in continuous mode, `+` and `-` keys adjust target TPS while the run is
  active;
- TPS adjustment step is `100` while target TPS is below `1000`, and `500`
  at `1000` or above;
- target TPS is clamped to the inclusive range `100..6000`;
- the TUI may also handle mouse clicks on `[-]` and `[+]` when the
  operator's terminal and SSH session support mouse events, but keyboard
  controls are the required path;
- P50 and P95 are computed over landed txs observed in the last 10 seconds,
  refreshed each second;
- changing target TPS in continuous mode does not reset or clear the
  10-second latency window;
- latest block is the newest block number observed by the block watcher on
  the active endpoint, or `-` before the watcher has a block;
- Node list shows every URL from `--rpcs` in input order. `host:port` is
  extracted from each URL. The `>` cursor marks `activeIndex`. Status is
  `alive` or `DOWN` from the per-second probe;
- No validator/RPC role labels. Bombard does not know which is which;
- Footer counters come from the existing tracker (`submitted`, `landed`,
  `timeouts`, `pending`) plus the endpoint-manager failover counter.

Ctrl-C exits the TUI. No final table. Continuous mode never prints the
one-shot final result block.

## One-Shot Output

When `--time` is set, bombard runs headless and prints exactly one result
block to stdout at the end of the run. The block starts with the existing
reference percentile table:

```
═══════════════════════════════════════════════════════════════════════════════════════════════════════════════
  PERCENTILES (last 10s, samples=<N>, timeouts=<N>, tps=<N>)
═══════════════════════════════════════════════════════════════════════════════════════════════════════════════

  ┌────────────────────┬───────────────┬───────────────┬───────────────┬───────────────┐
  │ Metric             │  Send         │  Mined        │  Confirm      │  Total        │
  ├────────────────────┼───────────────┼───────────────┼───────────────┼───────────────┤
  │ Min                │ ...
  │ Avg                │ ...
  │ Median (P50)       │ ...
  │ P75                │ ...
  │ P90                │ ...
  │ P95                │ ...
  │ P99                │ ...
  │ Max                │ ...
  │ Std Dev            │ ...
  └────────────────────┴───────────────┴───────────────┴───────────────┴───────────────┘
```

Same columns, same rows, same number format as the reference. After the
table, print one final counter line:

```text
FINAL submitted=<N> landed=<N> timeouts=<N> pending=<N>
```

This result block is the only stdout output in one-shot mode. No periodic
`STATS` lines. Setup/funding chatter is suppressed in one-shot mode unless
there is an error.

Exit code is `0` on a clean timed run, non-zero only on hard errors
(e.g. no URLs ever alive, funding could not complete).

## Lifecycle

1. Parse flags. Fail fast if `--rpcs` is empty.
2. Probe every URL. Pick first live as `activeIndex`. Fail if none live.
3. Derive worker wallets. Check balances against the active URL.
4. Fund under-funded workers from the genesis key against the active URL.
   Wait until every funding tx lands.
5. Build the WS pool against the active URL.
6. Start workers. Start the watcher against the active URL.
7. Start the per-second probe loop and (in continuous mode) the render loop.
8. In one-shot mode, start the `--time` countdown now (not before funding).
   The countdown includes normal worker startup/staggering. This matches the
   reference behavior: staggered worker starts are part of how the target TPS
   pressure is applied immediately rather than a separate warmup phase.
9. On `--time` expiration: stop workers, drain pending up to a short
   bounded grace period, print the final result block, exit.
10. On Ctrl-C: stop workers immediately. Continuous mode exits silently.
    One-shot mode prints what is available, exits non-zero.

If the active URL dies during funding, exit with an error. Funding failover
is deliberately out of scope because funding is setup, not the measured
benchmark path.

## Operator-Side Wrapper

`bombard` is invoked over SSH from the operator. The wrapper script is
not part of this spec, but its contract is:

- read `.env` for host inventory and `L1_VALIDATOR_COUNT`;
- read `runtime-data/l1.env` for `L1_CHAIN_ID`;
- assemble the chain RPC URLs the operator wants to load against (typically
  the first `L1_VALIDATOR_COUNT` hosts);
- copy the local `./bombard` binary to `<benchmark-work-dir>/bin/bombard`
  before every run, without stopping any AvalancheGo process;
- SSH to the benchmark host and exec `./bin/bombard --rpcs=... [--time=...] [--starting-tps=...]`.

The wrapper is the only piece that knows the inventory. `bombard` itself
never learns it.

## Decided Q&A

### Q1. Should bombard read `.env` or any inventory file?

No. The operator-side wrapper reads inventory and passes URLs in.

### Q2. How are submit and watch endpoints separated?

They are not. A single active URL serves both. WebSocket URL is derived
from the HTTP URL.

### Q3. How is the active node chosen?

First URL in `--rpcs` that responds to `eth_chainId` is active.

### Q4. What triggers a failover?

Probe failure, WebSocket submit transport error, or watcher WebSocket
dial/query failure on the active URL.

### Q5. After failover, does the old active come back automatically?

Not on its own. The active node only changes when the current active dies.
The next failover then picks the first live URL again, which may or may
not be a previously failed node depending on its current probe state.
There is no automatic failback while the current active remains healthy.

### Q6. Should bombard expose `--starting-tps`, `--ws-conns`, `--erc20`, or other
core knobs?

Only `--starting-tps`. Benchmark runs need to vary target load between 1k,
2k, 4k, etc. In continuous mode, target TPS is then adjustable inside the
TUI. Other core knobs stay configured by code constants from the reference
implementation. Adding another flag is a deliberate design choice and must
be justified in this spec.

### Q7. What percentiles get the TUI headline spots?

P50 and P95. The batch table still shows the full ladder.

### Q8. Does the TUI distinguish validators from RPCs?

No. Bombard does not know which is which. Every URL is just `node N`.

### Q9. When does the `--time` countdown start?

After funding completes, not when bombard starts.

### Q10. What happens if no URLs are alive?

At startup, bombard exits with a clear error. After a successful start,
bombard keeps probing every second and stays alive during temporary total
outage. The TUI shows everything `DOWN`. One-shot mode keeps the timer
running and reports whatever landed by the end.

### Q11. What happens if the active URL dies during funding?

Exit with an error. Funding is not the benchmark path, and adding failover
there complicates setup for little value.

### Q12. Can the TUI use a dependency?

Yes. A small terminal UI library is preferred over writing a rendering
engine by hand.

### Q13. Can the TPS buttons handle clicks?

Yes, if the selected TUI library and terminal session support mouse events.
Clicks on `[-]` and `[+]` are nice to have. Keyboard `-` and `+` are
mandatory because they work reliably over SSH.

### Q14. What are the live TPS adjustment rules?

`--starting-tps` outside `100..6000` fails fast. During continuous mode,
`+` and `-` adjust by `100` below `1000` TPS and by `500` at `1000` TPS or
above, clamping target TPS to `100..6000`.

### Q15. Does continuous mode ever print a final table?

No. Continuous mode is visual only and exits cleanly on Ctrl-C.

### Q16. Should bombard pause submissions during endpoint trouble?

No. Keep trying to submit. The benchmark should expose overload/failover
behavior through backlog, errors, timeouts, and latency.

### Q17. Does one-shot `--time` wait until all workers have launched?

No. `--time` starts after funding and includes normal worker startup
staggering. The stagger is part of the reference load-shaping model and
still applies target TPS immediately.

### Q18. Should one-shot mode print setup or funding progress?

No. Suppress setup/funding chatter unless there is an error. Stdout should
be the final result block only.

### Q19. Should continuous mode show funding progress?

Yes. Show setup/funding progress before load starts. Use a simple progress
bar if easy with the selected TUI library; otherwise use a plain progress
line.

### Q20. Does changing target TPS reset latency history?

No. Preserve the current 10-second latency window across target TPS changes.

### Q21. Does one-shot mode have different failover mechanics?

No. One-shot and continuous modes use the same runtime mechanics. The only
differences are UI/output and the timed stop condition.

### Q22. Should one-shot output include failover metadata?

No. One-shot mode is mainly an automation helper for quickly checking max
throughput. Keep output focused on the percentile table and final counters.

### Q23. Does bombard distribute load across all alive RPCs?

No. Submit to exactly one active node at a time through the active
WebSocket pool. The RPC list is for failover and status, not load
balancing.

### Q24. Does bombard fail back to an earlier URL when it recovers?

No. The active node only changes when the current active fails. There is no
automatic failback while the current active remains healthy.

### Q25. Does the TUI need a manual active-node switch key?

No. Failure-only switching is enough for now.

### Q26. Does bombard need ERC20 mode?

No. Native transfers only for now.

### Q27. How is mode selected?

If `--time` is omitted, run continuous TUI mode. If `--time` is present,
run one-shot timed mode.

### Q28. Is `--time=0` valid?

No. If `--time` is present, it must be at least `1s`.

### Q29. What duration syntax does `--time` accept?

Use Go duration syntax only, such as `40s`, `2m`, or `1m30s`. Bare
numbers are invalid.

## V2 Draft: Contract State Workload

This section is a draft for the next benchmark workload. It is not the
current implementation and should not be treated as a contract until the
code catches up.

V2 keeps the same operator shape where possible: one benchmark binary,
continuous mode without `--time`, one-shot mode with `--time`, active-node
failover, 10-second latency percentiles, and the same TUI/result style.

### Workload / Contract Model

V2 adds an ERC-20-ish contract workload that is closer to the expected
client application than native transfers.

The model:

- one real EOA sender submits all benchmark transactions;
- the contract keeps balances for many logical subaccounts;
- each transaction emits a transfer-like event and mutates contract state;
- the contract has an account cap, for example `5_000_000_000`;
- the cap is an addressable-account limit, not a claim that all accounts
  already exist in state;
- state grows naturally as new logical accounts are touched;
- after the benchmark reaches the cap, it continues overwriting existing
  accounts instead of growing forever.

The preferred benchmark call is parameterless:

```solidity
function simulateTransaction() external
```

The contract owns the account-selection logic. On every call, it advances
an internal counter, derives the logical account from the counter modulo
the account cap, updates that account's balance, and emits the event. The
benchmark tool should not generate per-account calldata.

The account key should not be a simple sequential storage key if we are
trying to approximate a large sparse account set. A reasonable default is
to hash the counter and truncate to an address-shaped key, for example:

```solidity
uint256 i = txIndex++;
address account = address(uint160(uint256(keccak256(abi.encode(i % accountCap)))));
```

Sequential account IDs are acceptable only if we deliberately want the
cheapest possible storage-key path. The default should favor more realistic
state access even though hashing adds EVM cost.

Important wording:

- `--accounts=5000000000` means "the workload can address and cycle through
  up to 5B logical subaccounts";
- it does not mean the chain starts with 5B materialized storage entries;
- reported results must distinguish account cap from materialized state
  reached during the run.

### Benchmark Tool Changes

V2 should add the smallest possible interface change: one workload/state
knob.

Tentative V2 flags:

```text
--rpcs=URL1,URL2,...
--time=DURATION
--starting-tps=N
--accounts=N
```

`--accounts` is required for the contract workload and means:

- grow state by touching new logical accounts until `N` accounts have been
  touched;
- after that, keep submitting the same parameterless contract call and let
  the contract wrap around inside the `N` account set;
- do not add separate flags for working-set size, growth ratio, or account
  selection unless a later benchmark result proves they are needed.

Open implementation decision:

- either V2 contract mode is the only mode once implemented, replacing
  native transfers;
- or V2 keeps native transfers as the default and introduces an explicit
  workload selector.

The second option adds another flag and conflicts with the current
"smallest interface" preference, so replacing native transfers may be the
cleaner path if the client workload becomes the only workload that matters.

The benchmark mechanics should otherwise remain familiar:

- same single active RPC/WebSocket endpoint behavior;
- same failover behavior;
- same one-shot and continuous modes;
- same 10-second rolling latency window;
- same latest-block display;
- same target TPS adjustment rules.

The main implementation differences are:

- deploy or locate the benchmark contract before measured load starts;
- fund only the single sender, not 1800 worker wallets, unless workers are
  still needed only as local pacing goroutines behind a single sender;
- manage one sender nonce stream correctly under high TPS;
- submit contract-call transactions to `simulateTransaction()`;
- track total calls submitted/landed/timeouts/pending the same way V1
  tracks native transactions;
- report the configured account cap and, if available, the contract's
  current touched-account count in the TUI and one-shot result.
