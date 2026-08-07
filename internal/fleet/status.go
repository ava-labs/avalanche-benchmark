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

	"github.com/ava-labs/avalanche-benchmark/internal/config"
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

// statusBootstrapping is the P-chain mode of a node whose bootstrapped health
// check is not passing yet. It is a replay in progress, not the permanent
// "not done bootstrapping" that --p-chain-follow-only reports forever.
const statusBootstrapping = "bootstrapping"

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
	chain  string
	id     string
	weight string
	state  string
	height string
}

func renderStatusTable(rows []statusRow) string {
	var buffer bytes.Buffer
	writer := tabwriter.NewWriter(&buffer, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "NODE\tDC\tROLE\tCHAIN\tID\tWEIGHT\tSTATE\tHEIGHT")
	for _, row := range rows {
		dc := row.dc
		if strings.TrimSpace(dc) == "" {
			dc = statusNA
		}
		chain := row.chain
		if strings.TrimSpace(chain) == "" {
			chain = statusNA
		}
		fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.number, dc, row.role, chain, row.id, row.weight, row.state, row.height)
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

// intendedUpCount counts machines whose unit is enabled, the same signal the
// address book is rendered from.
func intendedUpCount(rows []statusRow) int {
	count := 0
	for _, row := range rows {
		if row.state == "up" {
			count++
		}
	}
	return count
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

// statusPChainProbe is everything the P-chain machine and the public API
// reported. The local node is follow-only, so the heights are observed from its
// log plus metrics and everything about validator sets comes from the public
// API.
type statusPChainProbe struct {
	number         int
	serviceState   string
	mode           string
	bootstrapped   bool
	progress       string
	watchHint      string
	localOK        bool
	localHeight    uint64
	upstreamOK     bool
	upstreamHeight uint64
	created        bool
	setsOK         bool
	setsSource     string
	managerVisible bool
	// visibleByChain reports, per declared chain, whether the full expected
	// validator set is visible on the P-chain.
	visibleByChain map[string]bool
	// weights is every L1 validator's weight, keyed by NodeID. NodeIDs are
	// unique across chains, so one map covers them all.
	weights  map[string]uint64
	failures []string
	// heightNote explains an unobservable height on a healthy node. It is
	// printed as state, never counted as a failure.
	heightNote string
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
	default:
		visibleAll := probe.managerVisible
		visibleAny := probe.managerVisible
		for _, visible := range probe.visibleByChain {
			visibleAll = visibleAll && visible
			visibleAny = visibleAny || visible
		}
		switch {
		case visibleAll:
			row.l1State = "complete"
		case visibleAny:
			row.l1State = "partial"
		default:
			row.l1State = "missing"
		}
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
		// An observed upstream height is reported even when the local node
		// cannot answer. Only lag and the synced verdict need both readings.
		if probe.upstreamOK {
			row.upstream = strconv.FormatUint(probe.upstreamHeight, 10)
		}
		if !probe.upstreamOK || !probe.localOK {
			return row
		}
		lag := uint64(0)
		if probe.upstreamHeight > probe.localHeight {
			lag = probe.upstreamHeight - probe.localHeight
		}
		row.lag = strconv.FormatUint(lag, 10)
		row.mode = "catching-up"
		if probe.bootstrapped && probe.localHeight >= probe.upstreamHeight {
			row.mode = "synced"
			if row.l1State == "complete" {
				row.ready = "yes"
			}
		}
	default:
		row.upstream = statusUnknown
		row.lag = statusUnknown
	}
	// A node that has not passed its bootstrapped health check is still
	// replaying, whichever mode it was rendered in. That fact outranks the
	// configured mode, because nothing it reports is final yet.
	if !probe.bootstrapped && probe.mode != "" {
		row.mode = statusBootstrapping
		if probe.progress != "" {
			row.mode += " " + probe.progress
		}
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

	rows := make([]statusRow, 0, len(probes))
	for _, probe := range probes {
		row := statusRow{
			number: probe.node.Number,
			dc:     probe.node.DC,
			role:   string(probe.node.Role),
			chain:  chainOf(probe.node),
			id:     probe.identity,
			weight: statusNA,
			state:  probe.state,
			height: probe.height,
		}
		if probe.node.Role == config.RoleValidator || probe.node.Role == config.RoleOracleValidator {
			row.weight = statusNA
			if inv.created {
				row.weight = statusUnknown
				if weight, known := pchain.weights[probe.expectedNodeID]; known {
					row.weight = strconv.FormatUint(weight, 10)
				} else if pchain.setsOK {
					failures = append(failures, fmt.Sprintf(
						"node %d: identity %s (%s) is not in the public P-chain %s validator set",
						probe.node.Number, probe.identity, probe.expectedNodeID, chainOf(probe.node)))
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
	// The address book each machine holds is rendered from fleet-wide systemd
	// intent, which lives on the machines rather than in a file on control, so
	// report the count that intent produced. Without it the only way to answer
	// "why does this node list four peers" is to read node.json on every box.
	fmt.Fprintf(d.out, "ADDRESS BOOK  %d of %d L1 machines intended up\n\n", intendedUpCount(rows), len(rows))
	fmt.Fprint(d.out, renderStatusPChain(pchainStatusRow(pchain)))
	if pchain.setsSource != "" {
		fmt.Fprintf(d.out, "validator sets and weights: %s\n", pchain.setsSource)
	}
	if pchain.heightNote != "" {
		fmt.Fprintln(d.out, pchain.heightNote)
	}
	if pchain.watchHint != "" {
		fmt.Fprintln(d.out, pchain.watchHint)
	}
	failures = append(failures, pchain.failures...)
	// The validator sets are read from the public API, so their absence is a
	// failure no matter what the P-chain machine is doing. A node that is still
	// replaying is a reported state, not a failure.
	if pchain.created && !pchain.setsOK {
		fatal++
	}
	if pchainUnhealthy(pchain) {
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
	height, err := d.statusL1Height(ctx, fmt.Sprintf("%s/ext/bc/%s/rpc", base, inv.l1ChainFor(node)))
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
		number:         inv.pchain.Number,
		serviceState:   statusUnknown,
		created:        inv.created,
		visibleByChain: map[string]bool{},
		weights:        map[string]uint64{},
	}

	// The deployed mode is read BEFORE any public API call, because the mode
	// decides whether the public API applies at all: a frozen fleet is
	// airgapped by design, and asking an unreachable upstream reported its
	// normal steady state as unhealthy (reported 2026-08-05, status used as an
	// automated health probe).
	target, err := inv.target(inv.pchain)
	if err != nil {
		probe.failures = append(probe.failures, fmt.Sprintf("P-chain node %d: %v", inv.pchain.Number, err))
		d.probePublicPChain(ctx, inv, &probe, false)
		return probe
	}
	present, active, enabled, err := d.probeService(ctx, remote, target)
	if err != nil {
		probe.failures = append(probe.failures, fmt.Sprintf("P-chain node %d (%s): read service state: %v", inv.pchain.Number, inv.pchain.Host, err))
		d.probePublicPChain(ctx, inv, &probe, false)
		return probe
	}
	probe.serviceState = collapseServiceState(present, active, enabled)

	if probe.serviceState == statusUp {
		mode, err := d.probePChainMode(ctx, remote, target)
		if err != nil {
			probe.failures = append(probe.failures, fmt.Sprintf("P-chain node %d (%s): read deployed bootstrap mode: %v", inv.pchain.Number, inv.pchain.Host, err))
		}
		probe.mode = mode
	}

	// Validator sets and weights are network facts that a follow-only node can
	// never answer. A frozen fleet answers them from the deployment records; every
	// other state asks the public API, which also stays the fallback when the
	// machine is down and the mode is unknowable.
	if probe.mode == frozenMode {
		recordedValidatorSets(inv, &probe)
	} else {
		d.probePublicPChain(ctx, inv, &probe, probe.mode == followMode)
	}
	if probe.serviceState != statusUp {
		return probe
	}

	observation, err := d.observePChain(ctx, remote, target)
	if err != nil {
		probe.failures = append(probe.failures, fmt.Sprintf(
			"P-chain node %d (%s): observe local P-chain: %v", inv.pchain.Number, inv.pchain.Host, err))
		return probe
	}
	probe.bootstrapped = observation.bootstrapped
	probe.progress = observation.progress
	probe.localOK = observation.heightOK
	probe.localHeight = observation.height
	if !observation.heightOK {
		// The follow-only P chain logs a skipped-bootstrap line every few
		// seconds, so P.log rotates within hours and takes the startup height
		// entry with it. On a node whose bootstrapped health check passes,
		// that is an observability gap, not a fault.
		if observation.bootstrapped {
			probe.heightNote = fmt.Sprintf(
				"local P-chain height: the startup entry rotated out of %s; the height reports again after the next process restart",
				pchainLogPath(layoutFor(inv.environment), target))
		} else {
			probe.failures = append(probe.failures, fmt.Sprintf(
				"P-chain node %d (%s): no startup height in %s, local height not observable",
				inv.pchain.Number, inv.pchain.Host, pchainLogPath(layoutFor(inv.environment), target)))
		}
	}
	if !probe.bootstrapped {
		probe.watchHint = pchainWatchHint(inv.environment, target)
	}
	return probe
}

// pchainUnhealthy decides whether the P-chain probe makes status exit
// nonzero. A running node whose deployed mode cannot be read is unhealthy. An
// unobservable height is unhealthy only when the bootstrapped health check
// also fails: on a healthy node it means the startup entry rotated out of
// P.log, which is reported as state.
func pchainUnhealthy(probe statusPChainProbe) bool {
	if probe.serviceState != statusUp {
		return false
	}
	if probe.mode == "" {
		return true
	}
	return !probe.localOK && !probe.bootstrapped
}

// probePublicPChain reads everything status wants from the public P-chain
// API: the validator sets whenever the chain exists, plus the upstream height
// when asked (only following mode compares against it).
func (d *Deployer) probePublicPChain(ctx context.Context, inv inventory, probe *statusPChainProbe, readHeight bool) {
	network, err := config.LoadNetworkEnvironment(filepath.Join(d.root, ".env"))
	if err != nil {
		probe.failures = append(probe.failures, fmt.Sprintf("P-chain public API: %v", err))
		return
	}
	public := platformvm.NewClient(network.PChainAPI)
	if inv.created {
		d.probePublicValidatorSets(ctx, inv, public, network.PChainAPI, probe)
	}
	if !readHeight {
		return
	}
	// Sampled immediately before the local read so the comparison can never
	// call a lagging node synced.
	upstreamCtx, cancel := context.WithTimeout(ctx, d.http.Timeout)
	height, err := public.GetHeight(upstreamCtx)
	cancel()
	if err != nil {
		probe.failures = append(probe.failures, fmt.Sprintf("P-chain upstream %s: read height: %v", network.PChainAPI, err))
	} else {
		probe.upstreamOK = true
		probe.upstreamHeight = height
	}
}

// recordedValidatorSets answers the validator-set questions from the
// deployment records instead of the public API. A frozen fleet's sets are
// immutable: they are whatever the archive froze, and freezing was gated on
// every set being publicly visible. The recorded weights are the creation
// weights, which match the archive unless a weight was changed on the live
// P-chain between creation and freezing, a sequence the kit's workflow does
// not produce.
func recordedValidatorSets(inv inventory, probe *statusPChainProbe) {
	if !inv.created {
		return
	}
	probe.setsOK = true
	probe.setsSource = "deployment records (frozen fleet; upstream API not consulted)"
	probe.managerVisible = true
	for _, chain := range inv.chains {
		probe.visibleByChain[chain] = true
	}
	for _, node := range inv.public.Nodes {
		if node.Role == config.RoleValidator || node.Role == config.RoleOracleValidator {
			probe.weights[node.NodeID] = node.Weight
		}
	}
}

// probePublicValidatorSets fills in everything the public P-chain API knows
// about the deployment's L1s: whether the management set and every chain's
// validator set are visible, and what each validator weighs.
func (d *Deployer) probePublicValidatorSets(
	ctx context.Context,
	inv inventory,
	public *platformvm.Client,
	uri string,
	probe *statusPChainProbe,
) {
	setsCtx, cancel := context.WithTimeout(ctx, d.http.Timeout)
	defer cancel()
	manager, managerErr := public.GetCurrentValidators(setsCtx, inv.managerSubnetID, nil)
	if managerErr != nil {
		probe.failures = append(probe.failures, fmt.Sprintf(
			"P-chain API %s: read management validator set: %v", uri, managerErr))
		return
	}
	chainValidators := make(map[string][]platformvm.ClientPermissionlessValidator, len(inv.chains))
	for _, chain := range inv.chains {
		validators, err := public.GetCurrentValidators(setsCtx, inv.subnetIDs[chain], nil)
		if err != nil {
			probe.failures = append(probe.failures, fmt.Sprintf(
				"P-chain API %s: read %s validator set: %v", uri, chain, err))
			return
		}
		chainValidators[chain] = validators
	}
	probe.setsOK = true
	expectedByChain := make(map[string]map[ids.NodeID]struct{}, len(inv.chains))
	for _, node := range inv.public.Nodes {
		if node.Role != config.RoleValidator && node.Role != config.RoleOracleValidator {
			continue
		}
		nodeID, err := ids.NodeIDFromString(node.NodeID)
		if err != nil {
			continue
		}
		chain := node.ChainName()
		if expectedByChain[chain] == nil {
			expectedByChain[chain] = make(map[ids.NodeID]struct{})
		}
		expectedByChain[chain][nodeID] = struct{}{}
	}
	expectedManager := make(map[ids.NodeID]struct{})
	for _, node := range inv.public.Managers {
		nodeID, err := ids.NodeIDFromString(node.NodeID)
		if err != nil {
			continue
		}
		expectedManager[nodeID] = struct{}{}
	}
	probe.managerVisible = containsValidators(manager, expectedManager)
	for _, chain := range inv.chains {
		probe.visibleByChain[chain] = containsValidators(chainValidators[chain], expectedByChain[chain])
		for _, validator := range chainValidators[chain] {
			probe.weights[validator.NodeID.String()] = validator.Weight
		}
	}
}

// probeService is the single ssh round trip per machine: unit presence plus
// is-active and is-enabled.
func (d *Deployer) probeService(ctx context.Context, remote deployment, node nodeDeployment) (bool, string, string, error) {
	command := layoutFor(remote.environment).serviceProbe(node)
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
	path := filepath.Join(layoutFor(remote.environment).cfg, strconv.Itoa(node.node.Number), "node.json")
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
