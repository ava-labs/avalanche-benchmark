package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/ava-labs/libevm/common"
)

const (
	timeoutSLA     = 5 * time.Second
	ringBufferSize = 100000
)

type pendingEntry struct {
	sendStart   time.Time
	submittedAt time.Time
	workerID    int
	mined       time.Duration
	hasMined    bool
	confirm     time.Duration
	hasConfirm  bool
}

type latencySample struct {
	send    time.Duration
	mined   time.Duration
	confirm time.Duration
	total   time.Duration
}

type txTracker struct {
	mu        sync.Mutex
	pending   map[common.Hash]pendingEntry
	submitted uint64
	landed    uint64
	timeouts  uint64
	latencies []time.Duration

	ring     [ringBufferSize]latencySample
	ringHead int
	ringFull bool
}

func newTxTracker() *txTracker {
	return &txTracker{pending: make(map[common.Hash]pendingEntry)}
}

func (t *txTracker) markSubmitted(h common.Hash, workerID int, sendStart, sendEnd time.Time) {
	t.mu.Lock()
	t.pending[h] = pendingEntry{sendStart: sendStart, submittedAt: sendEnd, workerID: workerID}
	t.submitted++
	t.mu.Unlock()
}

func (t *txTracker) markLanded(h common.Hash, blockTime time.Time, observedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.pending[h]
	if !ok {
		return
	}
	send := e.submittedAt.Sub(e.sendStart)
	mined := blockTime.Sub(e.submittedAt)
	if mined < 0 {
		mined = 0
	}
	confirm := observedAt.Sub(e.submittedAt)
	if confirm < 0 {
		confirm = 0
	}
	total := send + confirm
	t.latencies = append(t.latencies, total)
	t.ring[t.ringHead] = latencySample{send: send, mined: mined, confirm: confirm, total: total}
	t.ringHead++
	if t.ringHead >= ringBufferSize {
		t.ringHead = 0
		t.ringFull = true
	}
	t.landed++
	delete(t.pending, h)
}

func (t *txTracker) markMined(h common.Hash, blockTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.pending[h]
	if !ok {
		return
	}
	mined := blockTime.Sub(e.submittedAt)
	if mined < 0 {
		mined = 0
	}
	e.mined = mined
	e.hasMined = true
	t.pending[h] = e
	t.maybeFinalizeLocked(h, e)
}

func (t *txTracker) markConfirmed(h common.Hash, observedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.pending[h]
	if !ok {
		return
	}
	confirm := observedAt.Sub(e.submittedAt)
	if confirm < 0 {
		confirm = 0
	}
	e.confirm = confirm
	e.hasConfirm = true
	t.pending[h] = e
	t.maybeFinalizeLocked(h, e)
}

func (t *txTracker) maybeFinalizeLocked(h common.Hash, e pendingEntry) {
	if !e.hasMined || !e.hasConfirm {
		return
	}
	send := e.submittedAt.Sub(e.sendStart)
	total := send + e.confirm
	t.latencies = append(t.latencies, total)
	t.ring[t.ringHead] = latencySample{send: send, mined: e.mined, confirm: e.confirm, total: total}
	t.ringHead++
	if t.ringHead >= ringBufferSize {
		t.ringHead = 0
		t.ringFull = true
	}
	t.landed++
	delete(t.pending, h)
}

func (t *txTracker) snapshotRing() ([]latencySample, uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	timeouts := t.timeouts
	if !t.ringFull {
		out := make([]latencySample, t.ringHead)
		copy(out, t.ring[:t.ringHead])
		return out, timeouts
	}
	out := make([]latencySample, ringBufferSize)
	copy(out, t.ring[t.ringHead:])
	copy(out[ringBufferSize-t.ringHead:], t.ring[:t.ringHead])
	return out, timeouts
}

func (t *txTracker) counts() (submitted, landed, timeouts uint64, pending int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.submitted, t.landed, t.timeouts, len(t.pending)
}

func fmtMs(d time.Duration) string {
	return fmt.Sprintf("%10d ms", d.Milliseconds())
}

func pctDur(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * (len(sorted) - 1) / 100
	return sorted[idx]
}

func meanDur(xs []time.Duration) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	var sum time.Duration
	for _, x := range xs {
		sum += x
	}
	return sum / time.Duration(len(xs))
}

func stddevDur(xs []time.Duration, mean time.Duration) time.Duration {
	if len(xs) < 2 {
		return 0
	}
	var sq float64
	m := float64(mean)
	for _, x := range xs {
		d := float64(x) - m
		sq += d * d
	}
	variance := sq / float64(len(xs)-1)
	return time.Duration(math.Sqrt(variance))
}

func (t *txTracker) printTableLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var prevLanded uint64
	prevAt := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		samples, timeouts := t.snapshotRing()
		if len(samples) == 0 {
			continue
		}

		t.mu.Lock()
		landed := t.landed
		t.mu.Unlock()
		now := time.Now()
		elapsed := now.Sub(prevAt).Seconds()
		var tps float64
		if elapsed > 0 {
			tps = float64(landed-prevLanded) / elapsed
		}
		prevLanded = landed
		prevAt = now

		sendXs := make([]time.Duration, len(samples))
		minedXs := make([]time.Duration, len(samples))
		confirmXs := make([]time.Duration, len(samples))
		totalXs := make([]time.Duration, len(samples))
		for i, s := range samples {
			sendXs[i] = s.send
			minedXs[i] = s.mined
			confirmXs[i] = s.confirm
			totalXs[i] = s.total
		}
		sortedSend := append([]time.Duration(nil), sendXs...)
		sortedMined := append([]time.Duration(nil), minedXs...)
		sortedConfirm := append([]time.Duration(nil), confirmXs...)
		sortedTotal := append([]time.Duration(nil), totalXs...)
		sort.Slice(sortedSend, func(i, j int) bool { return sortedSend[i] < sortedSend[j] })
		sort.Slice(sortedMined, func(i, j int) bool { return sortedMined[i] < sortedMined[j] })
		sort.Slice(sortedConfirm, func(i, j int) bool { return sortedConfirm[i] < sortedConfirm[j] })
		sort.Slice(sortedTotal, func(i, j int) bool { return sortedTotal[i] < sortedTotal[j] })

		meanSend := meanDur(sendXs)
		meanMined := meanDur(minedXs)
		meanConfirm := meanDur(confirmXs)
		meanTotal := meanDur(totalXs)

		fmt.Println()
		fmt.Println("═══════════════════════════════════════════════════════════════════════════════════════════════════════════════")
		fmt.Printf("  PERCENTILES (last %d TXs, timeouts=%d, tps=%.0f)\n", len(samples), timeouts, tps)
		fmt.Println("═══════════════════════════════════════════════════════════════════════════════════════════════════════════════")
		fmt.Println()
		fmt.Println("  ┌────────────────────┬───────────────┬───────────────┬───────────────┬───────────────┐")
		fmt.Println("  │ Metric             │  Send         │  Mined        │  Confirm      │  Total        │")
		fmt.Println("  ├────────────────────┼───────────────┼───────────────┼───────────────┼───────────────┤")
		fmt.Printf("  │ Min                │ %s │ %s │ %s │ %s │\n", fmtMs(sortedSend[0]), fmtMs(sortedMined[0]), fmtMs(sortedConfirm[0]), fmtMs(sortedTotal[0]))
		fmt.Printf("  │ Avg                │ %s │ %s │ %s │ %s │\n", fmtMs(meanSend), fmtMs(meanMined), fmtMs(meanConfirm), fmtMs(meanTotal))
		fmt.Printf("  │ Median (P50)       │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 50)), fmtMs(pctDur(sortedMined, 50)), fmtMs(pctDur(sortedConfirm, 50)), fmtMs(pctDur(sortedTotal, 50)))
		fmt.Printf("  │ P75                │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 75)), fmtMs(pctDur(sortedMined, 75)), fmtMs(pctDur(sortedConfirm, 75)), fmtMs(pctDur(sortedTotal, 75)))
		fmt.Printf("  │ P90                │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 90)), fmtMs(pctDur(sortedMined, 90)), fmtMs(pctDur(sortedConfirm, 90)), fmtMs(pctDur(sortedTotal, 90)))
		fmt.Printf("  │ P95                │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 95)), fmtMs(pctDur(sortedMined, 95)), fmtMs(pctDur(sortedConfirm, 95)), fmtMs(pctDur(sortedTotal, 95)))
		fmt.Printf("  │ P99                │ %s │ %s │ %s │ %s │\n", fmtMs(pctDur(sortedSend, 99)), fmtMs(pctDur(sortedMined, 99)), fmtMs(pctDur(sortedConfirm, 99)), fmtMs(pctDur(sortedTotal, 99)))
		fmt.Printf("  │ Max                │ %s │ %s │ %s │ %s │\n", fmtMs(sortedSend[len(sortedSend)-1]), fmtMs(sortedMined[len(sortedMined)-1]), fmtMs(sortedConfirm[len(sortedConfirm)-1]), fmtMs(sortedTotal[len(sortedTotal)-1]))
		fmt.Printf("  │ Std Dev            │ %s │ %s │ %s │ %s │\n", fmtMs(stddevDur(sendXs, meanSend)), fmtMs(stddevDur(minedXs, meanMined)), fmtMs(stddevDur(confirmXs, meanConfirm)), fmtMs(stddevDur(totalXs, meanTotal)))
		fmt.Println("  └────────────────────┴───────────────┴───────────────┴───────────────┴───────────────┘")
		fmt.Println()
	}
}

func (t *txTracker) reportLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		t.mu.Lock()
		now := time.Now()
		var expired []common.Hash
		for h, e := range t.pending {
			if now.Sub(e.submittedAt) > timeoutSLA {
				expired = append(expired, h)
			}
		}
		for _, h := range expired {
			e := t.pending[h]
			fmt.Printf("ERROR: tx %s worker=%d not mined after %.1fs\n",
				h.Hex(), e.workerID, now.Sub(e.submittedAt).Seconds())
			delete(t.pending, h)
			t.timeouts++
		}

		lats := t.latencies
		t.latencies = nil
		sub, land, to, pend := t.submitted, t.landed, t.timeouts, len(t.pending)
		t.mu.Unlock()

		if len(lats) > 0 {
			sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
			var sum time.Duration
			for _, l := range lats {
				sum += l
			}
			mean := sum / time.Duration(len(lats))
			p50 := lats[len(lats)/2]
			max := lats[len(lats)-1]
			fmt.Printf("STATS submitted=%d landed=%d timeouts=%d pending=%d | total-confirm-latency(last1s) n=%d mean=%v p50=%v max=%v\n",
				sub, land, to, pend, len(lats),
				mean.Round(time.Millisecond), p50.Round(time.Millisecond), max.Round(time.Millisecond))
		} else {
			fmt.Printf("STATS submitted=%d landed=%d timeouts=%d pending=%d | no landings this tick\n",
				sub, land, to, pend)
		}
	}
}

var tracker = newTxTracker()
