package network

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

// waitForNodeHealth waits for a node to become healthy and returns its node ID
func waitForNodeHealth(ctx context.Context, uri string, pid int, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		if !isProcessRunning(pid) {
			return "", fmt.Errorf("process exited before becoming healthy")
		}

		nodeID, err := checkNodeHealth(uri)
		if err == nil && nodeID != "" {
			return nodeID, nil
		}

		time.Sleep(healthCheckInterval)
	}

	return "", fmt.Errorf("timeout waiting for node to become healthy")
}

func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// checkNodeHealth checks if a node is healthy and returns its node ID
func checkNodeHealth(uri string) (string, error) {
	client := &http.Client{Timeout: nodeHealthTimeout}

	// Check info endpoint for node ID
	req, err := http.NewRequest("POST", uri+"/ext/info", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}`))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Result struct {
			NodeID string `json:"nodeID"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if result.Result.NodeID == "" {
		return "", fmt.Errorf("no node ID returned")
	}

	return result.Result.NodeID, nil
}
