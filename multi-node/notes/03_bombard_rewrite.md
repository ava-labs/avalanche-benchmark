# Bombard Rewrite - Design Notes

## Progress Tracker

- [x] Design doc complete
- [x] Phase 1: main.go - flags, config, entry point
- [x] Phase 2: keys.go - key pool generation and management
- [x] Phase 3: fund.go - lazy funding of keys
- [x] Phase 4: sender.go - metered sending loop per key
- [x] Phase 5: health.go - background health monitor
- [x] Phase 6: controller.go - rate controller, key activation
- [x] Phase 7: listener.go - block listener (metrics only)
- [x] Phase 8: TUI dashboard with ASCII chart and keyboard controls
- [ ] Phase 9: Multi-hour stability testing
- [ ] FUTURE: ERC20 mode (after native mode is proven stable)

## Problem Statement

The current bombard tool is unstable for long-running benchmarks. It works for 1-2 hours then falls apart - throughput oscillates wildly, drops to 10% capacity, recovers, repeats. Multiple bug fix attempts have failed because the architecture is fundamentally flawed.

### Root Causes of Instability

1. **Synchronous confirmation waiting**: Each sender waits for tx confirmation before sending next batch. WebSocket hiccups cause cascading stalls.

2. **500+ independent goroutines fighting**: No coordination, nonce gaps form, recovery storms happen when multiple goroutines refetch nonces simultaneously.

3. **Batch-based sending**: "Send batch, wait, send batch" creates bursty traffic instead of steady flow.

4. **Real-time feedback dependency**: The hot path depends on WebSocket block notifications. Any listener hiccup affects sending.

## Key Insights from Testing

### Nonce Behavior (tested against live node)

```
Nonce too low  -> immediate error: "nonce too low: next nonce X, tx nonce Y"
Nonce correct  -> accepted, returns nil
Nonce too high -> ACCEPTED SILENTLY (queued, waiting for gap to fill)
Duplicate      -> "already known"
```

**Critical**: Nonce gaps are silent. If tx with nonce 100 expires, nonces 101-150 sit in queue forever (until they also expire). No error returned on send. A key can get silently stuck.

### Available RPC Queries

- `NonceAt(addr, nil)` - confirmed nonce (on-chain)
- `PendingNonceAt(addr)` - pending nonce (includes mempool)
- `pending - confirmed` = in-flight tx count for that address
- `txpool_status` - NOT AVAILABLE on this node
- `BlockByNumber` - can get gas limit, tx count, gas used

### Chain Parameters (from live query)

- Gas limit: 50M per block
- Native transfer: 21k gas -> ~2380 tx/block
- ERC20 transfer: 65k gas -> ~769 tx/block
- Block rate: ~2 blocks/sec (500ms target)
- Max theoretical TPS: ~4760 (native), ~1538 (ERC20)
- Mempool config: 262k global slots, 1 minute expiration

## New Architecture

### Core Principle: Decouple Sending from Confirmation

The sender's job is to maintain a steady send rate. Confirmation tracking is background housekeeping, not in the hot path.

### Design: Metered Key Pool

```
User sets: --tps 5000 (or --max for auto-detect)

Tool calculates:
- Query gas limit from latest block
- Determine tx gas cost (21k native, 65k ERC20)
- Calculate max TPS = (gas_limit / tx_gas) * blocks_per_sec
- Target rate = min(user_tps, max * 1.1)
```

### Key Management

1. **Pre-generate large key pool at startup** (10k+ keys, just CPU, instant)
2. **Lazy funding**: Only fund a key when activating it
3. **Inactive keys serve as random tx targets** (free addresses)
4. **Active keys**: Each runs a simple metered send loop

### Sender Loop (per active key)

```go
func senderLoop(key, rate) {
    ticker := time.NewTicker(1 / rate)  // e.g., 10ms for 100 tx/s
    nonce := fetchNonce(key.Address)
    
    for range ticker.C {
        tx := buildTx(nonce, randomTarget())
        err := send(tx)
        
        if err == "nonce too low" {
            nonce = fetchNonce(key.Address)  // recover
            continue
        }
        if err == "already known" {
            nonce++  // skip, was duplicate
            continue
        }
        if err != nil {
            // other error (network?), retry same nonce next tick
            continue
        }
        
        nonce++
    }
}
```

No batching. No confirmation waiting. Just a steady metronome.

### Health Monitor (background, every 5-10 seconds)

```go
func healthCheck(key) {
    confirmed := NonceAt(key.Address)
    pending := PendingNonceAt(key.Address)
    inFlight := pending - confirmed
    
    if inFlight > threshold {  // e.g., 500 = ~5 seconds of backlog
        // Key might be stuck or we're sending too fast
        // Signal rate controller to slow down
    }
    
    if localNonce > pending + gap {  // we think we're ahead of mempool
        // Likely had silent expiration, reset local nonce
        resetNonce(key, pending)
    }
}
```

### Rate Controller

Maintains target TPS across all active keys.

```
target_tps = 5000
keys_active = 50
per_key_rate = target_tps / keys_active = 100 tx/s

If health monitor says we're backing up:
  - Reduce per_key_rate, OR
  - Deactivate some keys

If we're under target and blocks aren't full:
  - Increase per_key_rate, OR  
  - Activate more keys (fund on demand)
```

### Block Listener (informational only)

Still subscribe to blocks for metrics/logging, but NOT for flow control.

```go
func blockListener() {
    for block := range newBlocks {
        log.Printf("Block %d: %d txs, %.1fM gas, %dms delay",
            block.Number, block.TxCount, block.GasUsed/1e6, block.Delay)
        
        metrics.RecordBlock(block)  // for pretty charts
    }
}
```

If WebSocket hiccups, we miss a log line. Sending continues unaffected.

## UX Design

### Simple Interface

```bash
# Target specific TPS
bombard --rpc URL1,URL2,URL3 --tps 5000

# Auto-detect and saturate
bombard --rpc URL1,URL2,URL3 --max

# With ERC20 mode
bombard --rpc URL1,URL2,URL3 --tps 1500 --erc20
```

No more `--batch`, `--keys`, `--timeout`. The tool figures it out.

### What User Sees

```
Bombard v2
Chain ID: 99999
Gas limit: 50M, Max TPS (native): 4760
Target: 5000 TPS (105% of max)

Funding initial keys... done (50 keys, 5.0 ETH)
Starting bombardment...

[Block 92251] 2380 txs, 50.0M gas, 502ms
[Block 92252] 2380 txs, 50.0M gas, 498ms
[Block 92253] 2380 txs, 50.0M gas, 501ms
...

Stats (last 10s): 4760 TPS confirmed, 0 errors, 50 active keys
```

Steady. Predictable. Self-healing.

## Implementation Plan

### Phase 1: Core Types and Key Pool

File: `internal/bombard/types.go`
- Config struct (RPCURLs, TargetTPS, Mode, MaxMode bool)
- Key struct (PrivateKey, Address, Active, LocalNonce)
- Metrics struct

File: `internal/bombard/keypool.go`
- GenerateKeyPool(count) - pre-generate keys
- Fund(key) - lazy funding
- RandomTarget() - pick random inactive key address

### Phase 2: Sender Implementation

File: `internal/bombard/sender.go`
- SenderLoop(ctx, key, rateChan, client)
- Metered sending with time.Ticker
- Nonce recovery on "nonce too low"
- Reports errors to central collector

### Phase 3: Health Monitor

File: `internal/bombard/health.go`
- HealthMonitor runs every N seconds
- Checks each active key: confirmed vs pending nonce
- Detects stuck keys, signals rate controller
- Detects nonce drift, triggers reset

### Phase 4: Rate Controller

File: `internal/bombard/controller.go`
- Maintains target TPS
- Distributes rate across active keys
- Activates/deactivates keys as needed
- Responds to health monitor signals

### Phase 5: Block Listener (metrics only)

File: `internal/bombard/listener.go`
- Subscribe to new blocks
- Log block stats
- Feed metrics collector
- Graceful reconnect on disconnect (but doesn't affect sending)

### Phase 6: Main Orchestrator

File: `internal/bombard/bombard.go`
- Query chain params (gas limit, chain ID)
- Calculate target rate
- Initialize key pool
- Fund initial keys
- Start senders, health monitor, listener
- Graceful shutdown

File: `cmd/bombard/main.go`
- Parse flags (--rpc, --tps, --max, --erc20)
- Call bombard.Run(cfg)

## Error Handling Strategy

| Error | Action |
|-------|--------|
| "nonce too low" | Refetch nonce, continue |
| "already known" | Increment nonce, continue |
| "insufficient funds" | Deactivate key, log warning |
| Network timeout | Retry next tick, same nonce |
| "nonce too high" (silent) | Detected by health monitor via nonce drift |
| Tx expiration (silent) | Detected by health monitor, reset nonce |

## Constants / Tuning

```go
const (
    InitialKeyPoolSize = 10000
    InitialActiveKeys  = 50
    MaxActiveKeys      = 1000
    HealthCheckInterval = 5 * time.Second
    MaxInFlightPerKey  = 500  // ~5 sec at 100 tx/s
    FundAmount         = 100  // ETH per key
    NativeGas          = 21000
    ERC20Gas           = 65000
    BlocksPerSecond    = 2
)
```

## RPC Load Distribution

Use all provided RPC URLs round-robin:
- Each sender picks client: `clients[keyIndex % len(clients)]`
- Distributes load across nodes
- If one node is slow, only affects subset of senders

## Testing Checklist

1. Single key metered sending - verify steady rate
2. Nonce recovery on "nonce too low"
3. Health monitor detects stuck key
4. Rate controller adjusts on backpressure
5. Graceful handling of RPC disconnection
6. Long-running stability (target: 24+ hours)
7. Metrics accuracy vs actual chain state
