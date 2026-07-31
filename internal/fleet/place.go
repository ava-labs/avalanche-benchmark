package fleet

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/placement"
)

// Place moves a validator identity onto a validator machine and makes the fleet
// serve it. It is the ONLY placement verb: reconcile, write, reconcile.
//
//	before  converge any pre-existing drift, so this move is the only change in
//	        flight. Silent when the fleet already matches placement.json.
//	write   update placement.json atomically.
//	after   restart exactly the nodes whose runtime identity is now wrong.
//
// The before phase is the point of the design. Without it, a place on a fleet
// that had not been applied yet would silently bundle someone else's pending
// move into this one, so a single restart would carry two identity changes and
// a failure would leave neither attributable. Converging first makes every
// place a single, isolated transition.
//
// Placement is a bijection, so moving identity X onto machine N necessarily
// swaps: whatever machine N held goes to the machine X came from.
//
// ONE move per invocation, deliberately. A batched place would restart every
// affected machine in the same pass, taking several boxes offline at once, which
// is exactly the disruption an identity failover exists to avoid. Moving a whole
// quorum is therefore a sequence of single places, which is safe because the
// after phase does not wait for readiness.
func (d *Deployer) Place(ctx context.Context, identityLetter string, node int) error {
	if err := d.reconcilePlacement(ctx); err != nil {
		return fmt.Errorf("converging the fleet before placing: %w", err)
	}

	inv, err := d.inventory()
	if err != nil {
		return err
	}
	next, moves, err := planPlace(inv, identityLetter, node)
	if err != nil {
		return err
	}
	if err := placement.Validate(next, inv.public, inv.nodes); err != nil {
		return fmt.Errorf("refusing to write an invalid placement: %w", err)
	}

	current := inv.placement
	if err := placement.Save(placement.Path(d.root), next); err != nil {
		return err
	}
	for _, move := range moves {
		fmt.Fprintln(d.out, move)
	}

	// Test the placement, not len(moves): a no-op place still returns one move
	// (the "already on node N" note), so counting moves would restart nodes after
	// a placement that changed nothing.
	if maps.Equal(next, current) {
		return nil
	}
	return d.reconcilePlacement(ctx)
}

// reconcilePlacement drives the runtime to placement.json: it restarts exactly
// the machines that are not already serving their assigned identity.
//
// Silent when there is nothing to do, in either phase. place runs it twice and
// the converged case is the common one, so announcing a check that found
// nothing would just bury the output the operator asked for.
func (d *Deployer) reconcilePlacement(ctx context.Context) error {
	inv, err := d.inventory()
	if err != nil {
		return err
	}
	if !inv.created {
		return fmt.Errorf("place requires a complete deployment/network.env; run l1 create first")
	}

	l1 := inv.l1Nodes()
	probes := make(map[int]placementProbe, len(l1))
	for _, node := range l1 {
		probe, err := d.probePlacement(ctx, inv, node)
		if err != nil {
			return err
		}
		probes[node.Number] = probe
	}

	restart, notes, err := planApply(inv, l1, probes)
	if err != nil {
		return err
	}
	if len(restart) == 0 {
		return nil
	}
	for _, note := range notes {
		fmt.Fprintln(d.out, note)
	}

	// EVERY L1 machine gets the new address book, not just the ones that
	// swapped. state-sync-ips/ids pairs each peer's address with the NodeID it
	// runs, and node.go hands that list to Net.ManuallyTrack, so it is the only
	// way these nodes learn where their siblings are: they run
	// partial-sync-primary-network and the sole bootstrapper does not track the
	// L1, so nothing gossips their addresses. One moved identity therefore
	// invalidates every other machine's copy. Machines that are deliberately
	// down are skipped by placementTargets' caller set below but still receive
	// config here, so their next start cannot come up with a pre-swap view.
	allTargets, err := d.placementTargets(inv, l1)
	if err != nil {
		return err
	}
	up, err := d.intendedUp(ctx, inv, nil, nil)
	if err != nil {
		return err
	}
	renderedAll, cleanup, err := d.renderConfigs(inv, allTargets.selected, up)
	if err != nil {
		return err
	}
	defer cleanup()
	allTargets.selected = renderedAll
	if err := d.phase(ctx, allTargets, "config", d.installConfig); err != nil {
		return err
	}

	prepared, err := d.placementTargets(inv, restart)
	if err != nil {
		return err
	}
	// The identity phase makes a rerun converge after an interrupted place:
	// a restart alone would just reload the same stale key from disk.
	//
	// Deliberately NO readiness phase. A restarted node finishes bootstrapping
	// only once 75% of stake is connected, so waiting here deadlocks the exact
	// case place exists for: relocating a quorum one identity at a time passes
	// through intermediate placements where the surviving side is below that
	// gate, and move N would block forever on stake that only arrives with move
	// N+1. Chaining places cannot escape it either, because the next command's
	// BEFORE reconcile would wait on the same un-ready node. Like start, place
	// is a state change; observe convergence with fleet status.
	for _, phase := range []struct {
		name   string
		action func(context.Context, deployment, nodeDeployment) error
	}{
		{"identity", d.installIdentity},
		{"stop", d.stop},
		{"start", d.start},
	} {
		if err := d.phase(ctx, prepared, phase.name, phase.action); err != nil {
			return err
		}
	}
	fmt.Fprintf(d.out, "applied placement to %d node(s); not waiting for readiness (needs 75%% of stake connected)\n", len(restart))
	fmt.Fprintln(d.out, "watch convergence with: fleet status")
	return nil
}

// planPlace is the pure half of Place: it validates the request and returns
// the next placement plus a human description of what moved.
func planPlace(inv inventory, identityLetter string, node int) (placement.Placement, []string, error) {
	if _, err := identity.Index(identityLetter); err != nil {
		return nil, nil, err
	}
	generated, known := inv.identityByLetter[identityLetter]
	if !known {
		return nil, nil, fmt.Errorf("unknown identity %q; deployment/public.json has no such identity", identityLetter)
	}
	if generated.Role != config.RoleValidator {
		return nil, nil, fmt.Errorf("identity %q is a %s identity and is never movable", identityLetter, generated.Role)
	}

	machine, found := config.Node{}, false
	for _, candidate := range inv.nodes {
		if candidate.Number == node {
			machine, found = candidate, true
			break
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("unknown node %d; nodes.ini has no such machine", node)
	}
	if machine.Role != config.RoleValidator {
		return nil, nil, fmt.Errorf("node %d is a %s machine; identities move between validator machines only", node, machine.Role)
	}

	source, placed := inv.placement.NodeOf(identityLetter)
	if !placed {
		return nil, nil, fmt.Errorf("placement does not assign identity %q to any machine", identityLetter)
	}
	displaced, assigned := inv.placement[node]
	if !assigned {
		return nil, nil, fmt.Errorf("placement has no identity for node %d", node)
	}

	next := make(placement.Placement, len(inv.placement))
	for number, letter := range inv.placement {
		next[number] = letter
	}
	if source == node {
		return next, []string{fmt.Sprintf("identity %s is already on node %d; nothing moved", identityLetter, node)}, nil
	}
	next[node] = identityLetter
	next[source] = displaced
	moves := []string{
		fmt.Sprintf("identity %s: node %d -> node %d", identityLetter, source, node),
		fmt.Sprintf("identity %s: node %d -> node %d", displaced, node, source),
	}
	return next, moves, nil
}

// placementProbe is what reconcilePlacement observes about one machine.
type placementProbe struct {
	active  bool
	enabled bool
	// nodeID is the runtime identity reported by the node API. It is only
	// populated for an active service.
	nodeID string
}

// planApply is the pure half of reconcilePlacement: given what each machine is
// currently doing, decide which machines must be restarted.
func planApply(inv inventory, nodes []config.Node, probes map[int]placementProbe) ([]config.Node, []string, error) {
	var restart []config.Node
	var notes []string
	for _, node := range nodes {
		probe, probed := probes[node.Number]
		if !probed {
			return nil, nil, fmt.Errorf("node %d (%s) was not probed", node.Number, node.Host)
		}
		assigned, err := inv.assigned(node)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case probe.active && probe.nodeID == "":
			return nil, nil, fmt.Errorf("node %d (%s) is active but reported no runtime NodeID", node.Number, node.Host)
		case probe.active && probe.nodeID == assigned.NodeID:
			// Already serving its assigned identity. Untouched.
		case probe.active:
			restart = append(restart, node)
			notes = append(notes, fmt.Sprintf(
				"node %d runs %s but placement assigns identity %s (%s)",
				node.Number, probe.nodeID, assigned.Identity, assigned.NodeID))
		case probe.enabled:
			// Enabled but not running means an interrupted run, not a drill:
			// fleet stop disables the unit. Bringing it back converges.
			restart = append(restart, node)
			notes = append(notes, fmt.Sprintf(
				"node %d is enabled but not running; starting it with identity %s",
				node.Number, assigned.Identity))
		default:
			notes = append(notes, fmt.Sprintf("node %d is deliberately down; leaving it down", node.Number))
		}
	}
	return restart, notes, nil
}

func (d *Deployer) probePlacement(ctx context.Context, inv inventory, node config.Node) (placementProbe, error) {
	target, err := inv.target(node)
	if err != nil {
		return placementProbe{}, err
	}
	unit := serviceName(target)
	output, err := d.runSSHOutput(ctx, deployment{environment: inv.environment}, target, fmt.Sprintf(
		"printf '%%s %%s' \"$(sudo systemctl is-active %[1]s 2>/dev/null)\" \"$(sudo systemctl is-enabled %[1]s 2>/dev/null)\"",
		unit))
	if err != nil {
		return placementProbe{}, fmt.Errorf("node %d (%s) service state: %w", node.Number, node.Host, err)
	}

	fields := strings.Fields(string(output))
	probe := placementProbe{}
	if len(fields) > 0 && fields[0] == "active" {
		probe.active = true
	}
	if len(fields) > 1 && fields[1] == "enabled" {
		probe.enabled = true
	}
	if !probe.active {
		return probe, nil
	}
	probe.nodeID, err = d.runtimeNodeID(ctx, node.Host, target.httpPort)
	if err != nil {
		return placementProbe{}, fmt.Errorf("node %d (%s) service is active but its API did not answer: %w", node.Number, node.Host, err)
	}
	return probe, nil
}

// runtimeNodeID asks a running node which identity it is actually serving.
// Shared with status so a placement check and a status report can never
// disagree about the same machine.
func (d *Deployer) runtimeNodeID(ctx context.Context, host string, port int) (string, error) {
	var runtime struct {
		NodeID string `json:"nodeID"`
	}
	url := fmt.Sprintf("http://%s:%d/ext/info", host, port)
	if err := d.statusRPC(ctx, url, "info.getNodeID", struct{}{}, &runtime); err != nil {
		return "", err
	}
	if runtime.NodeID == "" {
		return "", fmt.Errorf("info.getNodeID returned no nodeID")
	}
	return runtime.NodeID, nil
}

// placementTargets builds the deployment view for a set of machines and fails
// loudly if control does not hold complete key material for any of them.
func (d *Deployer) placementTargets(inv inventory, nodes []config.Node) (deployment, error) {
	prepared := deployment{
		environment: inv.environment,
		chainID:     inv.chainID,
		subnetID:    inv.subnetID,
		selected:    make([]nodeDeployment, 0, len(nodes)),
	}
	for _, node := range nodes {
		target, err := inv.target(node)
		if err != nil {
			return deployment{}, err
		}
		if err := validateIdentityFiles(d.root, target.identity); err != nil {
			return deployment{}, err
		}
		prepared.selected = append(prepared.selected, target)
	}
	return prepared, nil
}
