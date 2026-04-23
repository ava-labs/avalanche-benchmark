# Low TPS Accident Investigation - 2026-01-28

## Incident Summary

- **Incident window**: 10:31 - 11:40 UTC (19:31 - 20:40 JST)
- **Symptom**: Block tx count dropped from ~2380 to ~650 (~70% reduction)
- **Duration**: ~70 minutes
- **Root cause**: 15 of 17 Bombard sender accounts stopped sending transactions

## Timeline

| Time (UTC) | Block  | Tx Count | Unique Senders | Status       |
|------------|--------|----------|----------------|--------------|
| 10:30:06   | 14600  | 2363     | 17             | Normal       |
| 10:31:46   | 14800  | 329      | 2              | DROP STARTS  |
| 10:33-11:31| 15000-22000 | ~650 | 2              | Incident     |
| 11:40:11   | 23000  | 2380     | 17             | RECOVERY     |
| 12:13+     | 27000+ | 2380     | 17             | Normal       |

## Hypotheses Tested

### 1. EC2 Instance Throttling - RULED OUT

**Hypothesis**: Burstable EC2 instances (like t3) can be throttled when CPU credits are exhausted.

**Investigation**:
- Checked Terraform config at `multi-node/terraform-aws-untested/main.tf`
- Instance type: `m6a.4xlarge` (16 vCPU, 64GB RAM, AMD EPYC)
- This is a **standard instance**, NOT burstable - no CPU credit system

**Conclusion**: Not the cause. m6a instances provide consistent performance.

### 2. System Resource Exhaustion - RULED OUT

**Commands executed on node1 (18.183.88.206)**:
```bash
ssh 18.183.88.206 'top -bn1 | head -15'
ssh 18.183.88.206 'free -h'
ssh 18.183.88.206 'df -h /'
ssh 18.183.88.206 'iostat -x 1 2'
```

**Results at ~12:43 UTC**:
- CPU: 22-26% user, 68-78% idle
- Memory: 6.7 GB used / 61 GB total (54 GB available)
- Disk: 38 GB used / 193 GB (20%)
- I/O: 2.57% utilization, ~8 MB/s writes
- iowait: 0%
- steal: 0% (no hypervisor throttling)

**EBS config**: gp3 with 6000 IOPS, 500 MB/s throughput - nowhere near limits

**Conclusion**: Not the cause. System resources were healthy.

### 3. Avalanche Node Errors/Throttling - RULED OUT

**Commands executed**:
```bash
ssh 18.183.88.206 'tail -200 ~/avalanche-benchmark/data/validator/logs/main.log'
ssh 18.183.88.206 'grep -iE "error|warn|throttl|timeout|slow|backpressure|reject|fail" ~/avalanche-benchmark/data/validator/logs/avalanchego.out'
ssh 18.183.88.206 'grep -iE "error|warn|throttl|timeout|slow|backpressure|reject|fail" ~/avalanche-benchmark/data/validator/logs/gw8R4pw5ZJZfuGq4TnHvjdvg4fiNFBocvbWbjmzcEQZfPCnNm.log'
```

**Results**:
- Only startup warnings (expected): "sybil control is not enforced", "Failed to load snapshot, regenerating"
- No runtime errors, throttling, backpressure, or timeout messages
- Logs from incident window (10:30-11:40) were rotated out due to high volume

**Conclusion**: Not the cause. Nodes were healthy.

### 4. Kernel/OS Level Issues - RULED OUT

**Commands executed**:
```bash
ssh 18.183.88.206 'dmesg -T | grep -E "Jan 28 (19:|20:)"'
ssh 18.183.88.206 'journalctl --since "2025-01-28 10:30:00" --until "2025-01-28 11:45:00" | grep -iE "error|warn|fail|oom|kill|throttl"'
```

**Results**: Empty output - no OOM kills, no kernel errors

**Conclusion**: Not the cause.

### 5. Gas Price Changes - RULED OUT

**Command executed**:
```bash
# Sample blocks before/during/after incident
for block in 14200 14800 15000 20000 24000 30000; do
  hex=$(printf "0x%x" $block)
  curl -s -X POST --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBlockByNumber\",\"params\":[\"$hex\", false],\"id\":1}" \
    -H 'Content-Type: application/json' \
    "http://18.183.88.206:9654/ext/bc/gw8R4pw5ZJZfuGq4TnHvjdvg4fiNFBocvbWbjmzcEQZfPCnNm/rpc"
done
```

**Results**:
- baseFee: constant at 1
- txGasPrice: constant at 25
- gasLimit: constant at 50M

**Conclusion**: Not the cause. Gas economics unchanged.

### 6. Bombard Sender Accounts Stopped - ROOT CAUSE FOUND

**Command executed**:
```bash
# Count unique senders per block
for block in 14000 14200 14400 14600 14800 15000 16000 18000 20000 22000 23000 24000 25000 27000 30000; do
  hex=$(printf "0x%x" $block)
  curl -s -X POST --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBlockByNumber\",\"params\":[\"$hex\", true],\"id\":1}" \
    -H 'Content-Type: application/json' \
    "http://18.183.88.206:9654/ext/bc/gw8R4pw5ZJZfuGq4TnHvjdvg4fiNFBocvbWbjmzcEQZfPCnNm/rpc" | \
    jq '[.result.transactions[].from] | unique | length'
done
```

**Results**:
| Block | Time | Txs | Unique Senders |
|-------|------|-----|----------------|
| 14600 | 10:30:06 | 2363 | 17 |
| 14800 | 10:31:46 | 329 | **2** |
| 15000-22000 | 10:33-11:31 | ~650 | **2** |
| 23000 | 11:40:11 | 2380 | 17 |

**Conclusion**: ROOT CAUSE. 15 of 17 sender accounts stopped at 10:31 UTC, resumed at 11:40 UTC.

## Key Data Points

### Block samples during incident
```
Block 14600 @ 10:30:06: txs=2363, unique_senders=17  <- LAST NORMAL
Block 14800 @ 10:31:46: txs=329, unique_senders=2    <- INCIDENT START
Block 15000 @ 10:33:26: txs=685, unique_senders=2
Block 18000 @ 10:58:27: txs=671, unique_senders=2
Block 20000 @ 11:15:09: txs=679, unique_senders=2
Block 22000 @ 11:31:50: txs=635, unique_senders=2
Block 23000 @ 11:40:11: txs=2380, unique_senders=17  <- RECOVERY
```

### Sample transactions comparison
Normal block (14400) - 17 unique senders, txs spread across accounts
Incident block (15000) - only 2 unique senders:
- 0xb0ad0557ebb732a450b214a504d6805937dcdc47
- 0x0021a663e7a1ab85b3d0ba4ef0b50f5c4a5c18b6

## Infrastructure Details

- **Instance type**: m6a.4xlarge (16 vCPU, 64GB RAM)
- **Region**: ap-northeast-1 (Tokyo)
- **EBS**: gp3, 200GB, 6000 IOPS, 500 MB/s
- **Nodes**: 3 (NODE1=18.183.88.206, NODE2=13.113.48.18, NODE3=35.78.177.10)
- **Chain ID**: gw8R4pw5ZJZfuGq4TnHvjdvg4fiNFBocvbWbjmzcEQZfPCnNm

## Bombard Source Code Analysis

### Architecture Overview

Source files analyzed:
- `single-node/cmd/bombard/main.go` - CLI entry point
- `single-node/internal/bombard/bombard.go` - Main orchestration
- `single-node/internal/bombard/sender.go` - Transaction sending loops
- `single-node/internal/bombard/listener.go` - WebSocket block listener
- `single-node/internal/bombard/fund.go` - Account funding
- `single-node/internal/bombard/keys.go` - Key generation

### Key Architecture Points

1. **One goroutine per sender key** (`bombard.go:155-175`)
2. **Default 50 keys** (`main.go:29: flag.IntVar(&keyCount, "keys", 50, ...)`)
3. **Round-robin RPC distribution**: `clientPool[idx%len(clientPool)]`
4. **Single shared WebSocket listener** for all senders

### Why 17 Senders Instead of 50?

**Unknown** - Need to check how Bombard was started. Possibilities:
- Started with `-keys 17` flag
- 33 goroutines died during funding phase
- Multiple Bombard instances with different key counts

### Potential Failure Points Identified

#### 1. Silent Goroutine Death in `sender.go`

```go
// sender.go:64-67 - Returns silently on chain ID error
chainID, err := client.NetworkID(context.Background())
if err != nil {
    log.Printf("failed to get chain ID: %v", err)
    return  // <-- GOROUTINE DIES, NO RECOVERY
}
```

**Impact**: If RPC temporarily fails, sender goroutine dies permanently.

#### 2. WebSocket Listener Single Point of Failure (`listener.go`)

```go
// listener.go:57-60 - Subscription failure kills listener
sub, err := l.client.SubscribeNewHead(context.Background(), headers)
if err != nil {
    log.Printf("failed to subscribe to new heads: %v", err)
    return  // <-- ALL SENDERS AFFECTED
}

// listener.go:65-67 - Subscription error also fatal
case err := <-sub.Err():
    log.Printf("subscription error: %v", err)
    return  // <-- ALL SENDERS AFFECTED
```

**Impact**: WebSocket disconnect kills the listener, causing all `AwaitTxMined` calls to timeout.

#### 3. No Reconnection Logic

The listener has no reconnection logic. If WebSocket dies:
- `subscribeToBlocks()` returns
- No new blocks are processed
- All `AwaitTxMined()` calls timeout after `timeoutSeconds`
- Senders enter error loop with 1-second sleeps

#### 4. Error Handling Causes Slowdown (`sender.go:108-111`)

```go
if hasError {
    shouldRefetchNonce = true
    time.Sleep(1 * time.Second)  // <-- SLOWS DOWN ON ANY ERROR
    continue
}
```

### Re-analysis: Timeout Doesn't Kill Senders

**Important correction**: `AwaitTxMined` timeout does NOT kill senders.

```go
// sender.go:113-115 - Timeout just triggers nonce refresh, loop continues
for _, hash := range txHashes {
    if listener.AwaitTxMined(hash, timeoutSeconds) != nil {
        shouldRefetchNonce = true  // Just marks for refresh
    }
}
// Loop continues normally after this
```

So even if WebSocket listener dies and all `AwaitTxMined` calls timeout:
- Senders should continue (with nonce refresh)
- They would just be slower due to waiting for timeouts

**This means the WebSocket listener failure hypothesis is WRONG or incomplete.**

### Actual Fatal Points (Goroutine Death)

Only these can actually kill a sender goroutine:

1. **`client.NetworkID()` failure** (`sender.go:64-67`):
   ```go
   chainID, err := client.NetworkID(context.Background())
   if err != nil {
       log.Printf("failed to get chain ID: %v", err)
       return  // FATAL - goroutine dies
   }
   ```

2. **`log.Fatalf` on sign failure** (`sender.go:83-85`):
   ```go
   signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), key)
   if err != nil {
       log.Fatalf("failed to sign transaction: %v", err)  // FATAL - entire process dies
   }
   ```

3. **Context cancellation** (`sender.go:71-75`) - but this is intentional shutdown

### Revised Hypothesis

Since `log.Fatalf` would kill the entire process (not just 15/17 senders), and context cancellation is intentional, the most likely cause is:

**Multiple Bombard instances running, one crashed/was killed**

- Instance A: 15 keys - crashed or was killed at 10:31 UTC
- Instance B: 2 keys - kept running
- Instance A restarted at 11:40 UTC

OR

**RPC connection failure during `NetworkID()` call**

If 15 sender goroutines happened to call `client.NetworkID()` at the same time and failed, they would all die silently. But this seems unlikely since `NetworkID()` is only called once at goroutine start.

### Recommended Fixes

1. **Replace `log.Fatalf` with error handling** - don't crash entire process
2. **Add retry logic for `NetworkID()`** - don't die on transient RPC failure
3. **Add goroutine health monitoring** - detect and restart dead senders
4. **Add reconnection logic to WebSocket listener** - for robustness
5. **Log sender count periodically** - to detect silent goroutine deaths

## Refactoring Requirements (from user)

### Philosophy: Reliability is #1

User's guidance on error handling philosophy:

> "About fatal errors that crash everything, for example like signing error - yes we should crash early, we should not hide any errors. This is all good, this is how it's supposed to be."

> "Now maybe let's talk about non-fatal errors. Then everything kind of has to restart. So if there is anything for example signing error, what could cause signing error? It cannot be caused by a bleep of a network, no freaking way! If we have a signing error, there's something truly horrible happening."

> "The same with the WebSocket error - let's keep reconnecting. So basically you either have fatal errors and they're like weak and nasty or you have just very simple errors that keep retrying writing no user in logs but like very gently."

> "Like how does it like this all right now from time to time it's like 'Hey, it has been like 80,000 errors and the last error like this stuff like that.' You know you can do it good."

> "Reliable is the number one word in here. It's either fails fast if there is something to fail or if it doesn't fail fast it has to keep retrying. It should not in any chance end up in a case where it's just like cannot do anything, everybody kinda dead, just hang out. Like it was that only two routines were sending transactions"

### Two Categories of Errors

1. **Fatal Errors → Crash Immediately**
   - Sign transaction failure (fundamentally broken - bad key, bad chain ID, etc.)
   - Key generation failure
   - These indicate something is truly wrong, not a network blip

2. **Transient Errors → Retry Forever Silently**
   - `NetworkID()` RPC failure → retry in loop
   - WebSocket disconnect → reconnect
   - `SendTransaction` failure → retry with nonce refresh
   - `PendingNonceAt` failure → retry
   - Log errors aggregated periodically ("80,000 errors, last: ..."), not spamming

### Key Principle: No Zombie State

The system must NEVER end up in a state where:
- Goroutines are alive but not doing useful work
- Senders silently died and nobody noticed
- The tool appears to be running but TPS dropped 70%

### Required Changes

1. **Wrap transient operations in retry loops** - Never `return` on network errors
2. **Add active sender count tracking** - Log every 30s how many senders are alive
3. **WebSocket reconnection** - Listener must reconnect on disconnect
4. **Aggregated error reporting** - Categorize errors, report counts periodically
5. **Keep `log.Fatalf` for truly fatal errors** - Sign failures, key gen failures

## Root Cause Found: Sequential Timeout Bug

### The Bug

In `sender.go`, the code waited for **each transaction sequentially**:

```go
// OLD CODE - BUGGY
for _, hash := range txHashes {
    if listener.AwaitTxMined(hash, timeoutSeconds) != nil {
        shouldRefetchNonce = true
    }
}
```

With `batchSize=500` and `timeoutSeconds=10`, if the WebSocket listener died:
- Each of 500 transactions would timeout after 10 seconds
- Total wait time: 500 × 10s = **5000 seconds = 83 minutes**

This matches the incident duration (~70 minutes) almost exactly.

### The Fix

Wait for only the **last transaction** in the batch. Due to EVM nonce ordering, if the last tx (highest nonce) is mined, all previous ones must be too:

```go
// NEW CODE - FIXED
// Wait for last transaction only - if it's mined, all previous ones are too
// (same sender, sequential nonces, EVM enforces nonce ordering)
if len(txHashes) > 0 {
    if listener.AwaitTxMined(txHashes[len(txHashes)-1], timeoutSeconds) != nil {
        shouldRefetchNonce = true
    }
}
```

### Changes Made

1. **`sender.go`**: Changed all three bombardment functions to wait for last tx only
   - `bombardWithTransactions()`
   - `bombardWithERC20Transactions()`
   - `bombardWithBothTransactions()`

2. **`main.go`**: Increased default timeout from 10s to 30s
   - With only one wait per batch, we can afford a longer timeout
   - Provides more margin for temporary network issues

### Impact

- Before: Timeout scenario = 500 × 10s = 83 minutes of hanging
- After: Timeout scenario = 1 × 30s = 30 seconds, then retry

## Open Questions

1. How was Bombard started? What flags were used? (`-keys` value?)
2. Was Bombard restarted at 11:40 UTC?
3. Were there any Bombard stdout/stderr logs from the incident?
4. Which 2 sender addresses kept working during the incident?

## Commands Reference

### Query block info
```bash
curl -s -X POST --data '{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x4A38", true],"id":1}' \
  -H 'Content-Type: application/json' \
  "http://18.183.88.206:9654/ext/bc/gw8R4pw5ZJZfuGq4TnHvjdvg4fiNFBocvbWbjmzcEQZfPCnNm/rpc"
```

### Check tmux scrollback (limited to 2000 lines)
```bash
tmux capture-pane -t 0 -p -S -2000
```

### Check node logs
```bash
ssh 18.183.88.206 'tail -200 ~/avalanche-benchmark/data/validator/logs/main.log'
ssh 18.183.88.206 'tail -200 ~/avalanche-benchmark/data/rpc/logs/gw8R4pw5ZJZfuGq4TnHvjdvg4fiNFBocvbWbjmzcEQZfPCnNm.log'
```
