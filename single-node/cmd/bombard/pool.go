package main

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"

	"github.com/ava-labs/libevm/ethclient"
)

// WorkerPool manages a set of active workers.
type WorkerPool struct {
	client      *ethclient.Client
	chainID     *big.Int
	keyPool     *KeyPool
	blockSignal *atomic.Uint64
	workers     []*Worker
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// NewWorkerPool creates a worker pool.
func NewWorkerPool(client *ethclient.Client, chainID *big.Int, keyPool *KeyPool, blockSignal *atomic.Uint64) *WorkerPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &WorkerPool{
		client:      client,
		chainID:     chainID,
		keyPool:     keyPool,
		blockSignal: blockSignal,
		workers:     make([]*Worker, 0),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Start activates the initial set of workers.
func (p *WorkerPool) Start(count int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	fmt.Printf("Starting %d workers...\n", count)

	for i := 0; i < count; i++ {
		if err := p.activateWorkerLocked(i); err != nil {
			return fmt.Errorf("failed to activate worker %d: %w", i, err)
		}
	}

	return nil
}

// activateWorkerLocked creates and starts a new worker. Must be called with lock held.
func (p *WorkerPool) activateWorkerLocked(keyIndex int) error {
	if keyIndex >= p.keyPool.Len() {
		return fmt.Errorf("no more keys available in pool")
	}

	key := p.keyPool.Get(keyIndex)

	// Pick a random target from the pool
	target := p.keyPool.RandomTarget(keyIndex)

	worker := NewWorker(key, p.client, p.chainID, target, p.blockSignal)
	p.workers = append(p.workers, worker)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		worker.Run(p.ctx)
	}()

	return nil
}

// Stop stops all workers and waits for them to finish.
func (p *WorkerPool) Stop() {
	p.cancel()
	p.wg.Wait()
}

// Stats returns aggregate statistics from all workers.
func (p *WorkerPool) Stats() (sent, resent, errors, dead uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, w := range p.workers {
		s, r, e := w.Stats()
		sent += s
		resent += r
		errors += e
	}

	return sent, resent, errors, 0 // dead is always 0 now
}

// ActiveCount returns the number of workers.
func (p *WorkerPool) ActiveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.workers)
}
