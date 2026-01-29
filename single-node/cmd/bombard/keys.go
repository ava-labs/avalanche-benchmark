package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethclient"
)

const (
	fundAmount    = 100  // Amount of native coin to fund each worker
	fundThreshold = 50   // Fund if balance is below this
	fundGasPrice  = 25e9 // 25 gwei
)

// DeriveWorkerKeys derives deterministic keys for workers from a master key.
// Keys are derived by hashing: keccak256(masterKey || "bombard-worker" || index)
func DeriveWorkerKeys(masterKey *ecdsa.PrivateKey, count int) ([]*ecdsa.PrivateKey, []common.Address, error) {
	keys := make([]*ecdsa.PrivateKey, count)
	addrs := make([]common.Address, count)

	for i := 0; i < count; i++ {
		// Deterministic derivation: hash master key bytes with domain separator and index
		seed := crypto.Keccak256(
			masterKey.D.Bytes(),
			[]byte("bombard-worker"),
			big.NewInt(int64(i)).Bytes(),
		)
		key, err := crypto.ToECDSA(seed)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to derive key %d: %w", i, err)
		}
		keys[i] = key
		addrs[i] = crypto.PubkeyToAddress(key.PublicKey)
	}

	return keys, addrs, nil
}

// FundWorkers ensures each worker has at least fundAmount of native coin.
// Only funds workers with balance below fundThreshold.
func FundWorkers(
	ctx context.Context,
	client *ethclient.Client,
	funderKey *ecdsa.PrivateKey,
	signer types.Signer,
	workerAddrs []common.Address,
) error {
	funderAddr := crypto.PubkeyToAddress(funderKey.PublicKey)

	// Check which workers need funding
	toFund := make([]int, 0)
	for i, addr := range workerAddrs {
		balance, err := client.BalanceAt(ctx, addr, nil)
		if err != nil {
			return fmt.Errorf("failed to get balance for worker %d: %w", i+1, err)
		}

		threshold := new(big.Int).Mul(big.NewInt(fundThreshold), big.NewInt(1e18))
		if balance.Cmp(threshold) < 0 {
			toFund = append(toFund, i)
			fmt.Printf("Worker %d needs funding (balance %.2f)\n", i+1, weiToEther(balance))
		}
	}

	if len(toFund) == 0 {
		fmt.Println("All workers have sufficient balance")
		return nil
	}

	// Get funder nonce
	nonce, err := client.PendingNonceAt(ctx, funderAddr)
	if err != nil {
		return fmt.Errorf("failed to get funder nonce: %w", err)
	}

	// Fund workers that need it
	amount := new(big.Int).Mul(big.NewInt(fundAmount), big.NewInt(1e18))
	gasPrice := big.NewInt(fundGasPrice)

	for i, workerIdx := range toFund {
		tx := types.NewTransaction(
			nonce+uint64(i),
			workerAddrs[workerIdx],
			amount,
			21000,
			gasPrice,
			nil,
		)
		signed, err := types.SignTx(tx, signer, funderKey)
		if err != nil {
			return fmt.Errorf("failed to sign funding tx for worker %d: %w", workerIdx+1, err)
		}
		err = client.SendTransaction(ctx, signed)
		if err != nil {
			return fmt.Errorf("failed to send funding tx for worker %d: %w", workerIdx+1, err)
		}
		fmt.Printf("Funded worker %d with %d coins\n", workerIdx+1, fundAmount)
	}

	fmt.Printf("Sent %d funding transactions\n", len(toFund))
	return nil
}

func weiToEther(wei *big.Int) float64 {
	f := new(big.Float).SetInt(wei)
	e := new(big.Float).SetInt(big.NewInt(1e18))
	result, _ := new(big.Float).Quo(f, e).Float64()
	return result
}
