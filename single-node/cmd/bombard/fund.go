package main

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethclient"
)

// EWOQ is the pre-funded test key used by Avalanche local networks.
const ewoqPrivateKeyHex = "56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027"

// Funder handles funding of keys from the EWOQ account.
type Funder struct {
	client  *ethclient.Client
	chainID *big.Int
	nonce   uint64
	mu      sync.Mutex
}

// NewFunder creates a funder connected to the given client.
func NewFunder(client *ethclient.Client) (*Funder, error) {
	chainID, err := client.NetworkID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get chain ID: %w", err)
	}

	ewoqKey, err := crypto.HexToECDSA(ewoqPrivateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("parse EWOQ key: %w", err)
	}
	ewoqAddr := crypto.PubkeyToAddress(ewoqKey.PublicKey)

	nonce, err := client.PendingNonceAt(context.Background(), ewoqAddr)
	if err != nil {
		return nil, fmt.Errorf("get EWOQ nonce: %w", err)
	}

	return &Funder{
		client:  client,
		chainID: chainID,
		nonce:   nonce,
	}, nil
}

// minBalance is the threshold below which we fund a key (50 ETH)
// We fund 100 ETH, so refund when below 50
var minBalance = toWei(50)

// FundKeys funds multiple keys concurrently. Skips already-funded keys.
func (f *Funder) FundKeys(ctx context.Context, keys []*Key) error {
	if len(keys) == 0 {
		return nil
	}

	// Filter to only keys that need funding
	needsFunding := make([]*Key, 0, len(keys))
	alreadyFunded := 0
	for _, key := range keys {
		balance, err := f.client.BalanceAt(ctx, key.Address, nil)
		if err != nil {
			// If we can't check, assume it needs funding
			needsFunding = append(needsFunding, key)
			continue
		}
		if balance.Cmp(minBalance) < 0 {
			needsFunding = append(needsFunding, key)
		} else {
			alreadyFunded++
		}
	}

	if len(needsFunding) == 0 {
		fmt.Printf("All %d keys already funded (>50 ETH)\n", len(keys))
		return nil
	}

	fmt.Printf("Funding: %d need funding, %d already funded\n", len(needsFunding), alreadyFunded)

	ewoqKey, _ := crypto.HexToECDSA(ewoqPrivateKeyHex)
	signer := types.NewEIP155Signer(f.chainID)
	amount := toWei(FundAmountETH)
	gasPrice := big.NewInt(GasPrice)

	var wg sync.WaitGroup
	var errCount atomic.Int32

	// Send all funding txs
	txHashes := make([]string, len(needsFunding))

	f.mu.Lock()
	for i, key := range needsFunding {
		tx := types.NewTransaction(
			f.nonce,
			key.Address,
			amount,
			NativeGasLimit,
			gasPrice,
			nil,
		)

		signedTx, err := types.SignTx(tx, signer, ewoqKey)
		if err != nil {
			f.mu.Unlock()
			return fmt.Errorf("sign funding tx: %w", err)
		}

		if err := f.client.SendTransaction(ctx, signedTx); err != nil {
			f.mu.Unlock()
			return fmt.Errorf("send funding tx for key %d: %w", i, err)
		}

		txHashes[i] = signedTx.Hash().Hex()
		f.nonce++
	}
	f.mu.Unlock()

	// Wait for all to be mined (poll receipts)
	for i, hash := range txHashes {
		wg.Add(1)
		go func(idx int, txHash string) {
			defer wg.Done()
			if err := waitForReceipt(ctx, f.client, txHash, 30*time.Second); err != nil {
				errCount.Add(1)
				fmt.Printf("funding tx %d failed: %v\n", idx, err)
			}
		}(i, hash)
	}

	wg.Wait()

	if errCount.Load() > 0 {
		return fmt.Errorf("%d funding transactions failed", errCount.Load())
	}

	return nil
}

// waitForReceipt polls for a transaction receipt until timeout.
func waitForReceipt(ctx context.Context, client *ethclient.Client, txHash string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	hash := common.HexToHash(txHash)

	for time.Now().Before(deadline) {
		receipt, err := client.TransactionReceipt(ctx, hash)
		if err == nil && receipt != nil {
			if receipt.Status == 1 {
				return nil
			}
			return fmt.Errorf("tx reverted")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	return fmt.Errorf("timeout waiting for tx %s", txHash)
}
