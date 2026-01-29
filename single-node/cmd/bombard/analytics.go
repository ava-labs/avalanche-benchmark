package main

import (
	"sync"
)

// PeriodStats holds metrics for a 10-second period
type PeriodStats struct {
	StartMs         uint64   // Period start timestamp (ms)
	EndMs           uint64   // Period end timestamp (ms)
	TotalTxs        uint64   // Total transactions in period
	TotalGasUsed    uint64   // Total gas used in period
	BlockCount      int      // Number of blocks in period
	BlockIntervals  []int    // Intervals between consecutive blocks (ms)
	blockTimestamps []uint64 // Raw timestamps for interval calculation
}

// TPS returns transactions per second for this period
func (p *PeriodStats) TPS() int {
	durationMs := p.EndMs - p.StartMs
	if durationMs == 0 {
		return 0
	}
	return int(float64(p.TotalTxs) / (float64(durationMs) / 1000.0))
}

// AvgBlockIntervalMs returns average block interval in ms
func (p *PeriodStats) AvgBlockIntervalMs() int {
	if len(p.BlockIntervals) == 0 {
		return 0
	}
	total := 0
	for _, iv := range p.BlockIntervals {
		total += iv
	}
	return total / len(p.BlockIntervals)
}

// MedianBlockIntervalMs returns median block interval in ms
func (p *PeriodStats) MedianBlockIntervalMs() int {
	if len(p.BlockIntervals) == 0 {
		return 0
	}
	// Simple median - copy and sort
	sorted := make([]int, len(p.BlockIntervals))
	copy(sorted, p.BlockIntervals)
	// Simple bubble sort (small slice)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return sorted[len(sorted)/2]
}

// AvgGasUsed returns average gas used per block
func (p *PeriodStats) AvgGasUsed() uint64 {
	if p.BlockCount == 0 {
		return 0
	}
	return p.TotalGasUsed / uint64(p.BlockCount)
}

// PeriodDurationMs is the fixed period length
const PeriodDurationMs = 10000 // 10 seconds

// Analytics tracks block metrics in 10-second periods based on block timestamps
type Analytics struct {
	mu sync.RWMutex

	// Epoch start - timestamp of first block, all periods are relative to this
	epochStartMs uint64

	// Periods indexed by period number
	// Period N covers [epochStartMs + N*10000, epochStartMs + (N+1)*10000)
	periods map[int]*PeriodStats

	// Current period number
	currentPeriod int

	// Running totals
	totalTxsConfirmed uint64
}

// blockData holds data for a single block
type blockData struct {
	timestampMs uint64
	txCount     uint64
	gasUsed     uint64
}

// NewAnalytics creates a new analytics tracker
func NewAnalytics() *Analytics {
	return &Analytics{
		periods: make(map[int]*PeriodStats),
	}
}

// RecordBlock records a new block into the appropriate period.
func (a *Analytics) RecordBlock(timestampMs, txCount, gasUsed, gasLimit uint64) {
	if timestampMs == 0 {
		return // Invalid timestamp
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.totalTxsConfirmed += txCount

	// First block ever - set epoch start
	if a.epochStartMs == 0 {
		a.epochStartMs = timestampMs
	}

	// Calculate which period this block belongs to
	periodNum := int((timestampMs - a.epochStartMs) / PeriodDurationMs)
	if periodNum < 0 {
		periodNum = 0 // Shouldn't happen, but be safe
	}

	// Update current period tracker
	if periodNum > a.currentPeriod {
		a.currentPeriod = periodNum
	}

	// Get or create the period
	period, exists := a.periods[periodNum]
	if !exists {
		periodStart := a.epochStartMs + uint64(periodNum)*PeriodDurationMs
		period = &PeriodStats{
			StartMs: periodStart,
			EndMs:   periodStart + PeriodDurationMs,
		}
		a.periods[periodNum] = period
	}

	// Record the block's timestamp for interval calculation
	period.blockTimestamps = append(period.blockTimestamps, timestampMs)
	period.TotalTxs += txCount
	period.TotalGasUsed += gasUsed
	period.BlockCount++

	// Recalculate intervals from timestamps
	period.BlockIntervals = nil
	if len(period.blockTimestamps) >= 2 {
		for i := 1; i < len(period.blockTimestamps); i++ {
			interval := int(period.blockTimestamps[i] - period.blockTimestamps[i-1])
			if interval > 0 {
				period.BlockIntervals = append(period.BlockIntervals, interval)
			}
		}
	}

	// Cleanup old periods (keep last 100)
	if len(a.periods) > 100 {
		minPeriod := a.currentPeriod - 100
		for k := range a.periods {
			if k < minPeriod {
				delete(a.periods, k)
			}
		}
	}
}

// LatestSealedPeriod returns the most recently completed (sealed) period stats.
// A period is sealed when we've moved past it (current period > that period).
func (a *Analytics) LatestSealedPeriod() *PeriodStats {
	a.mu.RLock()
	defer a.mu.RUnlock()

	// The latest sealed period is currentPeriod - 1
	if a.currentPeriod < 1 {
		return nil
	}

	period, exists := a.periods[a.currentPeriod-1]
	if !exists {
		return nil
	}

	// Return copy to avoid race
	p := *period
	return &p
}

// TPSHistory returns TPS values for sealed periods (for chart).
// Returns one value per period, in order.
func (a *Analytics) TPSHistory() []int {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.currentPeriod < 1 {
		return nil
	}

	// Get all sealed periods (0 to currentPeriod-1)
	history := make([]int, 0, a.currentPeriod)
	for i := 0; i < a.currentPeriod; i++ {
		period, exists := a.periods[i]
		if exists {
			history = append(history, period.TPS())
		} else {
			history = append(history, 0) // Empty period
		}
	}
	return history
}

// TotalConfirmed returns total transactions confirmed across all time
func (a *Analytics) TotalConfirmed() uint64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.totalTxsConfirmed
}

// CurrentPeriodNum returns the current period number
func (a *Analytics) CurrentPeriodNum() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.currentPeriod
}
