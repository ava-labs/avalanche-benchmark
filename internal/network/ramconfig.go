package network

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// RAMConfig holds calculated cache sizes based on available RAM
type RAMConfig struct {
	TotalRAM                  uint64 `json:"totalRam"`
	TrieCleanCache            int    `json:"trieCleanCache"`
	TrieDirtyCache            int    `json:"trieDirtyCache"`
	TrieDirtyCommitTarget     int    `json:"trieDirtyCommitTarget"`
	TriePrefetcherParallelism int    `json:"triePrefetcherParallelism"`
	SnapshotCache             int    `json:"snapshotCache"`
	AcceptedCacheSize         int    `json:"acceptedCacheSize"`
	TxPoolGlobalSlots         int    `json:"txPoolGlobalSlots"`
	TxPoolGlobalQueue         int    `json:"txPoolGlobalQueue"`
	TxPoolAccountSlots        int    `json:"txPoolAccountSlots"`
	TxPoolAccountQueue        int    `json:"txPoolAccountQueue"`
}

// DetectRAM returns total system RAM in bytes
func DetectRAM() (uint64, error) {
	switch runtime.GOOS {
	case "linux":
		return detectRAMLinux()
	case "darwin":
		return detectRAMDarwin()
	default:
		return 0, fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func detectRAMLinux() (uint64, error) {
	// Read from /proc/meminfo
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("failed to read /proc/meminfo: %w", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseUint(fields[1], 10, 64)
				if err != nil {
					return 0, fmt.Errorf("failed to parse MemTotal: %w", err)
				}
				return kb * 1024, nil // Convert KB to bytes
			}
		}
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

func detectRAMDarwin() (uint64, error) {
	// On macOS, we can't easily get RAM without cgo or external commands
	// For local development/testing, return a reasonable default
	// Production benchmarks run on Linux
	return 16 * 1024 * 1024 * 1024, nil // 16GB default for macOS dev
}

// CalculateRAMConfig calculates optimal cache sizes based on available RAM
// Uses approximately 40% of total RAM for caches, leaving room for OS, tx pool, and other processes
func CalculateRAMConfig(totalRAMBytes uint64) *RAMConfig {
	totalRAMMB := totalRAMBytes / (1024 * 1024)
	totalRAMGB := totalRAMMB / 1024

	cfg := &RAMConfig{
		TotalRAM: totalRAMBytes,
	}

	// Cache allocation strategy:
	// - Use ~40% of RAM for trie/snapshot caches
	// - Split: 35% trie-clean, 35% trie-dirty, 30% snapshot
	// - Reserve rest for OS, tx pool, and overhead

	cacheRAMMB := totalRAMMB * 40 / 100

	// Trie caches: 35% each of cache budget
	cfg.TrieCleanCache = int(cacheRAMMB * 35 / 100)
	cfg.TrieDirtyCache = int(cacheRAMMB * 35 / 100)

	// Snapshot cache: 30% of cache budget
	cfg.SnapshotCache = int(cacheRAMMB * 30 / 100)

	// Trie dirty commit target: ~1% of trie-dirty-cache, min 20MB
	cfg.TrieDirtyCommitTarget = max(20, cfg.TrieDirtyCache/100)

	// Prefetcher parallelism: scale with cores, cap at 128
	numCPU := runtime.NumCPU()
	cfg.TriePrefetcherParallelism = min(128, max(20, numCPU*4))

	// Accepted cache size: scale with RAM
	cfg.AcceptedCacheSize = min(1024, max(32, int(totalRAMGB)*8))

	// TX Pool sizing - scale aggressively with RAM for benchmarking
	// These determine how many transactions can be pending
	switch {
	case totalRAMGB >= 512: // 512GB+ (1TB machine)
		cfg.TxPoolGlobalSlots = 1048576  // 1M slots
		cfg.TxPoolGlobalQueue = 524288   // 512K queue
		cfg.TxPoolAccountSlots = 1024
		cfg.TxPoolAccountQueue = 4096
	case totalRAMGB >= 256: // 256-512GB (384GB machine)
		cfg.TxPoolGlobalSlots = 524288   // 512K slots
		cfg.TxPoolGlobalQueue = 262144   // 256K queue
		cfg.TxPoolAccountSlots = 512
		cfg.TxPoolAccountQueue = 2048
	case totalRAMGB >= 128: // 128-256GB
		cfg.TxPoolGlobalSlots = 262144   // 256K slots
		cfg.TxPoolGlobalQueue = 131072   // 128K queue
		cfg.TxPoolAccountSlots = 256
		cfg.TxPoolAccountQueue = 1024
	case totalRAMGB >= 64: // 64-128GB
		cfg.TxPoolGlobalSlots = 131072   // 128K slots
		cfg.TxPoolGlobalQueue = 65536    // 64K queue
		cfg.TxPoolAccountSlots = 128
		cfg.TxPoolAccountQueue = 512
	case totalRAMGB >= 32: // 32-64GB
		cfg.TxPoolGlobalSlots = 65536    // 64K slots
		cfg.TxPoolGlobalQueue = 32768    // 32K queue
		cfg.TxPoolAccountSlots = 64
		cfg.TxPoolAccountQueue = 256
	default: // < 32GB
		cfg.TxPoolGlobalSlots = 32768    // 32K slots
		cfg.TxPoolGlobalQueue = 16384    // 16K queue
		cfg.TxPoolAccountSlots = 32
		cfg.TxPoolAccountQueue = 128
	}

	return cfg
}

// GenerateChainConfig generates a complete chain config JSON with RAM-optimized settings
func GenerateChainConfig(ramCfg *RAMConfig) ([]byte, error) {
	config := map[string]interface{}{
		// Database
		"database-type": "pebbledb",

		// Block timing - minimal delay for benchmarking
		"min-delay-target": 200000000, // 200ms in nanoseconds

		// State management
		"pruning-enabled":   false,
		"commit-interval":   16384,
		"accepted-queue-limit": 256,

		// RAM-optimized caches
		"trie-clean-cache":           ramCfg.TrieCleanCache,
		"trie-dirty-cache":           ramCfg.TrieDirtyCache,
		"trie-dirty-commit-target":   ramCfg.TrieDirtyCommitTarget,
		"trie-prefetcher-parallelism": ramCfg.TriePrefetcherParallelism,
		"snapshot-cache":             ramCfg.SnapshotCache,
		"accepted-cache-size":        ramCfg.AcceptedCacheSize,

		// RAM-optimized TX pool
		"tx-pool-price-limit":   1,
		"tx-pool-price-bump":    1,
		"tx-pool-account-slots": ramCfg.TxPoolAccountSlots,
		"tx-pool-global-slots":  ramCfg.TxPoolGlobalSlots,
		"tx-pool-account-queue": ramCfg.TxPoolAccountQueue,
		"tx-pool-global-queue":  ramCfg.TxPoolGlobalQueue,
		"tx-pool-lifetime":      "1h",

		// Gossip - aggressive for fast tx propagation
		"push-gossip-frequency":        "50ms",
		"pull-gossip-frequency":        "500ms",
		"regossip-frequency":           "10s",
		"push-gossip-num-validators":   200,
		"push-regossip-num-validators": 50,
		"push-gossip-percent-stake":    0.5,

		// RPC settings
		"rpc-gas-cap":              500000000,
		"rpc-tx-fee-cap":           1000000,
		"batch-request-limit":      10000,
		"batch-response-max-size":  104857600,

		// Network
		"max-outbound-active-requests": 64,

		// Indexing - disabled for benchmark speed
		"skip-tx-indexing":     true,
		"transaction-history":  0,

		// Performance flags
		"metrics-expensive-enabled":       false,
		"allow-unfinalized-queries":       true,
		"allow-unprotected-txs":           true,
		"local-txs-enabled":               true,
		"snapshot-wait":                   false,
		"snapshot-verification-enabled":   false,
		"populate-missing-tries":          nil,
		"populate-missing-tries-parallelism": 4096,
		"state-sync-enabled":              false,
		"preimages":                       false,

		// Logging
		"log-level": "warn",
	}

	return json.MarshalIndent(config, "", "  ")
}

// PrintRAMConfig prints the RAM configuration in a human-readable format
func (r *RAMConfig) String() string {
	totalGB := float64(r.TotalRAM) / (1024 * 1024 * 1024)
	totalCacheMB := r.TrieCleanCache + r.TrieDirtyCache + r.SnapshotCache
	totalCacheGB := float64(totalCacheMB) / 1024

	return fmt.Sprintf(`RAM Configuration:
  Total System RAM:     %.1f GB

  Cache Allocation (%.1f GB total, %.0f%% of RAM):
    trie-clean-cache:   %d MB
    trie-dirty-cache:   %d MB
    snapshot-cache:     %d MB
    trie-dirty-commit:  %d MB
    prefetcher-parallel: %d
    accepted-cache:     %d

  TX Pool:
    global-slots:       %d
    global-queue:       %d
    account-slots:      %d
    account-queue:      %d`,
		totalGB,
		totalCacheGB, (totalCacheGB/totalGB)*100,
		r.TrieCleanCache,
		r.TrieDirtyCache,
		r.SnapshotCache,
		r.TrieDirtyCommitTarget,
		r.TriePrefetcherParallelism,
		r.AcceptedCacheSize,
		r.TxPoolGlobalSlots,
		r.TxPoolGlobalQueue,
		r.TxPoolAccountSlots,
		r.TxPoolAccountQueue,
	)
}
