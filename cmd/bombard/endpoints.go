package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ava-labs/libevm/ethclient"
)

type endpointStatus struct {
	URL       string
	Alive     bool
	LastError string
	Active    bool
}

type endpointManager struct {
	rpcs    []string
	wsConns int

	mu       sync.RWMutex
	active   int
	alive    []bool
	lastErr  []string
	pool     *wsPool
	failover uint64

	switchMu sync.Mutex
}

func newEndpointManager(ctx context.Context, rpcs []string, activeIndex int, wsConns int) (*endpointManager, error) {
	m := &endpointManager{
		rpcs:    append([]string(nil), rpcs...),
		wsConns: wsConns,
		active:  activeIndex,
		alive:   make([]bool, len(rpcs)),
		lastErr: make([]string, len(rpcs)),
	}
	m.alive[activeIndex] = true
	if err := m.rebuildPool(ctx, activeIndex, false); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *endpointManager) Close() {
	m.mu.Lock()
	pool := m.pool
	m.pool = nil
	m.mu.Unlock()
	if pool != nil {
		pool.Close()
	}
}

func (m *endpointManager) activeSnapshot() (int, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active < 0 || m.active >= len(m.rpcs) {
		return -1, ""
	}
	return m.active, m.rpcs[m.active]
}

func (m *endpointManager) activeWS() string {
	_, rpcURL := m.activeSnapshot()
	if rpcURL == "" {
		return ""
	}
	return httpRPCToWS(rpcURL)
}

func (m *endpointManager) statuses() ([]endpointStatus, uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]endpointStatus, len(m.rpcs))
	for i, rpcURL := range m.rpcs {
		out[i] = endpointStatus{
			URL:       rpcURL,
			Alive:     m.alive[i],
			LastError: m.lastErr[i],
			Active:    i == m.active,
		}
	}
	return out, m.failover
}

func (m *endpointManager) Do(ctx context.Context, fn func(*ethclient.Client) error) error {
	m.mu.RLock()
	pool := m.pool
	active := m.active
	m.mu.RUnlock()
	if pool == nil {
		return errors.New("no active endpoint pool")
	}
	err := pool.Do(ctx, fn)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		m.markDown(active, err)
		m.failoverFrom(ctx, active)
	}
	return err
}

func (m *endpointManager) markDown(index int, err error) {
	if index < 0 || index >= len(m.rpcs) {
		return
	}
	m.mu.Lock()
	m.alive[index] = false
	m.lastErr[index] = err.Error()
	m.mu.Unlock()
}

func (m *endpointManager) probeLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.probeOnce(ctx)
		}
	}
}

func (m *endpointManager) probeOnce(ctx context.Context) {
	type result struct {
		index int
		err   error
	}
	results := make(chan result, len(m.rpcs))
	var wg sync.WaitGroup
	for i, rpcURL := range m.rpcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := probeChainID(ctx, rpcURL)
			results <- result{index: i, err: err}
		}()
	}
	wg.Wait()
	close(results)

	m.mu.Lock()
	for res := range results {
		if res.err == nil {
			m.alive[res.index] = true
			m.lastErr[res.index] = ""
		} else {
			m.alive[res.index] = false
			m.lastErr[res.index] = res.err.Error()
		}
	}
	active := m.active
	activeDown := active >= 0 && active < len(m.alive) && !m.alive[active]
	m.mu.Unlock()

	if activeDown {
		m.failoverFrom(ctx, active)
	}
}

func (m *endpointManager) failoverFrom(ctx context.Context, failedIndex int) {
	m.switchMu.Lock()
	defer m.switchMu.Unlock()

	m.mu.RLock()
	if m.active != failedIndex {
		m.mu.RUnlock()
		return
	}
	next := -1
	for i, alive := range m.alive {
		if alive {
			next = i
			break
		}
	}
	m.mu.RUnlock()

	if next == -1 || next == failedIndex {
		return
	}
	if err := m.rebuildPool(ctx, next, true); err != nil {
		m.markDown(next, err)
		return
	}
}

func (m *endpointManager) rebuildPool(ctx context.Context, activeIndex int, countFailover bool) error {
	pool, err := newWSPool(ctx, httpRPCToWS(m.rpcs[activeIndex]), m.wsConns)
	if err != nil {
		return fmt.Errorf("build WS pool for %s: %w", m.rpcs[activeIndex], err)
	}

	m.mu.Lock()
	oldPool := m.pool
	m.pool = pool
	m.active = activeIndex
	m.alive[activeIndex] = true
	m.lastErr[activeIndex] = ""
	if countFailover {
		m.failover++
	}
	m.mu.Unlock()

	if oldPool != nil {
		oldPool.Close()
	}
	return nil
}

type targetRate struct {
	value atomic.Int64
}

func newTargetRate(startingTPS int) *targetRate {
	r := &targetRate{}
	r.value.Store(int64(startingTPS))
	return r
}

func (r *targetRate) get() int {
	return int(r.value.Load())
}

func (r *targetRate) set(next int) {
	if next < minTPS {
		next = minTPS
	}
	if next > maxTPS {
		next = maxTPS
	}
	r.value.Store(int64(next))
}

func (r *targetRate) adjust(delta int) {
	r.set(r.get() + delta)
}

func tpsStep(current int) int {
	if current < 1000 {
		return 100
	}
	return 500
}

func batchSizeForTPS(tps int) int {
	batchSize := tps * int(workerDelay/time.Millisecond) / 1000
	if batchSize < 1 {
		return 1
	}
	return batchSize
}
