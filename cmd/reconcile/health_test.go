package main

import "testing"

func TestClassifyHealth(t *testing.T) {
	tests := []struct {
		name      string
		connErr   bool
		status    int
		body      string
		wantState nodeHealth
		wantBlock uint64
	}{
		{"connection refused -> down", true, 0, "", healthDown, 0},
		{"503 -> bootstrapping", false, 503, "", healthBootstrapping, 0},
		{"bootstrapping message body", false, 200, `{"error":"API call rejected because chain is not done bootstrapping"}`, healthBootstrapping, 0},
		{"serving block 0", false, 200, `{"jsonrpc":"2.0","id":1,"result":"0x0"}`, healthServing, 0},
		{"serving block 0x47", false, 200, `{"jsonrpc":"2.0","id":1,"result":"0x47"}`, healthServing, 71},
		{"serving large block", false, 200, `{"jsonrpc":"2.0","id":1,"result":"0x1b"}`, healthServing, 27},
		{"reachable but junk -> bootstrapping", false, 200, `not json`, healthBootstrapping, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, bn := classifyHealth(tt.connErr, tt.status, tt.body)
			if st != tt.wantState || bn != tt.wantBlock {
				t.Errorf("classifyHealth(%v,%d,%q) = (%v,%d), want (%v,%d)",
					tt.connErr, tt.status, tt.body, st, bn, tt.wantState, tt.wantBlock)
			}
		})
	}
}

func TestNeededOnlineToRejoin(t *testing.T) {
	// 3 equal validators: ceil(75%) = 3 (all must be online to clear the latch).
	if got := neededOnlineToRejoin(); got != 3 {
		t.Errorf("neededOnlineToRejoin() = %d, want 3", got)
	}
}
