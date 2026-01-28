package bombard

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"log"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethclient"
)

var errorCount = 0
var lastError string
var errorReporterStarted = false
var errorReporterMu sync.Mutex

func startErrorReporter() {
	errorReporterMu.Lock()
	if errorReporterStarted {
		errorReporterMu.Unlock()
		return
	}
	errorReporterStarted = true
	errorReporterMu.Unlock()

	go func() {
		for {
			time.Sleep(30 * time.Second)
			if errorCount > 0 {
				fmt.Printf("Errors: %d, Last: %s\n", errorCount, lastError)
				errorCount = 0
				lastError = ""
			}
			if hadTransactionUnderpricedErrors {
				fmt.Println("Had transaction underpriced errors!")
				hadTransactionUnderpricedErrors = false
			}
		}
	}()
}

// Gas price set to 25 wei (genesis minBaseFee is 1 wei, so this gives 25x margin)
var gasPrice = int64(25)

var hadTransactionUnderpricedErrors = false

func bombardWithTransactions(ctx context.Context, client *ethclient.Client, key *ecdsa.PrivateKey, listener *TxListener, data []byte, to common.Address) {
	startErrorReporter()

	fromAddress := crypto.PubkeyToAddress(key.PublicKey)

	gasLimit := uint64(21000)

	if len(data) > 0 {
		gasLimit = 1000000
	}

	chainID, err := client.NetworkID(context.Background())
	if err != nil {
		log.Printf("failed to get chain ID: %v", err)
		return
	}

	shouldRefetchNonce := true

	nonce := uint64(0)

	value := big.NewInt(123)
	if len(data) > 0 {
		value = big.NewInt(0)
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Re-fetch nonce if previous transactions had errors
		if shouldRefetchNonce {
			newNonce, err := client.PendingNonceAt(context.Background(), fromAddress)
			if err != nil {
				log.Printf("failed to refresh nonce: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}
			nonce = newNonce
			shouldRefetchNonce = false
		}

		signedTxs := make([]*types.Transaction, 0, batchSize)
		for i := 0; i < batchSize; i++ {
			tx := types.NewTransaction(nonce, to, value, gasLimit, big.NewInt(gasPrice), data)

			signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), key)
			if err != nil {
				log.Fatalf("failed to sign transaction: %v", err)
			}

			signedTxs = append(signedTxs, signedTx)
			nonce++
		}

		// Send transactions sequentially to avoid overwhelming the connection pool
		// With 600 keys, we still get good parallelism across keys
		txHashes := make([]string, 0, len(signedTxs))
		hasError := false

		for _, signedTx := range signedTxs {
			err := client.SendTransaction(context.Background(), signedTx)
			if err != nil {
				// "already known" means tx is in mempool - not a real error
				if isAlreadyKnown(err) {
					txHashes = append(txHashes, signedTx.Hash().String())
					continue
				}
				lastError = err.Error()
				errorCount++
				hasError = true
				if isTransactionUnderpriced(err) {
					hadTransactionUnderpricedErrors = true
				}
				// Continue trying to send remaining transactions
				continue
			}
			txHashes = append(txHashes, signedTx.Hash().String())
		}

		// If we had errors, mark that we should refetch the nonce
		if hasError {
			shouldRefetchNonce = true
			time.Sleep(1 * time.Second)
			continue
		}

		// Wait for the last transaction to be mined (confirms the whole batch landed)
		if len(txHashes) > 0 {
			if listener.AwaitTxMined(txHashes[len(txHashes)-1], timeoutSeconds) != nil {
				shouldRefetchNonce = true
			}
		}
	}
}

func isTransactionUnderpriced(err error) bool {
	if strings.HasSuffix(err.Error(), ": transaction underpriced") {
		return true
	}

	if strings.Contains(err.Error(), "< pool minimum fee cap") {
		return true
	}

	return false
}

// isAlreadyKnown returns true if the error indicates the tx is already in the mempool.
// This is not a real error - the tx was accepted, just submitted twice.
func isAlreadyKnown(err error) bool {
	return strings.Contains(err.Error(), "already known")
}

// bombardWithERC20Transactions sends ERC20 token transfer transactions.
func bombardWithERC20Transactions(ctx context.Context, client *ethclient.Client, key *ecdsa.PrivateKey, listener *TxListener, to common.Address) {
	startErrorReporter()

	fromAddress := crypto.PubkeyToAddress(key.PublicKey)

	// ERC20 transfers need more gas than native transfers
	gasLimit := uint64(65000)

	chainID, err := client.NetworkID(context.Background())
	if err != nil {
		log.Printf("failed to get chain ID: %v", err)
		return
	}

	shouldRefetchNonce := true
	nonce := uint64(0)

	// Transfer 1 token (with 18 decimals) per transaction
	transferAmount := big.NewInt(1e18)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Re-fetch nonce if previous transactions had errors
		if shouldRefetchNonce {
			newNonce, err := client.PendingNonceAt(context.Background(), fromAddress)
			if err != nil {
				log.Printf("failed to refresh nonce: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}
			nonce = newNonce
			shouldRefetchNonce = false
		}

		signedTxs := make([]*types.Transaction, 0, batchSize)
		for i := 0; i < batchSize; i++ {
			// Encode ERC20 transfer calldata
			data := EncodeERC20Transfer(to, transferAmount)

			tx := types.NewTransaction(nonce, PredeployedTokenAddr, big.NewInt(0), gasLimit, big.NewInt(gasPrice), data)

			signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), key)
			if err != nil {
				log.Fatalf("failed to sign transaction: %v", err)
			}

			signedTxs = append(signedTxs, signedTx)
			nonce++
		}

		// Send transactions sequentially to avoid overwhelming the connection pool
		txHashes := make([]string, 0, len(signedTxs))
		hasError := false

		for _, signedTx := range signedTxs {
			err := client.SendTransaction(context.Background(), signedTx)
			if err != nil {
				// "already known" means tx is in mempool - not a real error
				if isAlreadyKnown(err) {
					txHashes = append(txHashes, signedTx.Hash().String())
					continue
				}
				lastError = err.Error()
				errorCount++
				hasError = true
				if isTransactionUnderpriced(err) {
					hadTransactionUnderpricedErrors = true
				}
				continue
			}
			txHashes = append(txHashes, signedTx.Hash().String())
		}

		// If we had errors, mark that we should refetch the nonce
		if hasError {
			shouldRefetchNonce = true
			time.Sleep(1 * time.Second)
			continue
		}

		// Wait for the last transaction to be mined (confirms the whole batch landed)
		if len(txHashes) > 0 {
			if listener.AwaitTxMined(txHashes[len(txHashes)-1], timeoutSeconds) != nil {
				shouldRefetchNonce = true
			}
		}
	}
}

// bombardWithBothTransactions alternates between native and ERC20 transfers.
func bombardWithBothTransactions(ctx context.Context, client *ethclient.Client, key *ecdsa.PrivateKey, listener *TxListener, data []byte, to common.Address) {
	startErrorReporter()

	fromAddress := crypto.PubkeyToAddress(key.PublicKey)

	chainID, err := client.NetworkID(context.Background())
	if err != nil {
		log.Printf("failed to get chain ID: %v", err)
		return
	}

	shouldRefetchNonce := true
	nonce := uint64(0)
	useERC20 := false // Alternate between native and ERC20

	nativeValue := big.NewInt(123)
	if len(data) > 0 {
		nativeValue = big.NewInt(0)
	}

	// Transfer 1 token (with 18 decimals) per ERC20 transaction
	erc20Amount := big.NewInt(1e18)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Re-fetch nonce if previous transactions had errors
		if shouldRefetchNonce {
			newNonce, err := client.PendingNonceAt(context.Background(), fromAddress)
			if err != nil {
				log.Printf("failed to refresh nonce: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}
			nonce = newNonce
			shouldRefetchNonce = false
		}

		signedTxs := make([]*types.Transaction, 0, batchSize)
		for i := 0; i < batchSize; i++ {
			var tx *types.Transaction

			if useERC20 {
				// ERC20 transfer
				txData := EncodeERC20Transfer(to, erc20Amount)
				tx = types.NewTransaction(nonce, PredeployedTokenAddr, big.NewInt(0), 65000, big.NewInt(gasPrice), txData)
			} else {
				// Native transfer
				gasLimit := uint64(21000)
				if len(data) > 0 {
					gasLimit = 1000000
				}
				tx = types.NewTransaction(nonce, to, nativeValue, gasLimit, big.NewInt(gasPrice), data)
			}

			signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), key)
			if err != nil {
				log.Fatalf("failed to sign transaction: %v", err)
			}

			signedTxs = append(signedTxs, signedTx)
			nonce++
		}

		// Toggle for next batch
		useERC20 = !useERC20

		// Send transactions sequentially
		txHashes := make([]string, 0, len(signedTxs))
		hasError := false

		for _, signedTx := range signedTxs {
			err := client.SendTransaction(context.Background(), signedTx)
			if err != nil {
				// "already known" means tx is in mempool - not a real error
				if isAlreadyKnown(err) {
					txHashes = append(txHashes, signedTx.Hash().String())
					continue
				}
				lastError = err.Error()
				errorCount++
				hasError = true
				if isTransactionUnderpriced(err) {
					hadTransactionUnderpricedErrors = true
				}
				continue
			}
			txHashes = append(txHashes, signedTx.Hash().String())
		}

		if hasError {
			shouldRefetchNonce = true
			time.Sleep(1 * time.Second)
			continue
		}

		// Wait for the last transaction to be mined (confirms the whole batch landed)
		if len(txHashes) > 0 {
			if listener.AwaitTxMined(txHashes[len(txHashes)-1], timeoutSeconds) != nil {
				shouldRefetchNonce = true
			}
		}
	}
}
