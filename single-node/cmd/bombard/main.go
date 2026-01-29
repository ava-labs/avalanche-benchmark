package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/rpc"
)

const (
	// EWOQ is the pre-funded test key for Avalanche local networks
	ewoqPrivateKey = "56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027"

	// Transaction parameters
	defaultTps = 4000

	tickerTime  = 90 * time.Second // Interval between sends (mempool expires in 60s, so 90s ensures clean slate)
	workerDelay = 50 * time.Millisecond
	numWorkers  = int(tickerTime / workerDelay)

	gasLimitNative = 21000
	gasLimitERC20  = 65000
	gasPrice       = 25
)

var erc20Contract = common.HexToAddress("0xB0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5")

var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	},
	Timeout: 30 * time.Second,
}

func main() {
	rpcURL := flag.String("rpc", "", "RPC URL")
	targetTps := flag.Int("tps", defaultTps, "Target transactions per second")
	erc20Mode := flag.Bool("erc20", false, "Send ERC20 transfers instead of native transfers")
	flag.Parse()

	if *rpcURL == "" {
		fmt.Println("Usage: bombard --rpc=<url> [--tps=4000] [--erc20]")
		os.Exit(1)
	}

	// Calculate batch size based on target TPS
	batchSize := *targetTps * int(workerDelay/time.Millisecond) / 1000

	// Connect
	rpcClient, err := rpc.DialHTTPWithClient(*rpcURL, httpClient)
	if err != nil {
		fmt.Printf("Failed to connect: %v\n", err)
		os.Exit(1)
	}
	client := ethclient.NewClient(rpcClient)
	fmt.Printf("Connected to %s\n", *rpcURL)

	ctx := context.Background()

	// Get chain ID
	chainID, err := client.NetworkID(ctx)
	if err != nil {
		fmt.Printf("Failed to get chain ID: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Chain ID: %s\n", chainID)

	// Load key
	privateKey, err := crypto.HexToECDSA(ewoqPrivateKey)
	if err != nil {
		fmt.Printf("Failed to load key: %v\n", err)
		os.Exit(1)
	}
	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	fmt.Printf("Address: %s\n", address.Hex())

	signer := types.NewEIP155Signer(chainID)

	// Derive worker keys
	workerKeys, workerAddrs, err := DeriveWorkerKeys(privateKey, numWorkers)
	if err != nil {
		fmt.Printf("Failed to derive worker keys: %v\n", err)
		os.Exit(1)
	}

	// Fund workers that need it
	fmt.Println("\nChecking worker balances...")
	err = FundWorkers(ctx, client, privateKey, signer, workerAddrs)
	if err != nil {
		fmt.Printf("Failed to fund workers: %v\n", err)
		os.Exit(1)
	}

	// Fund workers with ERC20 tokens if in ERC20 mode
	if *erc20Mode {
		fmt.Println("Checking worker ERC20 balances...")
		err = FundWorkersERC20(ctx, client, privateKey, signer, workerAddrs, erc20Contract)
		if err != nil {
			fmt.Printf("Failed to fund workers with ERC20: %v\n", err)
			os.Exit(1)
		}
	}

	// Wait for funding txs to be mined
	fmt.Println("Waiting for funding transactions...")
	time.Sleep(3 * time.Second)

	// Start block watcher
	go watchBlocks(ctx, rpcClient)

	mode := "native"
	if *erc20Mode {
		mode = "ERC20"
	}
	fmt.Printf("\nStarting %d workers (%s): send %d txs every %v, staggered by %v\n\n", numWorkers, mode, batchSize, tickerTime, workerDelay)

	// Start workers with staggered delays
	for i := 0; i < numWorkers; i++ {
		workerID := i + 1
		go runWorker(ctx, client, workerKeys[i], signer, workerAddrs[i], workerID, batchSize, *erc20Mode)
		if i < numWorkers-1 {
			time.Sleep(workerDelay)
		}
	}

	// Wait for context cancellation
	<-ctx.Done()
}

func runWorker(
	ctx context.Context,
	client *ethclient.Client,
	privateKey *ecdsa.PrivateKey,
	signer types.Signer,
	address common.Address,
	workerID int,
	batchSize int,
	erc20 bool,
) {
	ticker := time.NewTicker(tickerTime)
	defer ticker.Stop()

	round := 0

	// Run immediately on start
	runWorkerRound(ctx, client, privateKey, signer, address, workerID, &round, batchSize, erc20)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runWorkerRound(ctx, client, privateKey, signer, address, workerID, &round, batchSize, erc20)
		}
	}
}

func runWorkerRound(
	ctx context.Context,
	client *ethclient.Client,
	privateKey *ecdsa.PrivateKey,
	signer types.Signer,
	address common.Address,
	workerID int,
	round *int,
	batchSize int,
	erc20 bool,
) {
	*round++

	// Fetch nonce
	nonce, err := client.PendingNonceAt(ctx, address)
	if err != nil {
		fmt.Printf("[Worker %d] Failed to get nonce: %v\n", workerID, err)
		return
	}

	// Send batch (to self)
	_, errors := sendBatch(ctx, client, privateKey, signer, address, address, nonce, batchSize, erc20)
	if errors > 0 {
		fmt.Printf("[Worker %d] Errors: %d\n", workerID, errors)
	}
}

// encodeERC20Transfer returns calldata for transfer(address,uint256)
func encodeERC20Transfer(to common.Address, amount *big.Int) []byte {
	data := make([]byte, 68)
	copy(data[0:4], []byte{0xa9, 0x05, 0x9c, 0xbb}) // transfer(address,uint256) selector
	copy(data[16:36], to.Bytes())                   // address padded to 32 bytes
	amount.FillBytes(data[36:68])                   // uint256
	return data
}

func sendBatch(
	ctx context.Context,
	client *ethclient.Client,
	privateKey *ecdsa.PrivateKey,
	signer types.Signer,
	from, to common.Address,
	startNonce uint64,
	count int,
	erc20 bool,
) (sent, errors int) {
	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var tx *types.Transaction
		if erc20 {
			data := encodeERC20Transfer(to, big.NewInt(1))
			tx = types.NewTransaction(
				startNonce+uint64(i),
				erc20Contract,
				big.NewInt(0),
				gasLimitERC20,
				big.NewInt(gasPrice),
				data,
			)
		} else {
			tx = types.NewTransaction(
				startNonce+uint64(i),
				to,
				big.NewInt(1), // 1 wei
				gasLimitNative,
				big.NewInt(gasPrice),
				nil,
			)
		}

		signed, err := types.SignTx(tx, signer, privateKey)
		if err != nil {
			errors++
			continue
		}

		err = client.SendTransaction(ctx, signed)
		if err != nil {
			errors++
			continue
		}

		sent++
	}
	return
}
