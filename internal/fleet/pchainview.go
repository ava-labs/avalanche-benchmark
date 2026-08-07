package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/ava-labs/avalanche-benchmark/internal/config"
)

// Every fleet node runs AvalancheGo with --p-chain-follow-only, so its P-chain
// never finishes bootstrapping BY DESIGN: the bootstrapper reports
// Bootstrapped() and then re-arms its frontier poll forever instead of handing
// off to consensus. The permanent consequences are that info.isBootstrapped
// stays false and that platform.getHeight, along with every other platform.*
// call against the local node, answers HTTP 503 "chain is not done
// bootstrapping" forever. Neither is a catch-up phase, so neither may ever be
// waited on.
//
// This file is the single place that asks a local P-chain node about itself,
// using the signals that do work:
//
//   - readiness: the /ext/health bootstrapped check, present and not failing
//   - absolute height: the startup lastAcceptedHeight logged in P.log plus the
//     current process's avalanche_snowman_bs_accepted{chain="P"} counter
//   - replay progress: the periodic fetching/executing blocks log lines
//
// Validator sets and weights are not observable on a follow-only node at all.
// They come from the public P-chain API.
type pchainObservation struct {
	// bootstrapped is the /ext/health bootstrapped check passing. On these
	// nodes green health means caught up and tracking; it is the ready signal.
	bootstrapped bool
	// height is the absolute last accepted P-chain height.
	height uint64
	// heightOK is false when the startup log entry is not in the current P.log,
	// so no absolute height can be derived.
	heightOK bool
	// progress is where the replay currently is, empty when not replaying.
	progress string
}

func pchainLogPath(l layout, node nodeDeployment) string {
	return fmt.Sprintf("%s/%d/logs/P.log", l.data, node.node.Number)
}

// pchainWatchHint names the exact command that watches a replaying node, so a
// reported "bootstrapping" is never a dead end for the operator.
func pchainWatchHint(environment config.FleetEnvironment, node nodeDeployment) string {
	l := layoutFor(environment)
	return fmt.Sprintf(
		"watch: ssh -i %s %s@%s '%s'",
		environment.SSHKeyPath, environment.SSHUser, node.node.Host, l.sudo("tail -f "+pchainLogPath(l, node)),
	)
}

// observePChain is one ssh round trip plus the two HTTP reads, no retries. An
// error means a read failed; an observation with heightOK false means the reads
// succeeded but carry no derivable height.
func (d *Deployer) observePChain(ctx context.Context, remote deployment, node nodeDeployment) (pchainObservation, error) {
	l := layoutFor(remote.environment)
	logPath := pchainLogPath(l, node)
	// One round trip for both log signals. The startup line and the progress
	// line share no fields, so the combined output needs no separator.
	// ponytail: only the live P.log is read. A startup entry that has already
	// rotated away reports as an unobservable height rather than a wrong one.
	logs, err := d.runSSHOutput(ctx, remote, node,
		l.sudo(fmt.Sprintf("grep -a 'starting bootstrapper' %s 2>/dev/null | tail -1", logPath))+"; "+
			l.sudo(fmt.Sprintf("grep -aE 'fetching blocks|executing blocks' %s 2>/dev/null | tail -1", logPath)))
	if err != nil {
		return pchainObservation{}, fmt.Errorf("read %s: %w", logPath, err)
	}

	base := fmt.Sprintf("http://%s:%d", node.node.Host, node.httpPort)
	// A failing check makes /ext/health answer 503 with a complete body, and a
	// failing check is exactly what is being measured, so the body decides.
	code, health, err := d.httpGet(ctx, base+"/ext/health")
	if err != nil {
		return pchainObservation{}, err
	}
	if code != http.StatusOK && code != http.StatusServiceUnavailable {
		return pchainObservation{}, fmt.Errorf("GET %s/ext/health: HTTP %d", base, code)
	}
	bootstrapped, err := healthBootstrapped(health)
	if err != nil {
		return pchainObservation{}, fmt.Errorf("decode %s/ext/health: %w", base, err)
	}

	code, metrics, err := d.httpGet(ctx, base+"/ext/metrics")
	if err != nil {
		return pchainObservation{}, err
	}
	if code != http.StatusOK {
		return pchainObservation{}, fmt.Errorf("GET %s/ext/metrics: HTTP %d", base, code)
	}

	observation := pchainObservation{bootstrapped: bootstrapped}
	if startup, found := parseStartupHeight(string(logs)); found {
		// An absent counter means this process has accepted nothing yet, which
		// is a real zero rather than an unknown.
		accepted, _ := parseAcceptedBlocks(string(metrics))
		observation.height = startup + accepted
		observation.heightOK = true
	}
	if !bootstrapped {
		observation.progress = parseBootstrapProgress(string(logs))
	}
	return observation, nil
}

// pchainReadyOnce is one attempt at the deploy-time P-chain gate. It returns
// the line to print once the node is ready.
func (d *Deployer) pchainReadyOnce(ctx context.Context, prepared deployment) (string, error) {
	if prepared.pchainMode == frozenMode {
		observation, err := d.observePChain(ctx, prepared, prepared.pchain)
		if err != nil {
			return "", err
		}
		if !observation.bootstrapped {
			if observation.progress != "" {
				return "", fmt.Errorf("bootstrapped health check is not passing yet (%s)", observation.progress)
			}
			return "", fmt.Errorf("bootstrapped health check is not passing yet")
		}
		if !observation.heightOK || observation.height == 0 {
			return "", fmt.Errorf("local P-chain height is not observable in %s", pchainLogPath(layoutFor(prepared.environment), prepared.pchain))
		}
		// A frozen node deliberately does not track the upstream, so there is no
		// upstream height to compare it against, and nothing observable on the
		// node proves the shipped archive already contains the L1 conversions.
		// This is a deliberate, known relaxation of the following-mode gate: the
		// archive itself is the evidence.
		return fmt.Sprintf("frozen P-chain node is healthy at local height %d", observation.height), nil
	}

	// Following mode uses exactly the gate fleet freeze uses.
	gate, err := d.freezeGateState(ctx, prepared)
	if err != nil {
		return "", err
	}
	if err := gate.check(); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"P-chain node is synchronized at height %d and the public API shows both validator sets",
		gate.localHeight,
	), nil
}

func (d *Deployer) httpGet(ctx context.Context, url string) (int, []byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, d.http.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	response, err := d.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read %s: %w", url, err)
	}
	return response.StatusCode, body, nil
}

var (
	startupHeightPattern = regexp.MustCompile(`"lastAcceptedHeight"\s*:\s*(\d+)`)
	pctCompletePattern   = regexp.MustCompile(`"pctComplete"\s*:\s*([0-9.]+)`)
	etaPattern           = regexp.MustCompile(`"eta"\s*:\s*"([^"]+)"`)
)

// parseStartupHeight reads lastAcceptedHeight out of the newest "starting
// bootstrapper" entry. That is the database height when the current process
// started, which is what the per-process accepted counter is relative to.
func parseStartupHeight(logs string) (uint64, bool) {
	matches := startupHeightPattern.FindAllStringSubmatch(logs, -1)
	if len(matches) == 0 {
		return 0, false
	}
	height, err := strconv.ParseUint(matches[len(matches)-1][1], 10, 64)
	if err != nil {
		return 0, false
	}
	return height, true
}

// parseAcceptedBlocks reads avalanche_snowman_bs_accepted{chain="P"} out of the
// Prometheus exposition. The counter resets on restart, which is why it is only
// ever added to the startup height of the same process.
func parseAcceptedBlocks(metrics string) (uint64, bool) {
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, labelled, hasLabels := strings.Cut(line, "{")
		if !hasLabels || !strings.HasSuffix(name, "bs_accepted") {
			continue
		}
		labels, value, closed := strings.Cut(labelled, "}")
		if !closed || !strings.Contains(labels, `chain="P"`) {
			continue
		}
		// Prometheus values are floats, so a large counter can arrive in
		// exponent form.
		accepted, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || accepted < 0 {
			continue
		}
		return uint64(accepted), true
	}
	return 0, false
}

// parseBootstrapProgress summarizes the newest periodic bootstrap line. Both
// phases log pctComplete and eta but measure different work, so the phase is
// named.
func parseBootstrapProgress(logs string) string {
	progress := ""
	for _, line := range strings.Split(logs, "\n") {
		match := pctCompletePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		percentage, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		phase := "executing"
		if strings.Contains(line, "fetching blocks") {
			phase = "fetching"
		}
		progress = fmt.Sprintf("%s %.1f%%", phase, percentage)
		if eta := etaPattern.FindStringSubmatch(line); eta != nil {
			progress += ", eta " + eta[1]
		}
	}
	return progress
}

// healthBootstrapped reports the bootstrapped check. An absent check is not
// ready: the chain has not registered it yet.
func healthBootstrapped(body []byte) (bool, error) {
	var reply struct {
		Checks map[string]struct {
			Error *string `json:"error"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return false, err
	}
	check, present := reply.Checks["bootstrapped"]
	return present && check.Error == nil, nil
}
