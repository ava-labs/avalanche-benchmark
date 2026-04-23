# Bombard Round-Robin Polling Redesign

## Date: 2026-01-28

## Problem Statement

### Current Architecture (WebSocket-based)

The current `TxListener` connects via WebSocket to a **single node** to subscribe to new block headers:

```go
// listener.go - Current implementation
func NewTxListener(wsURL string) (*TxListener, error) {
    client, err := ethclient.Dial(wsURL)  // Single WebSocket connection
    // ...
    go listener.subscribeToBlocks()
}

func (l *TxListener) subscribeToBlocks() {
    sub, err := l.client.SubscribeNewHead(context.Background(), headers)
    // If this fails or disconnects, listener dies
    // No reconnection logic
}
```

### Why This Is a Problem

1. **Single Point of Failure**: If the WebSocket node lags, gets stuck, or has network issues, the entire listener is blind. Other nodes may already have new blocks, but we can't see them.

2. **No Reconnection**: If WebSocket disconnects, `subscribeToBlocks()` returns and never recovers. All `AwaitTxMined()` calls timeout.

3. **Node Connectivity Variance**: In multi-node setups, connectivity between nodes isn't perfect. One node might be a few blocks behind due to network partitions or sync delays. Relying on a single node means we inherit its view of the chain.

4. **Cascade Effect on Senders**: When listener can't see blocks:
   - `AwaitTxMined()` times out
   - Senders refetch nonce and retry
   - Throughput drops dramatically
   - This caused the 70-minute incident on 2026-01-28

### Real-World Scenario

```
Nodes: node1, node2, node3
Reality: Block 100 exists, node2 and node3 have it

WebSocket approach (connected to node1):
  node1 (stuck at block 99) --ws--> listener 
  listener thinks: "no new blocks"
  senders: waiting... waiting... timeout... retry...
  
  Meanwhile node2 and node3 have block 100, but we can't see it.
```

## Solution: Round-Robin Polling

### Core Idea

Replace WebSocket subscription with HTTP polling across all nodes. Poll every 10ms, rotating through nodes. Chase the block frontier - whichever node has the next block first, we'll find it.

### Algorithm

```
State:
  currentBlock = starting block number
  nodeIndex = 0
  nodes = [node1, node2, node3, ...]

Loop (every 10ms):
  1. Ask nodes[nodeIndex] for block currentBlock
  2. If found:
     - Record transactions from block
     - currentBlock++
     - nodeIndex = (nodeIndex + 1) % len(nodes)
  3. If not found:
     - nodeIndex = (nodeIndex + 1) % len(nodes)
     - Keep trying same block on next node
```

### Example Execution

```
Step 1: node1, block 100 → found → record txs, next: node2, block 101
Step 2: node2, block 101 → found → record txs, next: node3, block 102
Step 3: node3, block 102 → not found → next: node1, block 102
Step 4: node1, block 102 → not found → next: node2, block 102
Step 5: node2, block 102 → found → record txs, next: node3, block 103
...
```

### Benefits

1. **No Single Point of Failure**: If one node is stuck, we find the block from another node within milliseconds.

2. **Self-Healing**: No connection state to manage. Each poll is independent. Node goes down? We just get an error and try the next one.

3. **Distributed Load**: Polling rotates across nodes, spreading the query load.

4. **Consistent View**: We always chase the actual chain tip, not one node's potentially stale view.

### Tradeoffs

1. **More RPC Calls**: At 10ms intervals, that's 100 calls/second. Distributed across 3 nodes = ~33 calls/node/second. Negligible for modern nodes.

2. **Slight Latency**: WebSocket push is theoretically instant. Polling at 10ms adds up to 10ms latency. Acceptable for our use case.

3. **No Push Notification**: We poll on a schedule rather than react to events. But 10ms polling is fast enough that it doesn't matter.

## Implementation Plan

### New TxListener API

```go
// NewTxListener creates a polling-based listener across multiple RPC endpoints
func NewTxListener(rpcURLs []string) (*TxListener, error)

// AwaitTxMined remains the same interface
func (l *TxListener) AwaitTxMined(txHash string, timeoutSec int) error

// Close stops the polling loop
func (l *TxListener) Close()
```

### Changes Required

1. **listener.go**: Complete rewrite
   - Remove WebSocket subscription
   - Add HTTP client pool for polling
   - Implement round-robin block fetching
   - Keep the same `minedTxs` map and `AwaitTxMined` interface

2. **bombard.go**: Update listener initialization
   - Pass all RPC URLs instead of single WebSocket URL
   - Remove WebSocket URL conversion logic

## Code Changes

### listener.go - Complete Rewrite

Replaced WebSocket subscription with round-robin HTTP polling:

```go
// Key constants
const pollInterval = 10 * time.Millisecond

// TxListener now holds multiple clients
type TxListener struct {
    clients               []*ethclient.Client  // Pool of RPC clients
    minedTxs              map[string]bool
    // ... rest unchanged
}

// Constructor now takes multiple RPC URLs
func NewTxListener(rpcURLs []string) (*TxListener, error)

// Core polling loop
func (l *TxListener) pollBlocks() {
    // Get starting block from first available node
    // Then loop:
    //   - Ask nodes[nodeIndex] for block currentBlock
    //   - If found: process, increment block, advance node
    //   - If not found: advance node, retry same block
}
```

### bombard.go - Updated Listener Creation

Before:
```go
// Create WebSocket URL from HTTP URL
wsURL := strings.Replace(rpcURL, "http://", "ws://", 1)
wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
wsURL = strings.Replace(wsURL, "/rpc", "/ws", 1)

listener, err := NewTxListener(wsURL)
```

After:
```go
// Create polling-based listener using all RPC URLs (round-robin)
listener, err := NewTxListener(cfg.RPCURLs)
```

## Summary

| Aspect | Before (WebSocket) | After (Polling) |
|--------|-------------------|-----------------|
| Connection | Single WS to one node | HTTP to all nodes |
| Failure mode | Listener dies on disconnect | Graceful fallback to other nodes |
| Reconnection | None | N/A (stateless polling) |
| Latency | Push (instant) | Poll (up to 10ms) |
| Node coverage | 1 node | All nodes |
| Recovery | Manual restart | Automatic |

---

## Iteration 2: Parallel Goroutines with Atomic Counter

### Problem with Single-Loop Round-Robin

The initial round-robin approach uses a single goroutine that cycles through nodes sequentially. If we have 3 nodes and 10ms pause between requests, the effective polling rate is:
- 30ms to cycle through all nodes
- If a block appears right after we checked node1, we wait ~20-30ms to find it

### New Design: One Goroutine Per Node

Instead of one loop cycling through nodes, spawn one goroutine per node. Each goroutine independently polls its assigned node. They coordinate via a single atomic counter.

### Architecture

```
Shared state:
  currentBlock uint64 (atomic) = "which block are we looking for?"

goroutine per node:
  loop:
    1. Read currentBlock atomically -> targetBlock
    2. Fetch targetBlock from my node
    3. If not found or error: sleep 10ms, continue
    4. If found: try CAS(targetBlock, targetBlock+1)
       - CAS succeeds: I won! Process the block, notify waiters
       - CAS fails: Someone else already got it, discard
    5. Sleep 10ms
    6. Continue loop
```

### Example Execution

```
shared: currentBlock = 100 (atomic)

goroutine 1 (node1):          goroutine 2 (node2):          goroutine 3 (node3):
  read currentBlock -> 100      read currentBlock -> 100      read currentBlock -> 100
  fetch block 100               fetch block 100               fetch block 100
  got it!                       got it!                       still fetching...
  CAS(100 -> 101) SUCCESS       CAS(100 -> 101) FAIL          
  process block, notify         discard (stale)               got it!
  sleep 10ms                    sleep 10ms                    CAS(100 -> 101) FAIL
  read currentBlock -> 101      read currentBlock -> 101      discard (stale)
  fetch block 101...            fetch block 101...            sleep 10ms
                                                              read currentBlock -> 101
                                                              fetch block 101...
```

### Why CAS is Safe

`CompareAndSwapUint64(&currentBlock, old, new)` only succeeds if `currentBlock == old` at the moment of the swap. This guarantees:

1. **No double-processing**: Only one goroutine can successfully CAS from N to N+1
2. **No backwards movement**: CAS(100, 101) fails if currentBlock is already 101+
3. **No skipping**: We only increment by 1, so every block gets processed
4. **Race-free**: Atomic operations handle all concurrency

### Benefits Over Single-Loop

1. **3x faster block discovery**: All nodes polled in parallel
2. **True redundancy**: If node1 is slow, node2 or node3 can find the block independently
3. **Independent pacing**: Each node gets its own 10ms rest period
4. **No sequential bottleneck**: Slow node doesn't block checking other nodes

### Tradeoffs

1. **More goroutines**: One per node (typically 3-5, negligible)
2. **Wasted work**: Multiple nodes may fetch same block, only one wins. Acceptable since block data is cached/fast.
3. **Slightly more complex**: CAS logic vs simple increment

### Implementation Notes

```go
type TxListener struct {
    clients       []*ethclient.Client
    currentBlock  uint64  // atomic - the block we're looking for
    // ... rest unchanged
}

func (l *TxListener) pollBlocks() {
    // Initialize currentBlock from any available node
    
    // Spawn one goroutine per client
    for i, client := range l.clients {
        go l.pollNode(client, i)
    }
}

func (l *TxListener) pollNode(client *ethclient.Client, nodeIndex int) {
    for {
        select {
        case <-l.stopCh:
            return
        default:
        }
        
        targetBlock := atomic.LoadUint64(&l.currentBlock)
        
        found, blockData := l.tryFetchBlock(client, targetBlock)
        if found {
            // Try to claim this block
            if atomic.CompareAndSwapUint64(&l.currentBlock, targetBlock, targetBlock+1) {
                // We won the race - process this block
                l.processBlockData(targetBlock, blockData)
            }
            // If CAS failed, someone else processed it - that's fine
        }
        
        time.Sleep(pollInterval)
    }
}
```
