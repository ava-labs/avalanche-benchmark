package bombard

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ava-labs/libevm/ethclient"
)

const pollInterval = 10 * time.Millisecond

// TxListener tracks transactions in new blocks via HTTP polling across multiple nodes.
// Each node gets its own polling goroutine. They coordinate via atomic CAS on currentBlock.
type TxListener struct {
	clients               []*ethclient.Client
	currentBlock          uint64 // atomic - the block we're looking for
	minedTxs              map[string]bool
	minedTxsMu            sync.RWMutex
	pendingTxs            map[string]chan struct{}
	pendingTxsMu          sync.Mutex
	stopCh                chan struct{}
	lastBlockTimestampMs  uint64
	hasLastBlockTimestamp bool
	timestampMu           sync.Mutex // protects timestamp fields
}

// NewTxListener creates a polling-based listener across multiple RPC endpoints.
// Each endpoint gets its own polling goroutine for parallel block discovery.
func NewTxListener(rpcURLs []string) (*TxListener, error) {
	if len(rpcURLs) == 0 {
		return nil, fmt.Errorf("no RPC URLs provided")
	}

	clients := make([]*ethclient.Client, len(rpcURLs))
	for i, url := range rpcURLs {
		client, err := ethclient.Dial(url)
		if err != nil {
			// Clean up already created clients
			for j := 0; j < i; j++ {
				clients[j].Close()
			}
			return nil, fmt.Errorf("failed to connect to RPC %s: %w", url, err)
		}
		clients[i] = client
	}

	listener := &TxListener{
		clients:    clients,
		minedTxs:   make(map[string]bool),
		pendingTxs: make(map[string]chan struct{}),
		stopCh:     make(chan struct{}),
	}

	// Initialize currentBlock from first available node
	var startBlock uint64
	for i, client := range clients {
		block, err := client.BlockNumber(context.Background())
		if err == nil {
			startBlock = block + 1 // Start looking for the next block
			log.Printf("Listener initialized at block %d from node %d", block, i)
			break
		}
	}
	if startBlock == 0 {
		startBlock = 1 // Fallback
	}
	atomic.StoreUint64(&listener.currentBlock, startBlock)

	// Spawn one polling goroutine per node
	for i, client := range clients {
		go listener.pollNode(client, i)
	}

	log.Printf("TxListener started with %d nodes, polling every %v", len(clients), pollInterval)

	return listener, nil
}

// Close stops all polling goroutines and closes connections.
func (l *TxListener) Close() {
	close(l.stopCh)
	for _, client := range l.clients {
		client.Close()
	}
}

// pollNode continuously polls a single node for blocks.
// Multiple goroutines run this concurrently, coordinating via atomic CAS.
func (l *TxListener) pollNode(client *ethclient.Client, nodeIndex int) {
	for {
		select {
		case <-l.stopCh:
			return
		default:
		}

		targetBlock := atomic.LoadUint64(&l.currentBlock)

		// Try to fetch the target block
		blockData, err := l.fetchBlock(client, targetBlock)
		if err == nil && blockData != nil {
			// Found the block! Try to claim it with CAS
			if atomic.CompareAndSwapUint64(&l.currentBlock, targetBlock, targetBlock+1) {
				// We won the race - process this block
				l.processBlockData(targetBlock, blockData, nodeIndex)
			}
			// If CAS failed, another goroutine already processed it - that's fine
		}

		time.Sleep(pollInterval)
	}
}

// fetchBlock attempts to fetch a block by number. Returns nil if not found.
func (l *TxListener) fetchBlock(client *ethclient.Client, blockNum uint64) (map[string]interface{}, error) {
	var raw json.RawMessage
	err := client.Client().CallContext(
		context.Background(),
		&raw,
		"eth_getBlockByNumber",
		fmt.Sprintf("0x%x", blockNum),
		true, // include full transactions
	)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil // Block not found yet
	}

	var blockData map[string]interface{}
	if err := json.Unmarshal(raw, &blockData); err != nil {
		return nil, err
	}

	return blockData, nil
}

// processBlockData extracts transactions and notifies waiters.
func (l *TxListener) processBlockData(blockNum uint64, blockData map[string]interface{}, nodeIndex int) {
	// Extract transactions
	var txHashes []string
	if txs, ok := blockData["transactions"].([]interface{}); ok {
		for _, tx := range txs {
			if txMap, ok := tx.(map[string]interface{}); ok {
				if hash, ok := txMap["hash"].(string); ok {
					txHashes = append(txHashes, hash)
				}
			}
		}
	}
	txCount := len(txHashes)

	// Extract gasUsed
	var gasUsed uint64
	if gasUsedHex, ok := blockData["gasUsed"].(string); ok {
		gasUsed, _ = parseUint64String(gasUsedHex)
	}
	gasUsedMillions := float64(gasUsed) / 1_000_000.0

	// Extract timestampMilliseconds (Avalanche-specific)
	delayLabel := "n/a"
	l.timestampMu.Lock()
	if tsMsHex, ok := blockData["timestampMilliseconds"].(string); ok {
		if timestampMs, err := parseUint64String(tsMsHex); err == nil {
			if l.hasLastBlockTimestamp && timestampMs >= l.lastBlockTimestampMs {
				delayLabel = fmt.Sprintf("%d", timestampMs-l.lastBlockTimestampMs)
			}
			l.lastBlockTimestampMs = timestampMs
			l.hasLastBlockTimestamp = true
		}
	}
	l.timestampMu.Unlock()

	log.Printf(
		"New block: %d (node %d), tx count: %d, gas used: %.2fM, delay_ms: %s",
		blockNum,
		nodeIndex,
		txCount,
		gasUsedMillions,
		delayLabel,
	)

	// Mark transactions as mined
	l.minedTxsMu.Lock()
	for _, txHash := range txHashes {
		l.minedTxs[txHash] = true
	}
	l.minedTxsMu.Unlock()

	// Notify any waiters
	l.pendingTxsMu.Lock()
	for _, txHash := range txHashes {
		if ch, ok := l.pendingTxs[txHash]; ok {
			close(ch)
			delete(l.pendingTxs, txHash)
		}
	}
	l.pendingTxsMu.Unlock()
}

func parseUint64String(value string) (uint64, error) {
	if strings.HasPrefix(value, "0x") || strings.HasPrefix(value, "0X") {
		return strconv.ParseUint(strings.TrimPrefix(strings.ToLower(value), "0x"), 16, 64)
	}
	return strconv.ParseUint(value, 10, 64)
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
