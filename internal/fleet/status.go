package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/vms/platformvm"
)

// Cell placeholders. statusNA means the value does not apply or the node is
// deliberately down, statusUnknown means the value should exist but could not
// be observed.
const (
	statusNA      = "-"
	statusUnknown = "?"
)

// The four collapsed service states.
const (
	statusUp           = "up"
	statusDown         = "down"
	statusFailed       = "failed"
	statusNotInstalled = "not installed"
)

// collapseServiceState maps systemd unit presence plus is-active and
// is-enabled onto the four reported states.
func collapseServiceState(unitPresent bool, isActive, isEnabled string) string {
	if !unitPresent {
		return statusNotInstalled
	}
	switch strings.TrimSpace(isActive) {
	case "active", "activating":
		return statusUp
	case "failed":
		return statusFailed
	}
	if strings.TrimSpace(isEnabled) == "failed" {
		return statusFailed
	}
	// ponytail: enabled-but-inactive and disabled-and-inactive both collapse to
	// down, because the reported vocabulary has no word that separates them.
	return statusDown
}

type statusRow struct {
	number int
	dc     string
	role   string
	id     string
	weight string
	state  string
	height string
}

func renderStatusTable(rows []statusRow) string {
	var buffer bytes.Buffer
	writer := tabwriter.NewWriter(&buffer, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "NODE\tDC\tROLE\tID\tWEIGHT\tSTATE\tHEIGHT")
	for _, row := range rows {
		dc := row.dc
		if strings.TrimSpace(dc) == "" {
			dc = statusNA
		}
		fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.number, dc, row.role, row.id, row.weight, row.state, row.height)
	}
	writer.Flush()
	return buffer.String()
}

type statusPChainRow struct {
	number   int
	mode     string
	local    string
	upstream string
	lag      string
	l1State  string
	ready    string
}

func renderStatusPChain(row statusPChainRow) string {
	var buffer bytes.Buffer
	writer := tabwriter.NewWriter(&buffer, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "P-CHAIN\tMODE\tLOCAL HEIGHT\tUPSTREAM HEIGHT\tLAG\tL1 STATE\tREADY TO FREEZE")
	fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
		row.number, row.mode, row.local, row.upstream, row.lag, row.l1State, row.ready)
	writer.Flush()
	return buffer.String()
}

// statusProbe is everything one L1 machine reported.
type statusProbe struct {
	node           config.Node
	identity       string
	expectedNodeID string
	state          string
	height         string
	runtimeNodeID  string
	apiAnswered    bool
	drift          bool
	failures       []string
}

// statusPChainProbe is everything the P-chain machine reported.
type statusPChainProbe struct {
	number         int
	serviceState   string
	mode           string
	localOK        bool
	localHeight    uint64
	upstreamOK     bool
	upstreamHeight uint64
	created        bool
	setsOK         bool
	mainVisible    bool
	managerVisible bool
	mainWeights    map[string]uint64
	failures       []string
}

// pchainStatusRow turns the raw P-chain observations into printable cells.
func pchainStatusRow(probe statusPChainProbe) statusPChainRow {
	row := statusPChainRow{
		number:   probe.number,
		mode:     statusNA,
		local:    statusNA,
		upstream: statusNA,
		lag:      statusNA,
		l1State:  statusNA,
		ready:    statusNA,
	}
	if probe.serviceState != statusUp {
		return row
	}
	row.mode = statusUnknown
	row.ready = "no"

	row.local = statusUnknown
	if probe.localOK {
		row.local = strconv.FormatUint(probe.localHeight, 10)
	}
	switch {
	case !probe.created:
		row.l1State = statusNA
	case !probe.setsOK:
		row.l1State = statusUnknown
	case probe.mainVisible && probe.managerVisible:
		row.l1State = "complete"
	case probe.mainVisible || probe.managerVisible:
		row.l1State = "partial"
	default:
		row.l1State = "missing"
	}

	switch probe.mode {
	case frozenMode:
		row.mode = frozenMode
		row.upstream = statusNA
		row.lag = statusNA
		row.ready = statusNA
	case followMode:
		row.upstream = statusUnknown
		row.lag = statusUnknown
		if !probe.upstreamOK || !probe.localOK {
			return row
		}
		row.upstream = strconv.FormatUint(probe.upstreamHeight, 10)
		lag := uint64(0)
		if probe.upstreamHeight > probe.localHeight {
			lag = probe.upstreamHeight - probe.localHeight
		}
		row.lag = strconv.FormatUint(lag, 10)
		row.mode = "catching-up"
		if probe.localHeight >= probe.upstreamHeight {
			row.mode = "synced"
			if row.l1State == "complete" {
				row.ready = "yes"
			}
		}
	default:
		row.upstream = statusUnknown
		row.lag = statusUnknown
	}
	return row
}

// fatalProbe reports whether one machine's observation must make status exit
// nonzero. Deliberate down and not installed are valid drill states.
func fatalProbe(state string, apiAnswered, drift bool) bool {
	if drift {
		return true
	}
	return state == statusUp && !apiAnswered
}

func (d *Deployer) Status(ctx context.Context) error {
	inv, err := d.inventory()
	if err != nil {
		return err
	}
	remote := deployment{environment: inv.environment}

	machines := inv.l1Nodes()
	probes := make([]statusProbe, len(machines))
	var pchain statusPChainProbe

	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		pchain = d.probePChainStatus(ctx, inv, remote)
	}()
	for index, node := range machines {
		group.Add(1)
		go func(index int, node config.Node) {
			defer group.Done()
			probes[index] = d.probeNodeStatus(ctx, inv, remote, node)
		}(index, node)
	}
	group.Wait()

	var failures []string
	var drifts []string
	fatal := 0
	if inv.created && !pchain.setsOK && pchain.serviceState != statusUp {
		failures = append(failures, fmt.Sprintf(
			"local P-chain view is unavailable (node %d service is %s), validator weights not observed",
			pchain.number, pchain.serviceState))
	}

	rows := make([]statusRow, 0, len(probes))
	for _, probe := range probes {
		row := statusRow{
			number: probe.node.Number,
			dc:     probe.node.DC,
			role:   string(probe.node.Role),
			id:     probe.identity,
			weight: statusNA,
			state:  probe.state,
			height: probe.height,
		}
		if probe.node.Role == config.RoleValidator {
			row.weight = statusNA
			if inv.created {
				row.weight = statusUnknown
				if weight, known := pchain.mainWeights[probe.expectedNodeID]; known {
					row.weight = strconv.FormatUint(weight, 10)
				} else if pchain.setsOK {
					failures = append(failures, fmt.Sprintf(
						"node %d: identity %s (%s) is not in the local P-chain main validator set",
						probe.node.Number, probe.identity, probe.expectedNodeID))
				}
			}
		}
		rows = append(rows, row)
		failures = append(failures, probe.failures...)
		if probe.drift {
			drifts = append(drifts, fmt.Sprintf(
				"node %d runs %s but placement assigns identity %s (%s)",
				probe.node.Number, probe.runtimeNodeID, probe.identity, probe.expectedNodeID))
		}
		if fatalProbe(probe.state, probe.apiAnswered, probe.drift) {
			fatal++
		}
	}

	fmt.Fprint(d.out, renderStatusTable(rows))
	fmt.Fprintln(d.out)
	fmt.Fprint(d.out, renderStatusPChain(pchainStatusRow(pchain)))
	failures = append(failures, pchain.failures...)
	if pchain.serviceState == statusUp &&
		(!pchain.localOK || pchain.mode == "" || (pchain.created && !pchain.setsOK)) {
		fatal++
	}

	if len(drifts) > 0 {
		fmt.Fprintln(d.out)
		fmt.Fprintln(d.out, "IDENTITY DRIFT")
		for _, drift := range drifts {
			fmt.Fprintf(d.out, "  %s\n", drift)
		}
	}
	if len(failures) > 0 {
		fmt.Fprintln(d.out)
		fmt.Fprintln(d.out, "PROBE FAILURES")
		for _, failure := range failures {
			fmt.Fprintf(d.out, "  %s\n", failure)
		}
	}
	if fatal > 0 {
		return fmt.Errorf("fleet status found %d unhealthy node(s)", fatal)
	}
	return nil
}

func (d *Deployer) probeNodeStatus(ctx context.Context, inv inventory, remote deployment, node config.Node) statusProbe {
	probe := statusProbe{
		node:     node,
		identity: statusUnknown,
		state:    statusUnknown,
		height:   statusNA,
	}
	target, err := inv.target(node)
	if err != nil {
		probe.failures = append(probe.failures, fmt.Sprintf("node %d: %v", node.Number, err))
		return probe
	}
	probe.identity = target.identity.Identity
	probe.expectedNodeID = target.identity.NodeID

	present, active, enabled, err := d.probeService(ctx, remote, target)
	if err != nil {
		probe.failures = append(probe.failures, fmt.Sprintf("node %d (%s): read service state: %v", node.Number, node.Host, err))
		return probe
	}
	probe.state = collapseServiceState(present, active, enabled)
	if probe.state != statusUp {
		return probe
	}

	base := fmt.Sprintf("http://%s:%d", node.Host, target.httpPort)
	runtimeNodeID, err := d.runtimeNodeID(ctx, node.Host, target.httpPort)
	if err != nil {
		probe.failures = append(probe.failures, fmt.Sprintf("node %d (%s): read runtime NodeID: %v", node.Number, node.Host, err))
	} else {
		probe.apiAnswered = true
		probe.runtimeNodeID = runtimeNodeID
		probe.drift = runtimeNodeID != probe.expectedNodeID
	}

	probe.height = statusUnknown
	if !inv.created {
		probe.failures = append(probe.failures, fmt.Sprintf("node %d (%s): L1 is not created yet, height unavailable", node.Number, node.Host))
		return probe
	}
	height, err := d.statusL1Height(ctx, fmt.Sprintf("%s/ext/bc/%s/rpc", base, inv.chainID))
	if err != nil {
		probe.apiAnswered = false
		probe.failures = append(probe.failures, fmt.Sprintf("node %d (%s): read L1 height: %v", node.Number, node.Host, err))
		return probe
	}
	probe.height = strconv.FormatUint(height, 10)
	return probe
}

func (d *Deployer) probePChainStatus(ctx context.Context, inv inventory, remote deployment) statusPChainProbe {
	probe := statusPChainProbe{
		number:       inv.pchain.Number,
		serviceState: statusUnknown,
		created:      inv.created,
		mainWeights:  map[string]uint64{},
	}
	target, err := inv.target(inv.pchain)
	if err != nil {
		probe.failures = append(probe.failures, fmt.Sprintf("P-chain node %d: %v", inv.pchain.Number, err))
		return probe
	}
	present, active, enabled, err := d.probeService(ctx, remote, target)
	if err != nil {
		probe.failures = append(probe.failures, fmt.Sprintf("P-chain node %d (%s): read service state: %v", inv.pchain.Number, inv.pchain.Host, err))
		return probe
	}
	probe.serviceState = collapseServiceState(present, active, enabled)
	if probe.serviceState != statusUp {
		return probe
	}

	mode, err := d.probePChainMode(ctx, remote, target)
	if err != nil {
		probe.failures = append(probe.failures, fmt.Sprintf("P-chain node %d (%s): read deployed bootstrap mode: %v", inv.pchain.Number, inv.pchain.Host, err))
	}
	probe.mode = mode

	client := platformvm.NewClient(fmt.Sprintf("http://%s:%d", inv.pchain.Host, target.httpPort))
	if probe.mode == followMode {
		// Sampled immediately before the local read so the comparison can never
		// call a lagging node synced.
		network, err := config.LoadNetworkEnvironment(filepath.Join(d.root, ".env"))
		if err != nil {
			probe.failures = append(probe.failures, fmt.Sprintf("P-chain upstream: %v", err))
		} else {
			upstreamCtx, cancel := context.WithTimeout(ctx, d.http.Timeout)
			height, err := platformvm.NewClient(network.PChainAPI).GetHeight(upstreamCtx)
			cancel()
			if err != nil {
				probe.failures = append(probe.failures, fmt.Sprintf("P-chain upstream %s: read height: %v", network.PChainAPI, err))
			} else {
				probe.upstreamOK = true
				probe.upstreamHeight = height
			}
		}
	}

	heightCtx, cancelHeight := context.WithTimeout(ctx, d.http.Timeout)
	local, err := client.GetHeight(heightCtx)
	cancelHeight()
	if err != nil {
		probe.failures = append(probe.failures, fmt.Sprintf("P-chain node %d (%s): read local height: %v", inv.pchain.Number, inv.pchain.Host, err))
	} else {
		probe.localOK = true
		probe.localHeight = local
	}
	if !inv.created {
		return probe
	}

	setsCtx, cancelSets := context.WithTimeout(ctx, d.http.Timeout)
	defer cancelSets()
	manager, managerErr := client.GetCurrentValidators(setsCtx, inv.managerSubnetID, nil)
	main, mainErr := client.GetCurrentValidators(setsCtx, inv.subnetID, nil)
	if managerErr != nil || mainErr != nil {
		probe.failures = append(probe.failures, fmt.Sprintf(
			"P-chain node %d (%s): read local validator sets: management=%v main=%v",
			inv.pchain.Number, inv.pchain.Host, managerErr, mainErr))
		return probe
	}
	probe.setsOK = true
	expectedMain := make(map[ids.NodeID]struct{})
	for _, node := range inv.public.Nodes {
		if node.Role == config.RoleValidator {
			nodeID, err := ids.NodeIDFromString(node.NodeID)
			if err != nil {
				continue
			}
			expectedMain[nodeID] = struct{}{}
		}
	}
	expectedManager := make(map[ids.NodeID]struct{})
	for _, node := range inv.public.Managers {
		nodeID, err := ids.NodeIDFromString(node.NodeID)
		if err != nil {
			continue
		}
		expectedManager[nodeID] = struct{}{}
	}
	probe.mainVisible = containsValidators(main, expectedMain)
	probe.managerVisible = containsValidators(manager, expectedManager)
	for _, validator := range main {
		probe.mainWeights[validator.NodeID.String()] = validator.Weight
	}
	return probe
}

// probeService is the single ssh round trip per machine: unit presence plus
// is-active and is-enabled.
func (d *Deployer) probeService(ctx context.Context, remote deployment, node nodeDeployment) (bool, string, string, error) {
	unit := serviceName(node)
	command := fmt.Sprintf(
		"if sudo systemctl cat %[1]s >/dev/null 2>&1; then printf 'present '; else printf 'missing '; fi; "+
			"printf '%%s ' \"$(sudo systemctl is-active %[1]s)\"; "+
			"printf '%%s' \"$(sudo systemctl is-enabled %[1]s 2>/dev/null)\"",
		unit)
	output, err := d.runSSHOutput(ctx, remote, node, command)
	if err != nil {
		return false, "", "", err
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 {
		return false, "", "", fmt.Errorf("unexpected service probe output %q", strings.TrimSpace(string(output)))
	}
	enabled := ""
	if len(fields) > 2 {
		enabled = fields[2]
	}
	return fields[0] == "present", fields[1], enabled, nil
}

// probePChainMode reads the rendered configuration on the P-chain machine. A
// frozen render writes explicit empty bootstrap lists, a following render omits
// them entirely.
func (d *Deployer) probePChainMode(ctx context.Context, remote deployment, node nodeDeployment) (string, error) {
	path := filepath.Join(remoteConfigDir, strconv.Itoa(node.node.Number), "node.json")
	output, err := d.runSSHOutput(ctx, remote, node, "cat "+path)
	if err != nil {
		return "", err
	}
	var rendered map[string]any
	if err := json.Unmarshal(output, &rendered); err != nil {
		return "", fmt.Errorf("decode %s: %w", path, err)
	}
	bootstrapIPs, hasIPs := rendered["bootstrap-ips"]
	bootstrapIDs, hasIDs := rendered["bootstrap-ids"]
	switch {
	case !hasIPs && !hasIDs:
		return followMode, nil
	case hasIPs && hasIDs && bootstrapIPs == "" && bootstrapIDs == "":
		return frozenMode, nil
	default:
		return "", fmt.Errorf("%s has bootstrap-ips=%v bootstrap-ids=%v, which is neither a frozen nor a following render", path, bootstrapIPs, bootstrapIDs)
	}
}

func (d *Deployer) statusL1Height(ctx context.Context, url string) (uint64, error) {
	var raw string
	if err := d.statusRPC(ctx, url, "eth_blockNumber", []any{}, &raw); err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimPrefix(raw, "0x"), 16, 64)
}

// statusRPC does one JSON-RPC attempt with the existing HTTP client, no retry.
func (d *Deployer) statusRPC(ctx context.Context, url, method string, params, result any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := d.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", response.Status)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return fmt.Errorf("%s: %s", method, envelope.Error)
	}
	if len(envelope.Result) == 0 {
		return fmt.Errorf("%s: empty result", method)
	}
	return json.Unmarshal(envelope.Result, result)
}
