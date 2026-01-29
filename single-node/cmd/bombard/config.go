package main

import (
	"math/big"
	"net/http"
	"time"

	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/rpc"
)

// Config holds the bombardment configuration.
type Config struct {
	RPCURLs []string
	Workers int
}

// Constants for tuning
const (
	KeyPoolSize         = 10000           // Pre-generated keys
	ActiveWorkers       = 1000            // How many workers sending concurrently
	ConfirmationTimeout = 5 * time.Second // Max wait for nonce to advance
	FundAmountETH       = 100             // ETH to fund each active key
	NativeGasLimit      = 21000           // Gas for native transfer
	GasPrice            = 25              // Wei (genesis minBaseFee is 1)
)

// Shared HTTP transport for connection pooling
var sharedTransport = &http.Transport{
	MaxIdleConns:        10000,
	MaxIdleConnsPerHost: 10000,
	MaxConnsPerHost:     10000,
	IdleConnTimeout:     90 * time.Second,
}

var sharedHTTPClient = &http.Client{
	Transport: sharedTransport,
	Timeout:   30 * time.Second,
}

// dialRPC creates an ethclient using the shared HTTP transport.
func dialRPC(url string) (*ethclient.Client, error) {
	rpcClient, err := rpc.DialHTTPWithClient(url, sharedHTTPClient)
	if err != nil {
		return nil, err
	}
	return ethclient.NewClient(rpcClient), nil
}

// toWei converts ETH to wei.
func toWei(eth int64) *big.Int {
	wei := big.NewInt(eth)
	exp := big.NewInt(1e18)
	return wei.Mul(wei, exp)
}
