package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"

	ethereum "github.com/ava-labs/libevm"
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

// ERC20 funding constants
const (
	erc20FundAmount    = 1000000 // Amount of tokens to fund each worker (in token units)
	erc20FundThreshold = 500000  // Fund if balance is below this
	erc20FundGasLimit  = 65000   // Gas limit for ERC20 transfer
)

// encodeBalanceOf returns calldata for balanceOf(address)
func encodeBalanceOf(addr common.Address) []byte {
	data := make([]byte, 36)
	copy(data[0:4], []byte{0x70, 0xa0, 0x82, 0x31}) // balanceOf(address) selector
	copy(data[16:36], addr.Bytes())                 // address padded to 32 bytes
	return data
}

// encodeTransfer returns calldata for transfer(address,uint256)
func encodeTransfer(to common.Address, amount *big.Int) []byte {
	data := make([]byte, 68)
	copy(data[0:4], []byte{0xa9, 0x05, 0x9c, 0xbb}) // transfer(address,uint256) selector
	copy(data[16:36], to.Bytes())                   // address padded to 32 bytes
	amount.FillBytes(data[36:68])                   // uint256
	return data
}

// FundWorkersERC20 ensures each worker has ERC20 tokens for benchmarking.
func FundWorkersERC20(
	ctx context.Context,
	client *ethclient.Client,
	funderKey *ecdsa.PrivateKey,
	signer types.Signer,
	workerAddrs []common.Address,
	tokenContract common.Address,
) error {
	funderAddr := crypto.PubkeyToAddress(funderKey.PublicKey)

	// Check which workers need ERC20 funding
	toFund := make([]int, 0)
	threshold := new(big.Int).Mul(big.NewInt(erc20FundThreshold), big.NewInt(1e18))

	for i, addr := range workerAddrs {
		// Call balanceOf on the token contract
		callData := encodeBalanceOf(addr)
		result, err := client.CallContract(ctx, ethereum.CallMsg{
			To:   &tokenContract,
			Data: callData,
		}, nil)
		if err != nil {
			return fmt.Errorf("failed to get ERC20 balance for worker %d: %w", i+1, err)
		}

		balance := new(big.Int).SetBytes(result)
		if balance.Cmp(threshold) < 0 {
			toFund = append(toFund, i)
			fmt.Printf("Worker %d needs ERC20 funding (balance %.2f)\n", i+1, weiToEther(balance))
		}
	}

	if len(toFund) == 0 {
		fmt.Println("All workers have sufficient ERC20 balance")
		return nil
	}

	// Get funder nonce
	nonce, err := client.PendingNonceAt(ctx, funderAddr)
	if err != nil {
		return fmt.Errorf("failed to get funder nonce: %w", err)
	}

	// Fund workers that need it
	amount := new(big.Int).Mul(big.NewInt(erc20FundAmount), big.NewInt(1e18))
	gasPrice := big.NewInt(fundGasPrice)

	for i, workerIdx := range toFund {
		callData := encodeTransfer(workerAddrs[workerIdx], amount)
		tx := types.NewTransaction(
			nonce+uint64(i),
			tokenContract,
			big.NewInt(0),
			erc20FundGasLimit,
			gasPrice,
			callData,
		)
		signed, err := types.SignTx(tx, signer, funderKey)
		if err != nil {
			return fmt.Errorf("failed to sign ERC20 funding tx for worker %d: %w", workerIdx+1, err)
		}
		err = client.SendTransaction(ctx, signed)
		if err != nil {
			return fmt.Errorf("failed to send ERC20 funding tx for worker %d: %w", workerIdx+1, err)
		}
		fmt.Printf("Funded worker %d with %d tokens\n", workerIdx+1, erc20FundAmount)
	}

	fmt.Printf("Sent %d ERC20 funding transactions\n", len(toFund))
	return nil
}
