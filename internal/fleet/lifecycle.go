package fleet

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanchego/ids"
)

// selectNodes resolves maintenance selectors against a candidate machine list.
// A selector is a NODE NUMBER, nothing else; multiple selectors are a union; no
// selector means every candidate. A selector that matches nothing is a loud
// error, because silently doing less than asked is how a drill lies.
//
// There is deliberately no dc=<tag> selector. One `destroy dc=A` wipes half a
// two-site fleet in a single keystroke, and half is the worst possible number:
// the state-sync beacon list is weight 1 per entry with alpha = count/2 + 1, so
// losing half leaves every survivor exactly one beacon short of alpha and no
// node can state-sync for the rest of the incident (measured 2026-07-31: three
// validators stuck in an infinite frontier loop with local data already at the
// network's height). A DC drill is written out as node numbers instead. The
// dc= tag in nodes.ini stays, for the status column.
func selectNodes(candidates []config.Node, selectors []string) ([]config.Node, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("inventory has no L1 node to operate on")
	}
	if len(selectors) == 0 {
		return candidates, nil
	}
	// Accept "1,11,12" as well as separate arguments. Comma-separated is what a
	// hand reaches for under drill pressure, and rejecting it wastes a round trip
	// at the exact moment the fleet is degraded.
	expanded := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		for _, part := range strings.Split(selector, ",") {
			if part = strings.TrimSpace(part); part != "" {
				expanded = append(expanded, part)
			}
		}
	}
	if len(expanded) == 0 {
		return nil, fmt.Errorf("selectors %q contain no node number", strings.Join(selectors, " "))
	}

	chosen := make(map[int]struct{}, len(candidates))
	for _, selector := range expanded {
		matched := 0
		if strings.HasPrefix(selector, "dc=") {
			return nil, fmt.Errorf("selector %q is not supported: dc= selectors were removed because one command must not be able to take a whole site down; list the node numbers instead", selector)
		}
		number, err := strconv.Atoi(selector)
		if err != nil {
			return nil, fmt.Errorf("selector %q must be a node number", selector)
		}
		for _, node := range candidates {
			if node.Number == number {
				chosen[node.Number] = struct{}{}
				matched++
			}
		}
		if matched == 0 {
			return nil, fmt.Errorf("selector %q matches no L1 node", selector)
		}
	}
	// Inventory order, not selector order: phases read better bottom-up by
	// machine number and the set is a union anyway.
	result := make([]config.Node, 0, len(chosen))
	for _, node := range candidates {
		if _, wanted := chosen[node.Number]; wanted {
			result = append(result, node)
		}
	}
	return result, nil
}

// lifecycleTargets builds the addressing-only deployment view that start, stop,
// and destroy operate on. It renders nothing: these verbs never change
// configuration, only process and data state.
func (d *Deployer) lifecycleTargets(selectors []string) (deployment, inventory, error) {
	inv, err := d.inventory()
	if err != nil {
		return deployment{}, inventory{}, err
	}
	chosen, err := selectNodes(inv.l1Nodes(), selectors)
	if err != nil {
		return deployment{}, inventory{}, err
	}
	state := deployment{
		environment:   inv.environment,
		chainID:       inv.chainID,
		subnetID:      inv.subnetID,
		oracleChainID: inv.oracleChainID,
	}
	for _, node := range chosen {
		target, err := inv.target(node)
		if err != nil {
			return deployment{}, inventory{}, err
		}
		state.selected = append(state.selected, target)
	}
	return state, inv, nil
}

type lifecyclePhase struct {
	name   string
	action func(context.Context, deployment, nodeDeployment) error
}

func (d *Deployer) runPhases(ctx context.Context, state deployment, phases []lifecyclePhase) error {
	for _, phase := range phases {
		if err := d.phase(ctx, state, phase.name, phase.action); err != nil {
			return err
		}
	}
	return nil
}

// Start converges the selected L1 machines and touches NOTHING ELSE. A node that
// is already running the identity placement assigns to it is left strictly alone:
// not stopped, not restarted, not even re-pushed. Only a node that is down, or up
// serving the wrong identity, or up but not answering its API, is taken through
// stop, identity, config, start.
//
// That restraint is the point. Start used to stop every selected machine
// unconditionally, so `fleet start 5 6 7` took three healthy boxes down at once
// to fix nothing, which is indistinguishable from an outage on a fleet that only
// tolerates losing a quarter of its stake. Restarting a healthy node is never
// free: it drops its peers, re-enters bootstrap behind the 75% connected-stake
// gate, and on a degraded fleet may not come back at all.
//
// Start deliberately does NOT wait for readiness. A node finishes bootstrapping
// only once 75% of stake is connected (avalanchego builds its startup tracker as
// (3*bootstrapWeight+3)/4), so when several nodes are down, none of them can
// become ready until the others are also running. A blocking start would
// therefore deadlock in exactly the recovery case it exists for: restarting node
// A waits on node B, which has not been started yet because the command is still
// blocked on A. Start is a state change; observe convergence with fleet status.
func (d *Deployer) Start(ctx context.Context, selectors []string) error {
	state, inv, err := d.lifecycleTargets(selectors)
	if err != nil {
		return err
	}
	if !inv.created {
		return fmt.Errorf("fleet start requires a complete deployment/network.env; run l1 create first")
	}

	converge := make([]nodeDeployment, 0, len(state.selected))
	for _, target := range state.selected {
		active, runtimeID, err := d.probeRuntimeIdentity(ctx, inv, target)
		if err != nil {
			return err
		}
		needsRestart, note := planStart(target, active, runtimeID)
		if note != "" {
			fmt.Fprintln(d.out, note)
		}
		if needsRestart {
			converge = append(converge, target)
		}
	}
	if len(converge) == 0 {
		fmt.Fprintln(d.out, "every selected node already serves its assigned identity; nothing to do")
		return nil
	}

	bringingUp := make([]int, 0, len(converge))
	for _, target := range converge {
		bringingUp = append(bringingUp, target.node.Number)
	}
	up, err := d.intendedUp(ctx, inv, bringingUp, nil)
	if err != nil {
		return err
	}
	rendered, cleanup, err := d.renderConfigs(inv, converge, up)
	if err != nil {
		return err
	}
	defer cleanup()
	state.selected = rendered
	if err := d.runPhases(ctx, state, []lifecyclePhase{
		{"stop", d.stop},
		{"identity", d.installIdentity},
		{"config", d.installConfig},
		{"start", d.enableAndStart},
	}); err != nil {
		return err
	}
	fmt.Fprintf(d.out, "started %d L1 node(s); not waiting for readiness (needs 75%% of stake connected)\n", len(state.selected))
	fmt.Fprintln(d.out, "watch convergence with: fleet status")
	return d.refreshAddressBook(ctx, inv, bringingUp, nil)
}

// planStart is the pure half of Start: given what a machine is actually doing, it
// decides whether that machine must be restarted, plus the line to print. Kept
// separate so the idempotence rule is testable without a fleet.
//
// A running node serving its assigned identity needs NOTHING, and saying so is
// the whole contract: repeating a start must be a no-op.
func planStart(target nodeDeployment, active bool, runtimeNodeID string) (bool, string) {
	switch {
	case active && runtimeNodeID == target.identity.NodeID:
		return false, fmt.Sprintf("node %d already serves identity %s; leaving it alone",
			target.node.Number, target.identity.Identity)
	case active && runtimeNodeID == "":
		return true, fmt.Sprintf("node %d is running but its API does not answer; restarting it with identity %s",
			target.node.Number, target.identity.Identity)
	case active:
		return true, fmt.Sprintf("node %d runs %s but placement assigns identity %s; restarting it",
			target.node.Number, runtimeNodeID, target.identity.Identity)
	default:
		return true, ""
	}
}

// probeRuntimeIdentity reports whether a node's service is active and which identity it
// is actually serving. An active service whose API does not answer yields
// ("", true) rather than an error: that node needs fixing, which is precisely
// what the caller is about to do, so failing the command would be perverse.
func (d *Deployer) probeRuntimeIdentity(ctx context.Context, inv inventory, target nodeDeployment) (bool, string, error) {
	output, err := d.runSSHOutput(ctx, deployment{environment: inv.environment}, target,
		layoutFor(inv.environment).isActiveProbe(target))
	if err != nil {
		return false, "", fmt.Errorf("node %d (%s) service state: %w", target.node.Number, target.node.Host, err)
	}
	if strings.TrimSpace(string(output)) != "active" {
		return false, "", nil
	}
	runtimeID, err := d.runtimeNodeID(ctx, target.node.Host, target.httpPort)
	if err != nil {
		return true, "", nil
	}
	return true, runtimeID, nil
}

// refreshAddressBook re-renders node.json for every reachable L1 machine and
// pushes it, in parallel. It runs AFTER a lifecycle command has changed the
// up-set, so the book each machine holds names the machines that are meant to be
// running now.
//
// Pushing to already-running machines is the point, not an optimization: the
// systemd unit is Restart=on-failure, so a crash reloads node.json without the
// kit ever being involved. A machine left holding a book full of dead peers would
// come back from a crash unable to state-sync, with nobody having typed anything.
// Best effort per machine: a host that cannot be reached is one whose next start
// re-renders anyway.
func (d *Deployer) refreshAddressBook(ctx context.Context, inv inventory, bringingUp, takingDown []int) error {
	up, err := d.intendedUp(ctx, inv, bringingUp, takingDown)
	if err != nil {
		return err
	}
	all, err := d.placementTargets(inv, inv.l1Nodes())
	if err != nil {
		return err
	}
	rendered, cleanup, err := d.renderConfigs(inv, all.selected, up)
	if err != nil {
		return err
	}
	defer cleanup()
	all.selected = rendered
	listed := 0
	for _, node := range inv.l1Nodes() {
		if up[node.Number] {
			listed++
		}
	}
	fmt.Fprintf(d.out, "address book: %d of %d L1 machines intended up\n", listed, len(inv.l1Nodes()))
	if err := d.phaseParallel(ctx, all, "address book", d.installConfig); err != nil {
		fmt.Fprintf(d.out, "address book: some machines were not updated: %v\n", err)
	}
	return nil
}

// Stop disables and gracefully stops the selected services. Databases, logs,
// installed artifacts, and the current remote key are all preserved.
func (d *Deployer) Stop(ctx context.Context, selectors []string) error {
	state, inv, err := d.lifecycleTargets(selectors)
	if err != nil {
		return err
	}
	if err := d.runPhases(ctx, state, []lifecyclePhase{{"stop", d.disableAndStop}}); err != nil {
		return err
	}
	fmt.Fprintf(d.out, "stopped %d L1 node(s)\n", len(state.selected))
	return d.refreshAddressBook(ctx, inv, nil, selectedNumbers(state))
}

// Destroy simulates abrupt machine loss on the selected L1 machines and then
// removes only this L1's chain data. It is local to the L1 and unrelated to
// l1 destroy, which reclaims P-chain balances.
//
// Node numbers are REQUIRED. There is deliberately no bare form meaning "every
// node", because the blast radius of this verb is data: an operator who means to
// lose the whole fleet must name every machine, which is exactly the pause a
// destructive command should impose. Same reasoning as removing dc= selectors.
func (d *Deployer) Destroy(ctx context.Context, selectors []string) error {
	if len(selectors) == 0 {
		return fmt.Errorf("fleet destroy requires explicit node numbers: it deletes this L1's chain data and has no bare form meaning \"every node\"; name the machines you intend to lose%s", d.everyL1NodeHint())
	}
	state, inv, err := d.lifecycleTargets(selectors)
	if err != nil {
		return err
	}
	if !inv.created || state.chainID == ids.Empty {
		return fmt.Errorf("fleet destroy requires CHAIN_ID in deployment/network.env; there is no L1 chain data to remove")
	}
	if err := d.destroyPhases(ctx, state); err != nil {
		return err
	}
	fmt.Fprintf(d.out, "destroyed L1 chain data on %d node(s)\n", len(state.selected))
	return d.refreshAddressBook(ctx, inv, nil, selectedNumbers(state))
}

// everyL1NodeHint spells out the whole-fleet selector so an operator who really
// means every machine can copy it, best effort: the hint is a convenience and
// must never turn a missing-selector error into an inventory error.
func (d *Deployer) everyL1NodeHint() string {
	inv, err := d.inventory()
	if err != nil {
		return ""
	}
	nodes := inv.l1Nodes()
	if len(nodes) == 0 {
		return ""
	}
	numbers := make([]string, 0, len(nodes))
	for _, node := range nodes {
		numbers = append(numbers, strconv.Itoa(node.Number))
	}
	return fmt.Sprintf(".\nTo destroy the whole L1 fleet, say so explicitly:\n  fleet destroy %s", strings.Join(numbers, " "))
}

// destroyPhases keeps the safety gate structural: the kill phase must succeed
// on EVERY selected node before the delete phase touches ANY node.
func (d *Deployer) destroyPhases(ctx context.Context, state deployment) error {
	return d.runPhases(ctx, state, []lifecyclePhase{
		{"destroy kill", d.killService},
		{"destroy chain data", d.removeChainData},
	})
}

// enableAndStart mirrors disableAndStop: systemd is the only up/down intent, so
// start restores boot intent that a previous stop or destroy removed.
func (d *Deployer) enableAndStart(ctx context.Context, deployment deployment, node nodeDeployment) error {
	return d.runSSH(ctx, deployment, node, layoutFor(deployment.environment).enableAndStartCommand(node))
}

func (d *Deployer) disableAndStop(ctx context.Context, deployment deployment, node nodeDeployment) error {
	return d.runSSH(ctx, deployment, node, layoutFor(deployment.environment).disableAndStopCommand(node))
}

// killService prevents any restart, SIGKILLs the process, and proves the node
// is gone. Every step is best effort; the single hard gate is the final
// inactivity check, so an already-stopped node is still destroyable.
func (d *Deployer) killService(ctx context.Context, deployment deployment, node nodeDeployment) error {
	return d.runSSH(ctx, deployment, node, layoutFor(deployment.environment).killCommand(node))
}

func (d *Deployer) removeChainData(ctx context.Context, deployment deployment, node nodeDeployment) error {
	// AvalancheGo derives chain-data-dir from data-dir and gives every chain its
	// own <chain-id> subdirectory. Only this L1's directory is removed, so the
	// P-chain database, identity, logs, configuration, and binaries survive.
	nodeChainID, _ := deployment.l1For(node.node.Role)
	l := layoutFor(deployment.environment)
	return d.runSSH(ctx, deployment, node, l.sudo(fmt.Sprintf(
		"rm -rf %s/%d/chainData/%s",
		l.data, node.node.Number, nodeChainID)))
}

// selectedNumbers lists the machine numbers a lifecycle command acted on.
func selectedNumbers(state deployment) []int {
	numbers := make([]int, 0, len(state.selected))
	for _, target := range state.selected {
		numbers = append(numbers, target.node.Number)
	}
	return numbers
}
