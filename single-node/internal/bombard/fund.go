package bombard

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"sync"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethclient"
)

// EWOQ is the pre-funded test key used by Avalanche local networks.
const ewoqPrivateKey = "56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027"

// EwoqAddress is the address derived from ewoqPrivateKey (0x8db97C7cEcE249c2b98bDC0226Cc4C2A57BF52FC)
const EwoqAddress = "0x8db97C7cEcE249c2b98bDC0226Cc4C2A57BF52FC"

// PredeployedTokenAddress is the address of the predeployed ERC20 "Benchmark Token" (BENCH)
// in the default subnet-evm genesis. The ewoq address holds the entire 100M token supply.
const PredeployedTokenAddress = "0xB0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5"

func fundAccounts(client *ethclient.Client, listener *TxListener, keys []*Key) error {
	ewoqKey, err := crypto.HexToECDSA(ewoqPrivateKey)
	if err != nil {
		return fmt.Errorf("failed to parse EWOQ key: %w", err)
	}

	ewoqAddress := crypto.PubkeyToAddress(ewoqKey.PublicKey)

	chainID, err := client.NetworkID(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get chain ID: %w", err)
	}

	nonce, err := client.PendingNonceAt(context.Background(), ewoqAddress)
	if err != nil {
		return fmt.Errorf("failed to get nonce: %w", err)
	}

	// Fund amount: 100 ETH per account
	fundAmount := ToWei(100)
	gasLimit := uint64(21000)
	gasPriceVal := big.NewInt(gasPrice)

	var wg sync.WaitGroup
	errChan := make(chan error, len(keys))

	for i, key := range keys {
		wg.Add(1)
		go func(idx int, k *Key, n uint64) {
			defer wg.Done()

			tx := types.NewTransaction(n, k.Address, fundAmount, gasLimit, gasPriceVal, nil)
			signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), ewoqKey)
			if err != nil {
				errChan <- fmt.Errorf("failed to sign funding tx %d: %w", idx, err)
				return
			}

			err = client.SendTransaction(context.Background(), signedTx)
			if err != nil {
				errChan <- fmt.Errorf("failed to send funding tx %d: %w", idx, err)
				return
			}

			if err := listener.AwaitTxMined(signedTx.Hash().String(), timeoutSeconds*3); err != nil {
				errChan <- fmt.Errorf("funding tx %d not mined: %w", idx, err)
				return
			}
		}(i, key, nonce+uint64(i))
	}

	wg.Wait()
	close(errChan)

	var fundingErrors []error
	for err := range errChan {
		if err != nil {
			fundingErrors = append(fundingErrors, err)
			log.Printf("Funding error: %v", err)
		}
	}

	if len(fundingErrors) > 0 {
		return fmt.Errorf("encountered %d funding errors", len(fundingErrors))
	}

	return nil
}

// fundAccountsWithERC20 transfers ERC20 tokens from ewoq to all generated accounts.
func fundAccountsWithERC20(client *ethclient.Client, listener *TxListener, keys []*Key) error {
	ewoqKey, err := crypto.HexToECDSA(ewoqPrivateKey)
	if err != nil {
		return fmt.Errorf("failed to parse EWOQ key: %w", err)
	}

	ewoqAddress := crypto.PubkeyToAddress(ewoqKey.PublicKey)

	chainID, err := client.NetworkID(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get chain ID: %w", err)
	}

	nonce, err := client.PendingNonceAt(context.Background(), ewoqAddress)
	if err != nil {
		return fmt.Errorf("failed to get nonce: %w", err)
	}

	// Fund amount: 100M tokens per account (with 18 decimals)
	fundAmount := ToWei(100_000_000)
	gasLimit := uint64(100000) // ERC20 transfers need more gas
	gasPriceVal := big.NewInt(gasPrice)

	var wg sync.WaitGroup
	errChan := make(chan error, len(keys))

	for i, key := range keys {
		wg.Add(1)
		go func(idx int, k *Key, n uint64) {
			defer wg.Done()

			// Encode ERC20 transfer calldata
			data := EncodeERC20Transfer(k.Address, fundAmount)

			tx := types.NewTransaction(n, PredeployedTokenAddr, big.NewInt(0), gasLimit, gasPriceVal, data)
			signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), ewoqKey)
			if err != nil {
				errChan <- fmt.Errorf("failed to sign ERC20 funding tx %d: %w", idx, err)
				return
			}

			err = client.SendTransaction(context.Background(), signedTx)
			if err != nil {
				errChan <- fmt.Errorf("failed to send ERC20 funding tx %d: %w", idx, err)
				return
			}

			if err := listener.AwaitTxMined(signedTx.Hash().String(), timeoutSeconds*3); err != nil {
				errChan <- fmt.Errorf("ERC20 funding tx %d not mined: %w", idx, err)
				return
			}
		}(i, key, nonce+uint64(i))
	}

	wg.Wait()
	close(errChan)

	var fundingErrors []error
	for err := range errChan {
		if err != nil {
			fundingErrors = append(fundingErrors, err)
			log.Printf("ERC20 funding error: %v", err)
		}
	}

	if len(fundingErrors) > 0 {
		return fmt.Errorf("encountered %d ERC20 funding errors", len(fundingErrors))
	}

	return nil
}
