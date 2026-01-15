package network

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func printMetrics(ctx context.Context, rpcURL string) {
	const interval = 10 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastBlock uint64
	startTime := time.Now()

	// Get initial block info
	block, _, _, gasLimit, err := getLatestBlockInfo(ctx, rpcURL)
	if err == nil {
		lastBlock = block
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			block, _, _, currentGasLimit, err := getLatestBlockInfo(ctx, rpcURL)
			if err != nil {
				fmt.Printf("metrics error: %v\n", err)
				continue
			}
			if currentGasLimit > 0 {
				gasLimit = currentGasLimit
			}

			elapsed := time.Since(startTime).Seconds()
			
			// Calculate blocks per second over the interval
			blocksDiff := block - lastBlock
			bps := float64(blocksDiff) / interval.Seconds()
			
			// Get totals from all blocks in the interval
			totalTxs, totalGasUsed := getIntervalStats(ctx, rpcURL, lastBlock+1, block)
			tps := float64(totalTxs) / interval.Seconds()
			gasPerSec := float64(totalGasUsed) / interval.Seconds()

			// Theoretical max gas/sec = BPS * gasLimit
			maxGasPerSec := bps * float64(gasLimit)
			gasPercent := float64(0)
			if maxGasPerSec > 0 {
				gasPercent = gasPerSec / maxGasPerSec * 100
			}

			fmt.Printf("[%.0fs] Block %d | BPS: %.2f | TPS: %.0f | Gas/s: %.0fM (%.1f%%)\n",
				elapsed, block, bps, tps, gasPerSec/1e6, gasPercent)

			lastBlock = block
		}
	}
}

// getIntervalStats sums transaction counts and gas used from startBlock to endBlock (inclusive)
func getIntervalStats(ctx context.Context, rpcURL string, startBlock, endBlock uint64) (uint64, uint64) {
	var totalTxs, totalGas uint64
	for blockNum := startBlock; blockNum <= endBlock; blockNum++ {
		_, txCount, gasUsed, _, err := getBlockInfo(ctx, rpcURL, blockNum)
		if err != nil {
			continue
		}
		totalTxs += uint64(txCount)
		totalGas += gasUsed
	}
	return totalTxs, totalGas
}

func getBlockInfo(ctx context.Context, rpcURL string, blockNum uint64) (uint64, int, uint64, uint64, error) {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_getBlockByNumber",
		"params":  []interface{}{fmt.Sprintf("0x%x", blockNum), false},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", rpcURL, strings.NewReader(string(body)))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Result *struct {
			Number       string   `json:"number"`
			Transactions []string `json:"transactions"`
			GasUsed      string   `json:"gasUsed"`
			GasLimit     string   `json:"gasLimit"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, 0, 0, 0, err
	}
	if result.Error != nil {
		return 0, 0, 0, 0, fmt.Errorf(result.Error.Message)
	}
	if result.Result == nil {
		return 0, 0, 0, 0, fmt.Errorf("no block result")
	}

	num, _ := parseHexUint64(result.Result.Number)
	gasUsed, _ := parseHexUint64(result.Result.GasUsed)
	gasLimit, _ := parseHexUint64(result.Result.GasLimit)

	return num, len(result.Result.Transactions), gasUsed, gasLimit, nil
}

func getLatestBlockInfo(ctx context.Context, rpcURL string) (uint64, int, uint64, uint64, error) {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_getBlockByNumber",
		"params":  []interface{}{"latest", false},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", rpcURL, strings.NewReader(string(body)))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Result *struct {
			Number       string   `json:"number"`
			Transactions []string `json:"transactions"`
			GasUsed      string   `json:"gasUsed"`
			GasLimit     string   `json:"gasLimit"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, 0, 0, 0, err
	}
	if result.Error != nil {
		return 0, 0, 0, 0, fmt.Errorf(result.Error.Message)
	}
	if result.Result == nil {
		return 0, 0, 0, 0, fmt.Errorf("no block result")
	}

	blockNum, err := parseHexUint64(result.Result.Number)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	gasUsed, err := parseHexUint64(result.Result.GasUsed)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	gasLimit, err := parseHexUint64(result.Result.GasLimit)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	return blockNum, len(result.Result.Transactions), gasUsed, gasLimit, nil
}

func parseHexUint64(value string) (uint64, error) {
	value = strings.TrimPrefix(value, "0x")
	if value == "" {
		return 0, nil
	}
	return strconv.ParseUint(value, 16, 64)
}
