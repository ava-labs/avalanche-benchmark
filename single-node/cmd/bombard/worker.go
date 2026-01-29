package main

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethclient"
)

// Worker sends exactly 1 transaction per block.
// It waits for block signal, sends tx, waits for next block.
type Worker struct {
	key         *Key
	client      *ethclient.Client
	chainID     *big.Int
	signer      types.Signer
	target      common.Address
	stats       *WorkerStats
	blockSignal *atomic.Uint64 // Shared block signal from listener
	lastBlock   uint64          // Last block this worker sent in
}

// WorkerStats tracks per-worker metrics.
type WorkerStats struct {
	Sent   atomic.Uint64
	Resent atomic.Uint64
	Errors atomic.Uint64
}

// NewWorker creates a worker for the given key.
func NewWorker(key *Key, client *ethclient.Client, chainID *big.Int, target common.Address, blockSignal *atomic.Uint64) *Worker {
	return &Worker{
		key:         key,
		client:      client,
		chainID:     chainID,
		signer:      types.NewEIP155Signer(chainID),
		target:      target,
		stats:       &WorkerStats{},
		blockSignal: blockSignal,
		lastBlock:   0,
	}
}

// Run starts the worker loop. Sends 1 tx per block.
func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Wait for a new block (signal changed), except first time
		currentBlock := w.blockSignal.Load()
		if w.lastBlock > 0 && currentBlock <= w.lastBlock {
			// Not the first tx, and no new block yet - wait
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// First tx or new block! Send one transaction
		w.lastBlock = currentBlock

		// Get fresh nonce from chain
		nonce, err := w.client.PendingNonceAt(ctx, w.key.Address)
		if err != nil {
			// fmt.Printf("Worker %s nonce fetch error: %v\n", w.key.Address.Hex()[:10], err)
			w.stats.Errors.Add(1)
			time.Sleep(1 * time.Second)
			continue
		}

		// Build and sign transaction
		tx := w.buildTx(nonce)
		signedTx, err := types.SignTx(tx, w.signer, w.key.PrivateKey)
		if err != nil {
			fmt.Printf("Worker %s sign error: %v\n", w.key.Address.Hex()[:10], err)
			w.stats.Errors.Add(1)
			continue
		}

		// Send transaction
		err = w.client.SendTransaction(ctx, signedTx)
		if err != nil {
			w.handleSendError(err)
			continue
		}

		w.stats.Sent.Add(1)
	}
}

func (w *Worker) buildTx(nonce uint64) *types.Transaction {
	return types.NewTransaction(
		nonce,
		w.target,
		big.NewInt(1), // 1 wei
		NativeGasLimit,
		big.NewInt(GasPrice),
		nil,
	)
}

// handleSendError handles errors from SendTransaction.
func (w *Worker) handleSendError(err error) {
	errStr := err.Error()

	// "nonce too low" - our cached nonce is stale, will refetch next iteration
	if strings.Contains(errStr, "nonce too low") {
		return
	}

	// "already known" - tx already in mempool, this is fine
	if strings.Contains(errStr, "already known") {
		w.stats.Sent.Add(1)
		return
	}

	// "insufficient funds" - key is broke, log but keep trying
	if strings.Contains(errStr, "insufficient funds") {
		// fmt.Printf("Worker %s insufficient funds\n", w.key.Address.Hex()[:10])
		w.stats.Errors.Add(1)
		time.Sleep(5 * time.Second) // Back off before retry
		return
	}

	// "replacement transaction underpriced" - skip this nonce
	if strings.Contains(errStr, "underpriced") {
		return
	}

	// Other errors - count and continue
	w.stats.Errors.Add(1)
}

// Stats returns the current stats.
func (w *Worker) Stats() (sent, resent, errors uint64) {
	return w.stats.Sent.Load(), w.stats.Resent.Load(), w.stats.Errors.Load()
}
