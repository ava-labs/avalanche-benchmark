package bombard

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethclient"
)

// TxListener tracks transactions in new blocks via WebSocket subscription.
type TxListener struct {
	client       *ethclient.Client
	minedTxs     map[string]bool
	minedTxsMu   sync.RWMutex
	pendingTxs   map[string]chan struct{}
	pendingTxsMu sync.Mutex
	stopCh       chan struct{}
}

// NewTxListener creates a new transaction listener connected to the given WebSocket URL.
func NewTxListener(wsURL string) (*TxListener, error) {
	client, err := ethclient.Dial(wsURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to WebSocket: %w", err)
	}

	listener := &TxListener{
		client:     client,
		minedTxs:   make(map[string]bool),
		pendingTxs: make(map[string]chan struct{}),
		stopCh:     make(chan struct{}),
	}

	go listener.subscribeToBlocks()

	return listener, nil
}

// Close stops the listener and closes the connection.
func (l *TxListener) Close() {
	close(l.stopCh)
	l.client.Close()
}

func (l *TxListener) subscribeToBlocks() {
	headers := make(chan *types.Header)
	sub, err := l.client.SubscribeNewHead(context.Background(), headers)
	if err != nil {
		log.Printf("failed to subscribe to new heads: %v", err)
		return
	}
	defer sub.Unsubscribe()

	for {
		select {
		case <-l.stopCh:
			return
		case err := <-sub.Err():
			log.Printf("subscription error: %v", err)
			return
		case header := <-headers:
			l.processBlock(header)
		}
	}
}

func (l *TxListener) processBlock(header *types.Header) {
	// Retry fetching block with backoff - sometimes block isn't immediately available
	var block *types.Block
	var err error
	for i := 0; i < 5; i++ {
		block, err = l.client.BlockByHash(context.Background(), header.Hash())
		if err == nil {
			break
		}
		// Try by number as fallback
		block, err = l.client.BlockByNumber(context.Background(), header.Number)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		log.Printf("failed to get block %d: %v", header.Number.Uint64(), err)
		return
	}

	txCount := len(block.Transactions())
	log.Printf("New block: %d, tx count: %d", block.Number().Uint64(), txCount)

	l.minedTxsMu.Lock()
	for _, tx := range block.Transactions() {
		txHash := tx.Hash().String()
		l.minedTxs[txHash] = true
	}
	l.minedTxsMu.Unlock()

	// Notify any waiters
	l.pendingTxsMu.Lock()
	for _, tx := range block.Transactions() {
		txHash := tx.Hash().String()
		if ch, ok := l.pendingTxs[txHash]; ok {
			close(ch)
			delete(l.pendingTxs, txHash)
		}
	}
	l.pendingTxsMu.Unlock()
}

// AwaitTxMined waits for a transaction to be mined within the timeout.
func (l *TxListener) AwaitTxMined(txHash string, timeoutSec int) error {
	// Check if already mined
	l.minedTxsMu.RLock()
	if l.minedTxs[txHash] {
		l.minedTxsMu.RUnlock()
		return nil
	}
	l.minedTxsMu.RUnlock()

	// Create wait channel
	l.pendingTxsMu.Lock()
	ch := make(chan struct{})
	l.pendingTxs[txHash] = ch
	l.pendingTxsMu.Unlock()

	// Wait with timeout
	select {
	case <-ch:
		return nil
	case <-time.After(time.Duration(timeoutSec) * time.Second):
		l.pendingTxsMu.Lock()
		delete(l.pendingTxs, txHash)
		l.pendingTxsMu.Unlock()
		return fmt.Errorf("timeout waiting for transaction %s after %d seconds", txHash, timeoutSec)
	}
}
