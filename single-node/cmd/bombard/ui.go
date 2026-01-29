package main

import (
	"context"
	"fmt"
	"time"
)

// UI handles console output
type UI struct {
	pool          *WorkerPool
	analytics     *Analytics
	lastPeriodNum int
}

// NewUI creates a new UI
func NewUI(pool *WorkerPool, analytics *Analytics) *UI {
	return &UI{
		pool:          pool,
		analytics:     analytics,
		lastPeriodNum: -1,
	}
}

// Run starts the UI loop (blocks until context cancelled)
func (u *UI) Run(ctx context.Context) {
	fmt.Println("Bombard running - press Ctrl+C to stop")
	fmt.Println()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			return

		case <-ticker.C:
			u.checkAndPrint()
		}
	}
}

func (u *UI) checkAndPrint() {
	currentPeriod := u.analytics.CurrentPeriodNum()

	// Only print when a new period is sealed
	if currentPeriod <= u.lastPeriodNum+1 {
		return
	}

	// Print stats for the newly sealed period
	u.lastPeriodNum = currentPeriod - 1
	period := u.analytics.LatestSealedPeriod()
	if period == nil {
		return
	}

	// Calculate stats
	tps := period.TPS()
	avgInterval := period.AvgBlockIntervalMs()
	medianInterval := period.MedianBlockIntervalMs()
	avgGas := period.AvgGasUsed()
	blocks := period.BlockCount

	sent, resent, errors, dead := u.pool.Stats()
	totalConfirmed := u.analytics.TotalConfirmed()
	activeWorkers := u.pool.ActiveCount()

	errRate := float64(0)
	if sent > 0 {
		errRate = float64(errors) / float64(sent) * 100
	}

	// Format interval string
	intervalStr := "n/a"
	if avgInterval > 0 {
		intervalStr = fmt.Sprintf("%d/%dms", avgInterval, medianInterval)
	}

	// Print one line for this period
	fmt.Printf("[Period %d] TPS: %d | Blocks: %d | Interval(avg/med): %s | Gas: %.1fM | Workers: %d | Sent: %s | Resent: %d | Conf: %s | Dead: %d | Err: %.2f%%\n",
		u.lastPeriodNum,
		tps,
		blocks,
		intervalStr,
		float64(avgGas)/1e6,
		activeWorkers,
		formatNum(sent),
		resent,
		formatNum(totalConfirmed),
		dead,
		errRate,
	)
}

func formatNum(n uint64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
