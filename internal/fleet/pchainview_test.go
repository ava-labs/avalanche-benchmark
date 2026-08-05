package fleet

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
)

// One real startup line and one real progress line, as AvalancheGo writes them
// into P.log.
const sampleStartupLine = `[07-27|02:11:03.145] INFO <P Chain> bootstrap/bootstrapper.go:192 starting bootstrapper {"lastAcceptedID": "2AS5J2n1JbjJUPzYQmzbLdt5jJ1TQfBRMFhpaQrDCXKvW1Dt6q", "lastAcceptedHeight": 0}`

const sampleExecutingLine = `[07-27|02:15:41.001] INFO <P Chain> bootstrap/storage.go:256 executing blocks {"numExecuted": 283000, "numToExecute": 290135, "eta": "9s", "pctComplete": 97.51}`

const sampleFetchingLine = `[07-27|02:13:07.512] INFO <P Chain> bootstrap/bootstrapper.go:648 fetching blocks {"numFetchedBlocks": 116700, "numTotalBlocks": 290135, "eta": "3m12s", "pctComplete": 40.22}`

func TestParseStartupHeightTakesTheNewestEntry(t *testing.T) {
	if _, found := parseStartupHeight(""); found {
		t.Fatal("an empty log must not report a height")
	}
	if _, found := parseStartupHeight("no bootstrapper line here\n"); found {
		t.Fatal("an unrelated log must not report a height")
	}
	height, found := parseStartupHeight(sampleStartupLine)
	if !found || height != 0 {
		t.Fatalf("height = %d, found = %v, want 0 and true", height, found)
	}
	// A restart writes a new entry; the newest one pairs with the reset counter.
	restarted := sampleStartupLine + "\n" +
		strings.Replace(sampleStartupLine, `"lastAcceptedHeight": 0`, `"lastAcceptedHeight": 290135`, 1)
	height, found = parseStartupHeight(restarted)
	if !found || height != 290135 {
		t.Fatalf("restarted height = %d, found = %v, want 290135 and true", height, found)
	}
}

func TestParseAcceptedBlocks(t *testing.T) {
	metrics := strings.Join([]string{
		"# HELP avalanche_snowman_bs_accepted Number of blocks accepted during bootstrapping",
		"# TYPE avalanche_snowman_bs_accepted counter",
		`avalanche_snowman_bs_fetched{chain="P"} 290135`,
		`avalanche_snowman_bs_accepted{chain="C"} 12`,
		`avalanche_snowman_bs_accepted{chain="P"} 290135`,
		"",
	}, "\n")
	accepted, found := parseAcceptedBlocks(metrics)
	if !found || accepted != 290135 {
		t.Fatalf("accepted = %d, found = %v, want 290135 and true", accepted, found)
	}

	// Prometheus writes floats, so a counter can arrive in exponent form.
	exponent, found := parseAcceptedBlocks(`avalanche_snowman_bs_accepted{chain="P"} 2.90135e+05`)
	if !found || exponent != 290135 {
		t.Fatalf("exponent form = %d, found = %v, want 290135 and true", exponent, found)
	}

	if _, found := parseAcceptedBlocks(`avalanche_snowman_bs_accepted{chain="X"} 5`); found {
		t.Fatal("another chain's counter must not be read as the P-chain's")
	}
	if _, found := parseAcceptedBlocks("avalanche_process_uptime 42\n"); found {
		t.Fatal("an unrelated exposition must not report a counter")
	}
}

func TestParseBootstrapProgressNamesThePhase(t *testing.T) {
	if got := parseBootstrapProgress(sampleStartupLine); got != "" {
		t.Fatalf("progress without a progress line = %q, want empty", got)
	}
	if got := parseBootstrapProgress(sampleExecutingLine); got != "executing 97.5%, eta 9s" {
		t.Fatalf("executing progress = %q", got)
	}
	if got := parseBootstrapProgress(sampleFetchingLine); got != "fetching 40.2%, eta 3m12s" {
		t.Fatalf("fetching progress = %q", got)
	}
	// The newest line wins: fetching precedes executing in the same file.
	both := sampleFetchingLine + "\n" + sampleExecutingLine
	if got := parseBootstrapProgress(both); got != "executing 97.5%, eta 9s" {
		t.Fatalf("combined progress = %q", got)
	}
}

const passingHealthBody = `{"checks":{"bootstrapped":{"timestamp":"2026-07-27T02:20:00Z","duration":1000},` +
	`"network":{"message":{"connectedPeers":617},"timestamp":"2026-07-27T02:20:00Z","duration":900}},"healthy":true}`

const failingHealthBody = `{"checks":{"bootstrapped":{"error":"not yet run",` +
	`"timestamp":"2026-07-27T02:20:00Z","duration":0}},"healthy":false}`

func TestHealthBootstrappedNeedsThePassingCheck(t *testing.T) {
	bootstrapped, err := healthBootstrapped([]byte(passingHealthBody))
	if err != nil {
		t.Fatal(err)
	}
	if !bootstrapped {
		t.Fatal("a passing bootstrapped check must read as bootstrapped")
	}

	// 503 bodies are complete, and this is the case being measured.
	bootstrapped, err = healthBootstrapped([]byte(failingHealthBody))
	if err != nil {
		t.Fatal(err)
	}
	if bootstrapped {
		t.Fatal("a failing bootstrapped check must not read as bootstrapped")
	}

	// The chain has not registered its check yet: not ready, not an error.
	bootstrapped, err = healthBootstrapped([]byte(`{"checks":{"network":{}},"healthy":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if bootstrapped {
		t.Fatal("an absent bootstrapped check must not read as bootstrapped")
	}

	if _, err := healthBootstrapped([]byte("not json")); err == nil {
		t.Fatal("an undecodable health body must fail loudly")
	}
}

// TestObservePChainDrivesTheFrozenGate wires the real observation path against
// a local stand-in node: the log comes from the runner, health and metrics from
// HTTP. No platform.* call is involved anywhere, which is the whole point.
func TestObservePChainDrivesTheFrozenGate(t *testing.T) {
	health := failingHealthBody
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ext/health":
			if health == failingHealthBody {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			io.WriteString(w, health)
		case "/ext/metrics":
			io.WriteString(w, `avalanche_snowman_bs_accepted{chain="P"} 1200`+"\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	host, portText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	deployer := &Deployer{
		out:    io.Discard,
		runner: &recordingRunner{output: []byte(sampleStartupLine + "\n" + sampleExecutingLine + "\n")},
		http:   &http.Client{Timeout: 5 * time.Second},
	}
	pchain := nodeDeployment{node: config.Node{Number: 13, Host: host}, httpPort: port}
	prepared := deployment{pchainMode: frozenMode, pchain: pchain}
	ctx := context.Background()

	observation, err := deployer.observePChain(ctx, prepared, pchain)
	if err != nil {
		t.Fatal(err)
	}
	if observation.bootstrapped {
		t.Fatal("a failing health check must not read as bootstrapped")
	}
	// Startup height 0 plus 1200 accepted by this process.
	if !observation.heightOK || observation.height != 1200 {
		t.Fatalf("height = %d, heightOK = %v, want 1200 and true", observation.height, observation.heightOK)
	}
	if observation.progress != "executing 97.5%, eta 9s" {
		t.Fatalf("progress = %q", observation.progress)
	}

	if _, err := deployer.pchainReadyOnce(ctx, prepared); err == nil ||
		!strings.Contains(err.Error(), "executing 97.5%") {
		t.Fatalf("frozen refusal = %v, want the replay progress", err)
	}

	// Frozen mode is deliberately relaxed: healthy plus a nonzero local height.
	health = passingHealthBody
	message, err := deployer.pchainReadyOnce(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "1200") {
		t.Fatalf("frozen readiness message = %q, want the local height", message)
	}
}

func TestPChainWatchHintNamesTheExactCommand(t *testing.T) {
	hint := pchainWatchHint(
		config.FleetEnvironment{SSHUser: "ubuntu", SSHKeyPath: "/home/me/key.pem", SystemInstall: true},
		nodeDeployment{node: config.Node{Number: 13, Host: "54.67.148.21"}},
	)
	want := "watch: ssh -i /home/me/key.pem ubuntu@54.67.148.21 " +
		"'sudo tail -f /var/lib/avalanche-benchmark/13/logs/P.log'"
	if hint != want {
		t.Fatalf("hint = %q, want %q", hint, want)
	}
}
