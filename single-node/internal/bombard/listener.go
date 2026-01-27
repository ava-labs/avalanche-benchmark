package bombard

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethclient"
)

// TxListener tracks transactions in new blocks via WebSocket subscription.
type TxListener struct {
	client                *ethclient.Client
	minedTxs              map[string]bool
	minedTxsMu            sync.RWMutex
	pendingTxs            map[string]chan struct{}
	pendingTxsMu          sync.Mutex
	stopCh                chan struct{}
	lastBlockTimestampMs  uint64
	hasLastBlockTimestamp bool
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
	// Fetch block as raw JSON to get both transactions and timestampMilliseconds in one call
	var raw json.RawMessage
	var err error
	for i := 0; i < 5; i++ {
		err = l.client.Client().CallContext(context.Background(), &raw, "eth_getBlockByNumber", fmt.Sprintf("0x%x", header.Number.Uint64()), true)
		if err == nil && len(raw) > 0 && string(raw) != "null" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		log.Printf("failed to get block %d: %v", header.Number.Uint64(), err)
		return
	}

	var blockData map[string]interface{}
	if err := json.Unmarshal(raw, &blockData); err != nil {
		log.Printf("failed to parse block %d: %v", header.Number.Uint64(), err)
		return
	}

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
	if tsMsHex, ok := blockData["timestampMilliseconds"].(string); ok {
		if timestampMs, err := parseUint64String(tsMsHex); err == nil {
			if l.hasLastBlockTimestamp && timestampMs >= l.lastBlockTimestampMs {
				delayLabel = fmt.Sprintf("%d", timestampMs-l.lastBlockTimestampMs)
			}
			l.lastBlockTimestampMs = timestampMs
			l.hasLastBlockTimestamp = true
		}
	}

	log.Printf(
		"New block: %d, tx count: %d, gas used: %.2fM, delay_ms: %s",
		header.Number.Uint64(),
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
		return strconv.ParseUint(strings.TrimPrefix(value, "0x"), 16, 64)
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
