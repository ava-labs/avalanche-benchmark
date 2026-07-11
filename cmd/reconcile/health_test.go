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

// TestWedgeFrozen walks the fork-wedge detector through a poll sequence: it
// fires only after wedgeFrozenPolls consecutive unchanged CATCHING UP heights,
// and any movement or state change resets the count. A SERVING or DOWN node
// can never trip it, however static its height reading.
func TestWedgeFrozen(t *testing.T) {
	steps := []struct {
		name       string
		state      nodeHealth
		block      uint64
		prevBlock  uint64
		prevFrozen int
		wantFrozen int
		wantWedged bool
	}{
		{"first sighting, no history", healthCatchingUp, 277840, 0, 0, 0, false},
		{"frozen once", healthCatchingUp, 277840, 277840, 0, 1, false},
		{"frozen twice: WEDGED", healthCatchingUp, 277840, 277840, 1, 2, true},
		{"movement resets", healthCatchingUp, 277900, 277840, 1, 0, false},
		{"healthy sync never freezes", healthCatchingUp, 300000, 277900, 0, 0, false},
		{"serving node is never wedged even if static", healthServing, 500, 500, 5, 0, false},
		{"bootstrapping resets (rebuild in progress)", healthBootstrapping, 0, 0, 2, 0, false},
		{"down resets", healthDown, 0, 0, 2, 0, false},
		{"wedged at height 0 still fires after the window", healthCatchingUp, 0, 0, 1, 2, true},
	}
	for _, s := range steps {
		frozen, wedged := wedgeFrozen(s.state, s.block, s.prevBlock, s.prevFrozen)
		if frozen != s.wantFrozen || wedged != s.wantWedged {
			t.Errorf("%s: wedgeFrozen(%v,%d,%d,%d) = (%d,%v), want (%d,%v)",
				s.name, s.state, s.block, s.prevBlock, s.prevFrozen, frozen, wedged, s.wantFrozen, s.wantWedged)
		}
	}
}

// TestParseBootstrapCounter feeds real /ext/metrics lines (captured live on a1
// 2026-07-11). The P-chain filter is the load-bearing case: its bs counters
// (chain="P") advance forever under --p-chain-follow-only and must never read
// as L1 progress.
func TestParseBootstrapCounter(t *testing.T) {
	const chainID = "e3JzNuuG3CXifcWh1Sik55LbFTyCxMqkQUR4ohvZCDpNGzunB"
	body := `# HELP avalanche_snowman_bs_accepted Number of blocks accepted during bootstrapping
# TYPE avalanche_snowman_bs_accepted counter
avalanche_snowman_bs_accepted{chain="P"} 3504
avalanche_snowman_bs_accepted{chain="` + chainID + `"} 9734
# HELP avalanche_snowman_bs_fetched Number of blocks fetched during bootstrapping
# TYPE avalanche_snowman_bs_fetched counter
avalanche_snowman_bs_fetched{chain="P"} 3504
avalanche_snowman_bs_fetched{chain="` + chainID + `"} 9734
`
	if got, ok := parseBootstrapCounter(body, chainID); !ok || got != 19468 {
		t.Errorf("parseBootstrapCounter = (%d,%v), want (19468,true)", got, ok)
	}
	// Prometheus renders big counters in scientific notation.
	sci := `avalanche_snowman_bs_fetched{chain="` + chainID + `"} 4.157273e+06` + "\n"
	if got, ok := parseBootstrapCounter(sci, chainID); !ok || got != 4157273 {
		t.Errorf("scientific notation: got (%d,%v), want (4157273,true)", got, ok)
	}
	// Chain's counters absent (engine not started yet, or wrong chain) -> not ok.
	if _, ok := parseBootstrapCounter(`avalanche_snowman_bs_fetched{chain="P"} 3504`+"\n", chainID); ok {
		t.Errorf("expected ok=false when only the P-chain reports")
	}
	if _, ok := parseBootstrapCounter("", chainID); ok {
		t.Errorf("expected ok=false on empty body")
	}
}

// TestMadeProgress covers the wipe-loop fix's decision core: what counts as
// forward motion for a waited-on machine between two polls.
func TestMadeProgress(t *testing.T) {
	tests := []struct {
		name             string
		prevState, state nodeHealth
		prevBlock, block uint64
		prevBS, bs       uint64
		prevBSOK, bsOK   bool
		want             bool
	}{
		{"state change is progress", healthDown, healthBootstrapping, 0, 0, 0, 0, false, false, true},
		{"bootstrap counters advancing is progress", healthBootstrapping, healthBootstrapping, 0, 0, 4150000, 4150900, true, true, true},
		{"bootstrap counters frozen is NOT progress", healthBootstrapping, healthBootstrapping, 0, 0, 4150000, 4150000, true, true, false},
		{"silent Clear window (bs present but zero, unchanging) is NOT progress", healthBootstrapping, healthBootstrapping, 0, 0, 0, 0, true, true, false},
		{"first bs reading (engine came up) is progress", healthBootstrapping, healthBootstrapping, 0, 0, 0, 0, false, true, true},
		{"metrics unreadable both polls is NOT progress", healthBootstrapping, healthBootstrapping, 0, 0, 0, 0, false, false, false},
		{"height movement is progress", healthCatchingUp, healthCatchingUp, 100, 400, 0, 0, false, false, true},
		{"frozen height is NOT progress", healthCatchingUp, healthCatchingUp, 400, 400, 0, 0, false, false, false},
		{"post-restart lower bs alone is NOT progress", healthBootstrapping, healthBootstrapping, 0, 0, 4150000, 12, true, true, false},
	}
	for _, tt := range tests {
		if got := madeProgress(tt.prevState, tt.state, tt.prevBlock, tt.block, tt.prevBS, tt.bs, tt.prevBSOK, tt.bsOK); got != tt.want {
			t.Errorf("%s: madeProgress = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestStallBudget(t *testing.T) {
	// BOOTSTRAPPING gets the generous window (silent Bootstrapper.Clear + state
	// sync); everything else keeps the old 10 minutes.
	if stallBudget(healthBootstrapping) != bootstrapStallBudget {
		t.Errorf("bootstrapping budget = %v", stallBudget(healthBootstrapping))
	}
	for _, s := range []nodeHealth{healthDown, healthCatchingUp, healthServing} {
		if stallBudget(s) != defaultStallBudget {
			t.Errorf("budget(%v) = %v, want %v", s, stallBudget(s), defaultStallBudget)
		}
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
