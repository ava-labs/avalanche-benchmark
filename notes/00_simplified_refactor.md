# Simplified Architecture Refactor

## Problem
Current architecture has too many commands and complexity:
- 8 commands: start, flood, stop-flood, monitor, shutdown, status, logs, flood-status
- State management across multiple commands
- Separate processes for network and flooding
- Complex TUI monitor
- User has to run multiple commands to get a benchmark running

## Requirements

### 1. Single `start` Command
One command that does EVERYTHING:
- **Cleanup**: `pkill -f avalanchego` on startup to kill any existing processes
- **Start nodes**: Start all avalanchego nodes (primary + L1 validators + L1 RPC)
- **Embedded monitor**: Print metrics every second (blocks, TPS, height, etc.) - NO fancy TUI, just simple prints
- **Ctrl+C handling**: When user hits Ctrl+C, kill all nodes and exit cleanly
- **No state files**: Don't save state to disk, just run and cleanup

**Usage:**
```bash
./benchmark start
# Starts nodes, shows metrics every second
# Ctrl+C to stop everything
```

### 2. Separate `bombard` Command
Keep existing `cmd/bombard` exactly as is:
- Standalone tool for flooding transactions
- User runs it separately in another terminal if they want to flood
- Already exists, no changes needed

## Simplifications

### Remove These:
- ❌ `flood` command (user runs `bombard` directly)
- ❌ `stop-flood` command (user Ctrl+C bombard)
- ❌ `monitor` command (built into start)
- ❌ `shutdown` command (Ctrl+C on start)
- ❌ `status` command (metrics shown in start)
- ❌ `logs` command (not needed)
- ❌ `flood-status` command (not needed)
- ❌ State file management (no persistence)
- ❌ `internal/flood/` package (don't need to manage bombard process)
- ❌ `internal/config/` package (flags only)
- ❌ Fancy TUI monitor (just simple prints)

### Keep These:
- ✅ `cmd/bombard/main.go` - standalone flooding tool
- ✅ `internal/bombard/` - transaction flooding logic
- ✅ `internal/network/network.go` - node management (simplified)
- ✅ `genesis.json` and `chain-config.json` - config files

## Architecture

### File Structure (New)
```
cmd/
  benchmark/
    main.go                    # ONE command: start
  bombard/
    main.go                    # Keep as-is
internal/
  network/
    network.go                 # Simplified: no state files, just start/stop
  bombard/                     # Keep all as-is
    *.go
genesis.json                   # Keep
chain-config.json              # Keep
```

### Flow

#### Start Command
```
1. pkill -f avalanchego (cleanup)
2. Start primary nodes
3. Create subnet/chain
4. Start L1 validators
5. Start L1 RPC nodes
6. Spawn goroutine: print metrics every 1 second
   - Block height
   - TPS (calculated from block changes)
   - Gas usage
   - Node count
7. Wait for Ctrl+C
8. Kill all node PIDs
9. Exit
```

#### Metrics Output (Simple)
```
Block 0 | TPS: 0 | Gas: 0/500M (0%)
Block 1 | TPS: 145 | Gas: 12M/500M (2.4%)
Block 2 | TPS: 892 | Gas: 45M/500M (9%)
Block 3 | TPS: 1534 | Gas: 78M/500M (15.6%)
^C
Shutting down...
```

## Code Changes

### cmd/benchmark/main.go
- Remove all commands except `start`
- `start` command:
  - pkill -f avalanchego at beginning
  - Call network.StartAndMonitor(cfg)
  - network.StartAndMonitor blocks until Ctrl+C
  - Returns list of PIDs to kill
  - Kill all PIDs on exit

### internal/network/network.go
- Remove: SaveState, LoadState, state file management
- Add: StartAndMonitor function that:
  - Starts all nodes (returns PIDs)
  - Starts metrics goroutine (prints every second)
  - Blocks until context cancelled
  - Returns PIDs for cleanup
- Simplify: Just return PIDs, no state struct

### internal/monitor/ 
- DELETE entire package
- Move simple metrics logic into network.go
- Just fetch block number every second and print

### internal/flood/
- DELETE entire package
- User runs bombard directly

### internal/config/
- DELETE entire package
- Use flags directly in main.go

## Benefits
1. **One command**: User runs `./benchmark start` and sees metrics
2. **Clean**: pkill ensures no leftover processes
3. **Simple**: No state files, no process management
4. **Clear**: Metrics printed every second, easy to understand
5. **Fast**: Ctrl+C kills everything immediately
6. **Separate concerns**: `bombard` is standalone, user controls it

## Migration
Old workflow:
```bash
./benchmark start
./benchmark flood
./benchmark monitor    # separate terminal
./benchmark shutdown
```

New workflow:
```bash
./benchmark start      # Shows metrics, Ctrl+C to stop
# In another terminal:
./bombard -rpc http://127.0.0.1:9650/ext/bc/CHAINID/rpc -keys 600 -batch 50
```

---

## Implementation Plan

### Phase 1: Cleanup (Delete Files)
1. **Delete** `internal/flood/flood.go` - No longer manage bombard as subprocess
2. **Delete** `internal/config/config.go` - Use flags directly
3. **Delete** `internal/monitor/monitor.go` - Replace with simple prints
4. **Delete** `config.example.json` - Not needed, use flags

### Phase 2: Simplify network.go
**File:** `internal/network/network.go`

Changes:
1. **Remove functions:**
   - `LoadState()` - no state files
   - `saveState()` - no state files
   - `CheckNodeHealth()` - move simple version into StartAndMonitor
   
2. **Keep functions:**
   - `Start()` - keep mostly as-is, returns Result with PIDs
   - `Stop()` - simplified, just kill PIDs (no state lookup)
   - `findAvalanchego()` 
   - `findPluginDir()`
   - `startNode()`
   - `startL1Node()`
   - All helpers

3. **Add new function:**
   ```go
   func StartAndMonitor(ctx context.Context, cfg Config) error {
     // 1. pkill -f avalanchego
     // 2. Call Start() to get Result with PIDs
     // 3. Launch metrics goroutine (prints every second)
     // 4. Wait for ctx.Done() (Ctrl+C)
     // 5. Kill all PIDs
     // 6. Return
   }
   ```

4. **Remove types:**
   - `State` struct - no persistence needed
   - Keep `Result` but simplify

### Phase 3: Rewrite cmd/benchmark/main.go
**File:** `cmd/benchmark/main.go`

Complete rewrite:
```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	
	"github.com/ava-labs/avalanche-benchmark/internal/network"
	"github.com/spf13/cobra"
)

var (
	genesisPath      string
	chainConfigPath  string
	dataDir          string
	primaryNodes     int
	l1Validators     int
	l1RPCs           int
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "benchmark",
		Short: "Avalanche L1 Benchmark",
		Long:  "Start a local Avalanche network and benchmark it.",
	}

	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start network and show metrics",
		RunE:  runStart,
	}
	
	startCmd.Flags().StringVar(&genesisPath, "genesis", "./genesis.json", "Genesis file")
	startCmd.Flags().StringVar(&chainConfigPath, "chain-config", "./chain-config.json", "Chain config file")
	startCmd.Flags().StringVar(&dataDir, "data-dir", "", "Data directory")
	startCmd.Flags().IntVar(&primaryNodes, "primary-nodes", 2, "Primary network nodes")
	startCmd.Flags().IntVar(&l1Validators, "l1-validators", 2, "L1 validator nodes")
	startCmd.Flags().IntVar(&l1RPCs, "l1-rpcs", 1, "L1 RPC nodes")
	
	rootCmd.AddCommand(startCmd)
	rootCmd.Execute()
}

func runStart(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg := network.Config{
		DataDir:              dataDir,
		GenesisPath:          genesisPath,
		ChainConfigPath:      chainConfigPath,
		PrimaryNodeCount:     primaryNodes,
		L1ValidatorNodeCount: l1Validators,
		L1RPCNodeCount:       l1RPCs,
	}

	return network.StartAndMonitor(ctx, cfg)
}
```

### Phase 4: Add Cleanup to network.go
**File:** `internal/network/network.go`

Add at start of `StartAndMonitor`:
```go
// Kill any existing avalanchego processes
exec.Command("pkill", "-f", "avalanchego").Run()
time.Sleep(1 * time.Second) // Wait for cleanup
```

### Phase 5: Add Simple Metrics
**File:** `internal/network/network.go`

Add metrics function:
```go
func printMetrics(ctx context.Context, rpcURL string) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	
	var lastBlock uint64
	var lastTime time.Time
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			block, txCount, gasUsed, gasLimit := getBlockInfo(rpcURL)
			
			// Calculate TPS
			var tps float64
			if lastBlock > 0 && block > lastBlock {
				elapsed := time.Since(lastTime).Seconds()
				if elapsed > 0 {
					tps = float64(txCount) / elapsed
				}
			}
			
			if block > lastBlock {
				lastBlock = block
				lastTime = time.Now()
			}
			
			// Print metrics
			gasPercent := float64(0)
			if gasLimit > 0 {
				gasPercent = float64(gasUsed) / float64(gasLimit) * 100
			}
			
			fmt.Printf("Block %d | TPS: %.0f | Gas: %d/%d (%.1f%%)\n", 
				block, tps, gasUsed/1e6, gasLimit/1e6, gasPercent)
		}
	}
}

func getBlockInfo(rpcURL string) (block uint64, txCount int, gasUsed uint64, gasLimit uint64) {
	// Simple RPC call to get latest block
	// Parse and return values
	// (copy from monitor.go)
}
```

### Files Modified Summary

**DELETE (4 files):**
- `internal/flood/flood.go`
- `internal/config/config.go` 
- `internal/monitor/monitor.go`
- `config.example.json`

**MODIFY (2 files):**
- `cmd/benchmark/main.go` - Complete rewrite (simple)
- `internal/network/network.go` - Add StartAndMonitor, remove state management

**KEEP UNCHANGED (7+ files):**
- `cmd/bombard/main.go`
- `internal/bombard/*.go` (all files)
- `genesis.json`
- `chain-config.json`
- `Makefile`
- `README.md` (will need updates)

### Testing Plan
1. Run `make build`
2. Run `./bin/benchmark start`
3. Verify nodes start
4. Verify metrics print every second
5. In another terminal: `./bin/bombard -rpc http://127.0.0.1:9750/ext/bc/CHAINID/rpc -keys 100 -batch 10`
6. Verify TPS increases
7. Ctrl+C on benchmark
8. Verify all nodes killed
9. Run `ps aux | grep avalanchego` to confirm cleanup

### Estimated Changes
- **Lines deleted:** ~2000 (flood, config, monitor, old main.go)
- **Lines added:** ~200 (new main.go, StartAndMonitor, simple metrics)
- **Net reduction:** ~1800 lines of code
- **Complexity reduction:** From 8 commands → 1 command
