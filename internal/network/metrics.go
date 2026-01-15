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
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastBlock uint64
	var lastBlockTime time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			block, txCount, gasUsed, gasLimit, err := getLatestBlockInfo(ctx, rpcURL)
			if err != nil {
				fmt.Printf("metrics error: %v\n", err)
				continue
			}

			var tps float64
			if block != lastBlock {
				if !lastBlockTime.IsZero() {
					elapsed := time.Since(lastBlockTime).Seconds()
					if elapsed > 0 {
						tps = float64(txCount) / elapsed
					}
				}
				lastBlock = block
				lastBlockTime = time.Now()
			}

			gasPercent := float64(0)
			if gasLimit > 0 {
				gasPercent = float64(gasUsed) / float64(gasLimit) * 100
			}

			fmt.Printf("Block %d | TPS: %.0f | Gas: %d/%d (%.1f%%)\n",
				block, tps, gasUsed/1e6, gasLimit/1e6, gasPercent)
		}
	}
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
