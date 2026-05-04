package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// waitHealthy polls http://host:port/ext/health and returns nil when the
// endpoint returns 200, or the last error after timeout.
func waitHealthy(ctx context.Context, host string, port int, timeout time.Duration, label string) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://%s:%d/ext/health", host, port)
	client := &http.Client{Timeout: 3 * time.Second}

	var lastErr error
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("[%s] not healthy after %s: %v", label, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}
}
