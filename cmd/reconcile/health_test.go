package main

import (
	"testing"
	"time"
)

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

func TestMarkCatchingUp(t *testing.T) {
	tests := []struct {
		name    string
		in      []healthResult
		wantMax uint64
		want    []nodeHealth
	}{
		{"all at tip stay serving",
			[]healthResult{{healthServing, 47100}, {healthServing, 47099}, {healthServing, 47100}},
			47100,
			[]nodeHealth{healthServing, healthServing, healthServing}},
		{"node 2013 behind is catching up",
			[]healthResult{{healthServing, 47100}, {healthServing, 45087}, {healthServing, 47095}},
			47100,
			[]nodeHealth{healthServing, healthCatchingUp, healthServing}},
		{"exactly threshold behind stays serving, one more is catching up",
			[]healthResult{{healthServing, 1000}, {healthServing, 1000 - catchUpThreshold}, {healthServing, 1000 - catchUpThreshold - 1}},
			1000,
			[]nodeHealth{healthServing, healthServing, healthCatchingUp}},
		{"single responding node is trivially serving",
			[]healthResult{{healthDown, 0}, {healthServing, 42}, {healthDown, 0}},
			42,
			[]nodeHealth{healthDown, healthServing, healthDown}},
		{"down and bootstrapping heights (0) do not drag the max down",
			[]healthResult{{healthBootstrapping, 0}, {healthServing, 50000}, {healthDown, 0}},
			50000,
			[]nodeHealth{healthBootstrapping, healthServing, healthDown}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			markCatchingUp(tt.in)
			if got := fleetMaxBlock(tt.in); got != tt.wantMax {
				t.Errorf("fleetMaxBlock = %d, want %d", got, tt.wantMax)
			}
			for i, r := range tt.in {
				if r.state != tt.want[i] {
					t.Errorf("node %d: state = %v, want %v", i, r.state, tt.want[i])
				}
			}
		})
	}
}

func TestNeededOnlineToRejoin(t *testing.T) {
	// 3 equal validators: ceil(75%) = 3 (all must be online to clear the latch).
	if got := neededOnlineToRejoin(3); got != 3 {
		t.Errorf("neededOnlineToRejoin(3) = %d, want 3", got)
	}
}

func TestParseConsensusHealth(t *testing.T) {
	const chainID = "24TbaJDZibLELkqPYUBGzCPfFyRp7ZdfTJYqCdGQnHEo4zFrvd"
	// Mirrors a real /ext/health/health body: the chain's message is an OBJECT,
	// while "bls" is a STRING and "bootstrapped" is an ARRAY - the mix that broke
	// the first cut (decoding the whole map into one object struct failed on those).
	healthy := `{"checks":{
		"` + chainID + `":{"message":{"engine":{"consensus":{"lastAcceptedHeight":515187,"longestProcessingBlock":"0s","processingBlocks":0}},"networking":{"percentConnected":1,"disconnectedValidators":[]}},"timestamp":"2026-06-24T21:17:55Z","duration":766376},
		"bls":{"message":"node is not a validator","timestamp":"2026-06-24T21:17:55Z"},
		"bootstrapped":{"message":[],"timestamp":"2026-06-24T21:17:55Z"},
		"diskspace":{"message":{"availableDiskBytes":191987679232,"availableDiskPercentage":92}}
	},"healthy":true}`
	pc, longest, last, ok := parseConsensusHealth([]byte(healthy), chainID)
	if !ok {
		t.Fatalf("ok=false on a valid body - the mixed message types must not break parsing")
	}
	if pc != 1 {
		t.Errorf("percentConnected = %v, want 1", pc)
	}
	if longest != 0 {
		t.Errorf("longestProcessing = %v, want 0", longest)
	}
	if last != 515187 {
		t.Errorf("lastAccepted = %d, want 515187", last)
	}

	// A reconnecting validator: connected to 2/3 of stake, a block stuck 1.5s.
	degraded := `{"checks":{"` + chainID + `":{"message":{"engine":{"consensus":{"lastAcceptedHeight":600000,"longestProcessingBlock":"1.5s"}},"networking":{"percentConnected":0.6667}}}},"healthy":false}`
	pc, longest, last, ok = parseConsensusHealth([]byte(degraded), chainID)
	if !ok || pc > 0.7 || longest != 1500*time.Millisecond || last != 600000 {
		t.Errorf("degraded parse = (pc=%v, longest=%v, last=%d, ok=%v)", pc, longest, last, ok)
	}

	// Chain check absent (node still coming up) -> not ok.
	if _, _, _, ok := parseConsensusHealth([]byte(`{"checks":{"bls":{"message":"x"}},"healthy":false}`), chainID); ok {
		t.Errorf("expected ok=false when the chain's check is absent")
	}
	// Unparseable body -> not ok.
	if _, _, _, ok := parseConsensusHealth([]byte(`not json`), chainID); ok {
		t.Errorf("expected ok=false on unparseable body")
	}
}
