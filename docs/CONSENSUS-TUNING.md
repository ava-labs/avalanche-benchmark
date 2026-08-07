# Consensus parameter tuning: manual checks and benchmark runbook

The shipped configuration is **k=60, alphaPreference=31, alphaConfidence=38,
beta=12** (`chains/default/subnet-config.json`). Run everything below from the deployment
root on the control machine.

## 1. Check the live parameters

There are three levels of confidence. The log is authoritative. The file
only shows what the next restart loads.

### a) What the running process loaded (authoritative)

At startup, avalanchego writes its subnet configuration into the
`initializing node` log line. The last occurrence is what the live process
uses:

```bash
ssh -i <key> <user>@<node-ip> \
  'sudo journalctl -u avalanche-benchmark-node-<n> --no-pager \
   | grep -o "\"k\":[0-9]*,\"alphaPreference\":[0-9]*,\"alphaConfidence\":[0-9]*,\"beta\":[0-9]*" \
   | tail -2'
```

On a rootless install there is no journal. Read the same line from the
node's console log under the data directory instead.

Each boot writes two entries. The primary network comes first, the L1
second. The **last line is the L1**. Example output:

```
"k":20,"alphaPreference":15,"alphaConfidence":15,"beta":20   <- primary network default, ignore
"k":60,"alphaPreference":31,"alphaConfidence":38,"beta":12   <- live L1 config
```

`k=20/15/15/beta=20` is the avalanchego `DefaultParameters` value for the
primary network. It has nothing to do with your L1. Repeat the command for
each node to check the full fleet.

### b) What is on disk (what the next restart loads)

```bash
SUBNET=$(grep ^SUBNET_ID= deployment/network.env | cut -d= -f2)
ssh -i <key> <user>@<node-ip> "sudo cat /etc/avalanche-benchmark/<n>/subnets/$SUBNET.json"
```

On a rootless install, the path is `<REMOTE_DIR>/config/<n>/subnets/`.

**Caution: `subnets/` can contain more than one file.** A file from an
older deployment shows stale parameters. It is dead weight when it is not
in `track-subnets`, but it looks real. Always select the file by the
current `SUBNET_ID` from `deployment/network.env`.

### c) The source of truth that you edit

Edit `chains/default/subnet-config.json` in the deployment root on the control machine.
Avalanchego reads it once at startup. A change therefore needs a restart.
It does not need a chain reset.

## 2. Apply a parameter change

The snow alpha values are node-local poll thresholds. They are not
block-validity rules. A rolling restart with mixed alpha values is
therefore safe. `proposerWindowMilliseconds` and
`proposerMillisecondTimestamps` are NOT safe to mix: they must be equal on
every node.

```bash
vim chains/default/subnet-config.json                                  # edit snowParameters
./bin/fleet deploy frozen 1 2 3 4 5 6 7 8 9 10 11 12    # rolling, one node at a time, ~20s each
./bin/fleet status                                      # expect all up, near-equal heights
```

**All stake must be online before you roll.** A node that restarts while
one of the three heavy validators is down cannot complete bootstrap. The
gate is 75% of stake (`(3*bootstrapWeight+3)/4`), and 205000/305000 = 67%
is below it. The node answers HTTP 503 forever, and the deploy stops in
its readiness phase.

Avalanchego `Parameters.Verify()` enforces the valid combinations:
`K/2 < alphaPreference <= alphaConfidence <= K`. At k=60, the
alphaPreference floor is 31.

## 3. Run the benchmark

```bash
./bin/bombard -rps 4000 -duration 60s
```

The live screen is on by default; set `-tui=false` for plain log lines.
The tool discovers all
`role=rpc` nodes from `nodes.ini` and the chain ID from
`deployment/network.env`. It sends every transaction across all of them.
You do not select nodes.

Read the output as follows. `mined / duration` is the real TPS. The
`minedTps` value per line is a noisy one-second sample; do not judge from
one row. `AT-CAP(behind)` means the 2000-nonce in-flight cap and the
latency limit the throughput, not the chain capacity. This is normal in
the one-validator-down case.

### The one-validator-down drill

```bash
./bin/fleet stop 3                    # node 3 = identity c, weight 100000, a heavy validator
sleep 5
./bin/bombard -rps 4000 -duration 60s
./tools/forkcheck.sh                  # ALWAYS after a run
./bin/fleet start 3                   # then wait until status is clean; takes minutes
./bin/fleet status
```

Stop node 1, 2, or 3 for a meaningful test. Nodes 4 to 8 have weight 1000
each. The loss of one is 0.3% of the stake and proves nothing.

### Fork check (after every run, not before)

`./tools/forkcheck.sh` reads `eth_blockNumber` from every main-L1 node. It
takes the minimum and compares the block hash at `minimum - 5` on every
node. One distinct hash means OK.

This check matters because **TPS cannot see a fork**. At alpha 11/11,
three nodes finalized a different block at height 814087 than the other
nine. The benchmark still reported a clean 3582 TPS with 0 rejects and 0
reorgs, because only 2000 of 305000 weight had diverged.

The L1 builds blocks only under load. **Equal static heights on an idle
chain are therefore normal.** The fork signal is heights that differ and
do not converge while the chain is idle.

A forked node cannot replay past the divergence. It stops with
`failed to verify block ... in bootstrapping`. Recover it like this:

```bash
./bin/fleet destroy 4 6 10        # SIGKILL + delete chainData only; the machines stay
./bin/fleet deploy frozen 4 6 10  # state sync brings them onto the majority branch
```

## 4. Measured results (one heavy validator down, 4000 rps, 60s, beta=12)

With one of three heavy validators down, 205000/305000 = 67.21% of the
stake is online. A poll of k returns on average `0.6721 * k` online votes.
The important value is the **alphaConfidence/k ratio** against 0.6721, not
the raw numbers.

| aC/k | config | TPS | p50 | fork |
|---|---|---|---|---|
| 0.55 | k=20 11/11 | 3582 | 300-700ms, 1.7s spikes | **FORKED** |
| 0.60 | k=30 16/18 | 3558 | 250-350ms | none |
| 0.633 | k=30 16/19 | 3704 | 620-830ms | none |
| **0.633** | **k=60 31/38** | **3682** | **344-716ms** | **none (current)** |
| 0.65 | k=60 31/39 | 2960 | 509-678ms | none |
| 0.667 | k=30 16/20 | 1870 | 1.45s, stalls, 22k resubmits | none |

Full fleet, nothing down, at 11/11: 3930 TPS, p50 75ms.

**The throughput cliff is between 0.633 and 0.65.** 0.633 is the tightest
ratio that clears 3000 TPS. We selected k=60 over k=30 at the same ratio
for two reasons. It puts alphaPreference at a lower ratio (31/60 = 0.517
against 16/30 = 0.533). It also halves the sampling variance.

### A higher k does not scale traffic or latency with k

`sendQuery` samples `k` slots **with replacement** into a `bag` (multiset)
for the vote accounting. It sends to `set.Of(vdrIDs...)`, a deduplicated
set:

```go
vdrIDs, _ := e.Validators.Sample(e.Ctx.SubnetID, e.Params.K)  // k slots, with replacement
vdrBag := bag.Of(vdrIDs...)                                   // multiplicities kept for votes
vdrSet := set.Of(vdrIDs...)                                   // DEDUPED for the network send
e.Sender.SendPullQuery(ctx, vdrSet, ...)
```

One poll therefore sends at most one message per distinct validator, for
any k. Each validator sends one Chits reply, and the reply counts with its
sampled multiplicity.

Measured on node 9, full fleet, 4000 rps, per accepted block:

| | k=20 | k=60 | change |
|---|---|---|---|
| chits / block | 15.3 | 21.3 | 1.39x |
| polls / block | 3.94 | 4.64 | 1.18x |
| poll_dur | 6.76ms | 4.92ms | **down** |
| accept_lat | 20.8ms | 17.1ms | **down** |
| rounds/accept | 12.29 | 12.59 | flat |

A tripled k raised the message volume approximately 1.4x, not 3x. The
latency went down. The residual 1.4x comes from the skewed weights. At
k=20, most samples land on the three heavy validators, so few distinct
validators receive a query. At k=60, the samples reach the five light
validators more often. The expected number of distinct recipients only
moves from ~3.3 to ~3.9.

### Why alphaPreference stays at its floor

The two alpha values want opposite things:

- **alphaConfidence: higher is safer.** It is the threshold to accrue
  confidence toward finalization. A high value demands a supermajority
  before a block counts.
- **alphaPreference: lower is safer.** It is the threshold to change your
  preference. A low value makes the nodes adopt the emerging plurality
  quickly. That collapses the metastable ~50/50 split between sibling
  blocks, and this split is what produces forks. A high value makes the
  nodes stubborn and keeps the split alive.

At 11/11, both were one marginal threshold. That is the worst arrangement:
one 55% poll changed the preference and counted toward finalization at the
same time.

### Two separate thresholds, both driven by aC/k

**1. A hard query gate. This one halts the chain.** In
`snow/engine/snowman/engine.go`, `sendQuery` calls
`abortDueToInsufficientConnectedStake` before it samples:

```go
stakeConnectedRatio := e.Config.ConnectedValidators.ConnectedPercent()
minConnectedStakeToQuery := float64(e.Params.AlphaConfidence) / float64(e.Params.K)
if stakeConnectedRatio < minConnectedStakeToQuery { /* drop the query entirely */ }
```

When the connected stake falls below `aC/k`, the node stops issuing
queries, and consensus stops. The node logs this at Debug level with the
reason "insufficient stake". It is easy to miss.

With one heavy validator down, the connected stake is 0.6721. **aC/k must
stay below 0.6721**:

| aC/k | margin | verdict |
|---|---|---|
| 38/60 = 0.633 | 6.0% | current, safe |
| 39/60 = 0.650 | 3.3% | thin |
| 40/60 = 0.667 | 0.8% | razor |
| 41/60 = 0.683 | negative | **hard halt** |
| 14/20 = 0.700 | negative | **hard halt** |

The margin matters because `ConnectedPercent` moves with transient
disconnects. At 38/60, the fleet can additionally lose all five light
validators (0.6557 connected) and still poll.

**2. A health-report threshold. This one is cosmetic.**
`MinPercentConnectedHealthy()` is `0.8 * aC/k + 0.2`. Only
`snow/networking/handler/health.go` reads it. At 38/60, it demands 0.707
connected against the 0.6721 that is available. The nodes therefore report
**unhealthy** while one heavy validator is down, and consensus works. Do
not blame the configuration for this report.

**There is also a statistical effect on top of the gate.** beta=12
requires 12 *consecutive* polls above aC. As aC/k approaches the 0.6721
mean, the probability of that sequence collapses. This is why 40/60 and
20/30 crawl instead of a clean halt: they clear the gate by a small margin
and lose the sequence. Both effects push in the same direction.

## 5. Soak result (2026-08-05) and the open item it leaves

A 60-second run cannot prove the absence of forks, so we ran the soak: 45
minutes at 4000 rps offered, k=60/31/38/12, full fleet, fresh genesis,
data on NVMe. `tools/forkcheck.sh` ran every 5 minutes and once after the
load stopped.

- **No fork.** All ten checks returned one hash. The engine counters
  agree: `reorg_exec +0`, `bad_blocks +0`, over 46992 blocks and
  approximately 9.0M accepted transactions.
- Throughput was not constant. The first ~15 minutes held the full 4000
  TPS at p50 62 to 67ms with zero resubmits. The block cadence then
  degraded in steps: ~2870 mined TPS at minute ~20, ~2020 at minute ~45,
  with p50 near 900ms and the in-flight cap binding. The chain stayed
  self-consistent through the slowdown, and transactions per block grew
  as the cadence fell.

The consensus question is answered: this configuration does not fork
under sustained 2x overload. The new open item is the slowdown: sustained
maximum-rate load degrades block cadence over tens of minutes on this
shape. Characterize what grows (state, trie commits, or the txpool replay
path; the soak counted 413662 replays) before quoting a sustained-rate
number beyond ~15 minutes.
