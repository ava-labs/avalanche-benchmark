# Consensus parameter tuning: manual checks and benchmark runbook

Fleet is currently on **k=60, alphaPreference=31, alphaConfidence=38, beta=12**.

Control box: `ssh -i ~/.ssh/fleet ubuntu@13.57.189.81` (IP changes on every stop/start,
get the current one with `cd terraform-aws-untested && terraform output -raw control_ip`).
Everything below runs from `~/avalanche-benchmark` on the control box.

## 1. Checking the live parameters

Three levels of confidence. The log is authoritative; the file only tells you what the
next restart will load.

### a) What the running process actually loaded (authoritative)

avalanchego dumps its subnet configs into the `initializing node` line at startup, so the
last occurrence in the journal is what the live process is using:

```bash
ssh -i ~/.ssh/fleet ubuntu@<node-ip> \
  'sudo journalctl -u avalanche-benchmark-node-<n> --no-pager \
   | grep -o "\"k\":[0-9]*,\"alphaPreference\":[0-9]*,\"alphaConfidence\":[0-9]*,\"beta\":[0-9]*" \
   | tail -2'
```

Each boot logs two entries, primary network first and the L1 second, so the **last line is
the L1**. Verified output on node 1 right now:

```
"k":60,"alphaPreference":31,"alphaConfidence":39,"beta":12   <- previous boot, ignore
"k":20,"alphaPreference":15,"alphaConfidence":15,"beta":20   <- primary network default, ignore
"k":60,"alphaPreference":31,"alphaConfidence":38,"beta":12   <- live L1 config
```

`k=20/15/15/beta=20` is avalanchego's `DefaultParameters` for the primary network and has
nothing to do with your L1.

All 12 at once, from control:

```bash
cd ~/avalanche-benchmark
for e in "1 54.193.214.107" "2 18.145.140.146" "3 18.145.230.145" "4 13.56.77.191" \
         "5 44.252.128.5" "6 16.146.251.121" "7 54.203.213.98" "8 54.212.242.77" \
         "9 18.145.162.68" "10 54.241.105.142" "11 54.218.141.97" "12 52.11.237.94"; do
  set -- $e
  echo -n "node $1: "
  ssh -n -i ~/.ssh/fleet -o StrictHostKeyChecking=no ubuntu@$2 \
    "sudo journalctl -u avalanche-benchmark-node-$1 --no-pager \
     | grep -o '\"alphaPreference\":[0-9]*,\"alphaConfidence\":[0-9]*' | tail -1"
done
```

### b) What is on disk (what the next restart will load)

```bash
SUBNET=$(grep ^SUBNET_ID= deployment/network.env | cut -d= -f2)
ssh -i ~/.ssh/fleet ubuntu@<node-ip> "sudo cat /etc/avalanche-benchmark/<n>/subnets/$SUBNET.json"
```

**Gotcha: `subnets/` contains more than one file.** There is a leftover
`LkUhywTrgqhH1vMtcVX5DCqBpur73vAua4mvkstqKEVaAd17S.json` from an older deployment still
holding `k=30, aP=16, aC=17`. It is not in `track-subnets`, so it is dead weight, but
`cat`-ing the wrong file will show you stale parameters. Always index by the current
`SUBNET_ID` from `deployment/network.env`.

### c) The source of truth you edit

`~/avalanche-benchmark/subnet-config.json` on the control box. Read once at avalanchego
startup, so a change needs a restart (no L1 reset).

## 2. Applying a parameter change

Snow alphas are node-local poll thresholds, not block-validity rules, so a rolling restart
with temporarily mixed alphas is safe. (`proposerWindowMilliseconds` and
`proposerMillisecondTimestamps` are NOT: those must match everywhere.)

```bash
cd ~/avalanche-benchmark
vim subnet-config.json                                  # edit snowParameters
./bin/fleet deploy frozen 1 2 3 4 5 6 7 8 9 10 11 12    # rolling, one node fully at a time, ~20s each
./bin/fleet status                                      # expect 12x up, identical heights
```

**Full stake must be online before rolling.** A node restarted while one of the three heavy
validators is down cannot finish bootstrap: the gate is 75% of stake
(`(3*bootstrapWeight+3)/4`) and 205000/305000 = 67% falls short. It will sit at HTTP 503
forever and wedge the deploy in its readiness phase.

Valid combinations only, enforced by avalanchego `Parameters.Verify()`:
`K/2 < alphaPreference <= alphaConfidence <= K`. At k=60 the alphaPreference floor is 31.

## 3. Running the benchmark yourself

```bash
cd ~/avalanche-benchmark
./bin/bombard -rps 4000 -duration 60s
```

Drop `-tui=false` and you get the live TUI. It auto-discovers all four `role=rpc` nodes from
`nodes.ini` and the chain ID from `deployment/network.env`, then fans every tx across all of
them; you do not target nodes yourself.

Read the output as: `mined` / `duration` = actual TPS. `minedTps` per line is a noisy
one-second sample, so do not judge from a single row. `AT-CAP(behind)` means throughput is
limited by the 2000-nonce in-flight cap and latency rather than by the chain's capacity,
which is normal in the one-down case.

### The one-validator-down drill

```bash
./bin/fleet stop 3                    # node 3 = identity c, weight 100000, one of the three heavies
sleep 5
./bin/bombard -rps 4000 -duration 60s
./forkcheck.sh                        # ALWAYS after a run
./bin/fleet start 3                   # then wait for status to go clean, takes a few minutes
./bin/fleet status
```

Stopping nodes 1, 2 or 3 is the meaningful test. Nodes 4-8 carry weight 1000 each, so
losing one is only 0.3% of stake and proves nothing.

### Fork check (do this after every run, not before)

`./forkcheck.sh` reads `eth_blockNumber` from all 12 nodes, takes the minimum, and compares
the block hash at `min - 5` across every node. One distinct hash = OK.

Why it matters: **TPS cannot see a fork.** At alpha 11/11 nodes 4, 6 and 10 finalized a
different block at height 814087 than the other nine, and the benchmark still reported a
clean 3582 TPS with 0 rejects and 0 reorgs, because only 2000 of 305000 weight had diverged.

Also note: the L1 only builds blocks under load, so **identical static heights on an idle
chain are normal**. Heights that DIFFER and do not converge while idle are the fork signal.

Recovery from a forked node (it cannot replay past the divergence and will FATAL with
`failed to verify block ... in bootstrapping`):

```bash
./bin/fleet destroy 4 6 10        # SIGKILL + wipe chainData only, does NOT touch machines
./bin/fleet deploy frozen 4 6 10  # state syncs back onto the majority branch
```

## 4. Measured results (one heavy validator down, 4000 rps, 60s, beta=12)

With one of three heavies down, 205000/305000 = 67.21% of stake is online, so a poll of k
returns on average 0.6721*k online votes. What matters is the **alphaConfidence/k ratio**
against that 0.6721, not the raw numbers.

| aC/k | config | TPS | p50 | fork |
|---|---|---|---|---|
| 0.55 | k=20 11/11 | 3582 | 300-700ms, 1.7s spikes | **FORKED** |
| 0.60 | k=30 16/18 | 3558 | 250-350ms | none |
| 0.633 | k=30 16/19 | 3704 | 620-830ms | none |
| **0.633** | **k=60 31/38** | **3682** | **344-716ms** | **none (current)** |
| 0.65 | k=60 31/39 | 2960 | 509-678ms | none |
| 0.667 | k=30 16/20 | 1870 | 1.45s, stalls, 22k resubmits | none |

Full fleet, nothing down, at 11/11: 3930 TPS, p50 75ms.

**The throughput cliff is between 0.633 and 0.65.** 0.633 is the tightest ratio that clears
3000 TPS. k=60 was chosen over k=30 for the same ratio because it puts alphaPreference at a
lower ratio (31/60 = 0.517 vs 16/30 = 0.533) and halves sampling variance.

### Raising k does NOT scale traffic or latency with k

`sendQuery` samples `k` slots **with replacement** into a `bag` (multiset) for vote
accounting, but sends to `set.Of(vdrIDs...)`, a deduplicated set:

```go
vdrIDs, _ := e.Validators.Sample(e.Ctx.SubnetID, e.Params.K)  // k slots, with replacement
vdrBag := bag.Of(vdrIDs...)                                   // multiplicities kept for votes
vdrSet := set.Of(vdrIDs...)                                   // DEDUPED for the network send
e.Sender.SendPullQuery(ctx, vdrSet, ...)
```

So one poll sends at most 8 messages (one per distinct validator) no matter how large k is,
and each validator's single Chits reply is counted with its sampled multiplicity.

Measured on node 9, full fleet, 4000 rps, per accepted block:

| | k=20 | k=60 | change |
|---|---|---|---|
| chits / block | 15.3 | 21.3 | 1.39x |
| polls / block | 3.94 | 4.64 | 1.18x |
| poll_dur | 6.76ms | 4.92ms | **down** |
| accept_lat | 20.8ms | 17.1ms | **down** |
| rounds/accept | 12.29 | 12.59 | flat |

Tripling k raised message volume ~1.4x, not 3x, and latency went *down*. The residual 1.4x is
because the weights are skewed: at k=20 most samples land on the three heavies, so few distinct
validators get hit, while k=60 more often reaches the five light validators too. Expected
distinct recipients only moves from ~3.3 to ~3.9.

### Why alphaPreference is at its floor and not raised

The two alphas want opposite things:

- **alphaConfidence: higher is safer.** It is the threshold to accrue confidence toward
  finalization, so a high value demands a supermajority before a block counts.
- **alphaPreference: lower is safer.** It is the threshold to *flip your preference*. A low
  value makes nodes snap to the emerging plurality and collapses the metastable ~50/50 split
  between sibling blocks that produces forks in the first place. A high value makes nodes
  stubborn and keeps the split alive.

At 11/11 both were the same single marginal threshold, which is the worst arrangement: one
55% poll simultaneously flipped preference and counted toward finalization.

### Two separate thresholds, both driven by aC/k

**1. A hard query gate (this one halts the chain).** `snow/engine/snowman/engine.go`,
`sendQuery` calls `abortDueToInsufficientConnectedStake` before sampling:

```go
stakeConnectedRatio := e.Config.ConnectedValidators.ConnectedPercent()
minConnectedStakeToQuery := float64(e.Params.AlphaConfidence) / float64(e.Params.K)
if stakeConnectedRatio < minConnectedStakeToQuery { /* drop the query entirely */ }
```

So if connected stake falls below `aC/k` the node stops issuing queries altogether and
consensus stops dead. It logs at Debug level with reason "insufficient stake", which is why
it is easy to miss.

With one heavy down, connected stake is 0.6721, so **aC/k must stay below 0.6721**:

| aC/k | margin | verdict |
|---|---|---|
| 38/60 = 0.633 | 6.0% | current, safe |
| 39/60 = 0.650 | 3.3% | thin |
| 40/60 = 0.667 | 0.8% | razor |
| 41/60 = 0.683 | negative | **hard halt** |
| 14/20 = 0.700 | negative | **hard halt** |

Margin matters because `ConnectedPercent` fluctuates with transient disconnects. At 38/60
you could additionally lose all five light validators (0.6557 connected) and still poll.

**2. A health-report threshold (cosmetic).** `MinPercentConnectedHealthy()` =
`0.8 * aC/k + 0.2`, referenced only in `snow/networking/handler/health.go`. At 38/60 that
demands 0.707 vs 0.6721 connected, so nodes report **unhealthy** while one heavy is down even
though consensus is working fine. Do not blame the config for this.

**On top of the gate, there is a statistical effect.** beta=12 requires 12 *consecutive*
polls clearing aC, so as aC/k approaches the 0.6721 mean the tail probability collapses. That
is why 40/60 and 20/30 crawl rather than cleanly halt: they clear the gate by a hair but lose
the coin-flip sequence. Both effects push the same direction.

## 5. Open item

60-second runs cannot prove fork-freedom. The 11/11 fork took minutes of sustained load to
appear. Nothing forked at 0.60 or 0.633, but that is absence of evidence over ~1 minute
each. A 30-60 minute soak at k=60/31/38 with periodic `./forkcheck.sh` is still needed
before trusting it.
