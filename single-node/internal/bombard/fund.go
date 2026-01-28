package bombard

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"sync"

	ethereum "github.com/ava-labs/libevm"
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
	// Fund amount: 100 ETH per account
	fundAmount := ToWei(100)
	// Minimum balance to consider "funded" - accounts spend gas so balance decreases
	minBalance := ToWei(1)

	// Find keys that need funding
	var unfundedKeys []*Key
	var unfundedIndices []int
	for i, key := range keys {
		balance, err := client.BalanceAt(context.Background(), key.Address, nil)
		if err != nil {
			return fmt.Errorf("failed to check balance for key %d: %w", i, err)
		}
		if balance.Cmp(minBalance) < 0 {
			unfundedKeys = append(unfundedKeys, key)
			unfundedIndices = append(unfundedIndices, i)
		}
	}

	if len(unfundedKeys) == 0 {
		fmt.Println("all accounts already funded, skipping funding")
		return nil
	}

	fmt.Printf("funding %d of %d accounts (others already funded)\n", len(unfundedKeys), len(keys))

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

	gasLimit := uint64(21000)
	gasPriceVal := big.NewInt(gasPrice)

	var wg sync.WaitGroup
	errChan := make(chan error, len(unfundedKeys))

	for i, key := range unfundedKeys {
		wg.Add(1)
		go func(idx int, originalIdx int, k *Key, n uint64) {
			defer wg.Done()

			tx := types.NewTransaction(n, k.Address, fundAmount, gasLimit, gasPriceVal, nil)
			signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), ewoqKey)
			if err != nil {
				errChan <- fmt.Errorf("failed to sign funding tx %d: %w", originalIdx, err)
				return
			}

			err = client.SendTransaction(context.Background(), signedTx)
			if err != nil {
				errChan <- fmt.Errorf("failed to send funding tx %d: %w", originalIdx, err)
				return
			}

			if err := listener.AwaitTxMined(signedTx.Hash().String(), timeoutSeconds*3); err != nil {
				errChan <- fmt.Errorf("funding tx %d not mined: %w", originalIdx, err)
				return
			}
		}(i, unfundedIndices[i], key, nonce+uint64(i))
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

	// Verify all accounts actually received funds (use minBalance threshold)
	if err := verifyBalances(client, keys, minBalance); err != nil {
		return err
	}

	return nil
}

// verifyBalances checks that all keys have at least the expected balance.
func verifyBalances(client *ethclient.Client, keys []*Key, expectedMin *big.Int) error {
	var unfundedKeys []int
	for i, key := range keys {
		balance, err := client.BalanceAt(context.Background(), key.Address, nil)
		if err != nil {
			return fmt.Errorf("failed to check balance for key %d: %w", i, err)
		}
		if balance.Cmp(expectedMin) < 0 {
			unfundedKeys = append(unfundedKeys, i)
			log.Printf("Key %d (%s) has insufficient balance: %s (expected %s)",
				i, key.Address.Hex(), balance.String(), expectedMin.String())
		}
	}
	if len(unfundedKeys) > 0 {
		return fmt.Errorf("%d keys have insufficient balance: %v", len(unfundedKeys), unfundedKeys)
	}
	fmt.Printf("verified all %d accounts have sufficient balance\n", len(keys))
	return nil
}

// fundAccountsWithERC20 transfers ERC20 tokens from ewoq to all generated accounts.
func fundAccountsWithERC20(client *ethclient.Client, listener *TxListener, keys []*Key) error {
	// Fund amount: 100M tokens per account (with 18 decimals)
	fundAmount := ToWei(100_000_000)
	// Minimum balance to consider "funded" - accounts spend tokens so balance decreases
	minBalance := ToWei(1_000_000)

	// ERC20 balanceOf(address) selector
	balanceOfSelector := []byte{0x70, 0xa0, 0x82, 0x31}

	// Find keys that need ERC20 funding
	var unfundedKeys []*Key
	var unfundedIndices []int
	for i, key := range keys {
		callData := make([]byte, 36)
		copy(callData[0:4], balanceOfSelector)
		copy(callData[16:36], key.Address.Bytes())

		result, err := client.CallContract(context.Background(), ethereum.CallMsg{
			To:   &PredeployedTokenAddr,
			Data: callData,
		}, nil)
		if err != nil {
			return fmt.Errorf("failed to check ERC20 balance for key %d: %w", i, err)
		}

		balance := new(big.Int).SetBytes(result)
		if balance.Cmp(minBalance) < 0 {
			unfundedKeys = append(unfundedKeys, key)
			unfundedIndices = append(unfundedIndices, i)
		}
	}

	if len(unfundedKeys) == 0 {
		fmt.Println("all accounts already have ERC20 tokens, skipping funding")
		return nil
	}

	fmt.Printf("funding %d of %d accounts with ERC20 (others already funded)\n", len(unfundedKeys), len(keys))

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

	gasLimit := uint64(100000) // ERC20 transfers need more gas
	gasPriceVal := big.NewInt(gasPrice)

	var wg sync.WaitGroup
	errChan := make(chan error, len(unfundedKeys))

	for i, key := range unfundedKeys {
		wg.Add(1)
		go func(idx int, originalIdx int, k *Key, n uint64) {
			defer wg.Done()

			// Encode ERC20 transfer calldata
			data := EncodeERC20Transfer(k.Address, fundAmount)

			tx := types.NewTransaction(n, PredeployedTokenAddr, big.NewInt(0), gasLimit, gasPriceVal, data)
			signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), ewoqKey)
			if err != nil {
				errChan <- fmt.Errorf("failed to sign ERC20 funding tx %d: %w", originalIdx, err)
				return
			}

			err = client.SendTransaction(context.Background(), signedTx)
			if err != nil {
				errChan <- fmt.Errorf("failed to send ERC20 funding tx %d: %w", originalIdx, err)
				return
			}

			if err := listener.AwaitTxMined(signedTx.Hash().String(), timeoutSeconds*3); err != nil {
				errChan <- fmt.Errorf("ERC20 funding tx %d not mined: %w", originalIdx, err)
				return
			}
		}(i, unfundedIndices[i], key, nonce+uint64(i))
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

	// Verify all accounts actually received ERC20 tokens (use minBalance threshold)
	if err := verifyERC20Balances(client, keys, minBalance); err != nil {
		return err
	}

	return nil
}

// verifyERC20Balances checks that all keys have at least the expected ERC20 token balance.
func verifyERC20Balances(client *ethclient.Client, keys []*Key, expectedMin *big.Int) error {
	// ERC20 balanceOf(address) selector: keccak256("balanceOf(address)")[:4] = 0x70a08231
	balanceOfSelector := []byte{0x70, 0xa0, 0x82, 0x31}

	var unfundedKeys []int
	for i, key := range keys {
		// Encode balanceOf call
		callData := make([]byte, 36)
		copy(callData[0:4], balanceOfSelector)
		copy(callData[16:36], key.Address.Bytes())

		result, err := client.CallContract(context.Background(), ethereum.CallMsg{
			To:   &PredeployedTokenAddr,
			Data: callData,
		}, nil)
		if err != nil {
			return fmt.Errorf("failed to check ERC20 balance for key %d: %w", i, err)
		}

		balance := new(big.Int).SetBytes(result)
		if balance.Cmp(expectedMin) < 0 {
			unfundedKeys = append(unfundedKeys, i)
			log.Printf("Key %d (%s) has insufficient ERC20 balance: %s (expected %s)",
				i, key.Address.Hex(), balance.String(), expectedMin.String())
		}
	}
	if len(unfundedKeys) > 0 {
		return fmt.Errorf("%d keys have insufficient ERC20 balance: %v", len(unfundedKeys), unfundedKeys)
	}
	fmt.Printf("verified all %d accounts have sufficient ERC20 balance\n", len(keys))
	return nil
}
