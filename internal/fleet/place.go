package fleet

import (
	"context"
	"fmt"
	"strings"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/placement"
)

// Place moves a validator identity onto a validator machine and pushes the
// resulting assignment to disk on every inventory machine. It is a
// control-plane move plus key distribution: it never stops or starts anything.
//
// Placement is a bijection, so moving identity X onto machine N necessarily
// swaps: whatever machine N held goes to the machine X came from.
func (d *Deployer) Place(ctx context.Context, identityLetter string, node int) error {
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

	// Control-side truth is written before any remote work, so a crash during
	// the push leaves placement.json correct and a rerun converges.
	if err := placement.Save(placement.Path(d.root), next); err != nil {
		return err
	}
	inv.placement = next
	for _, move := range moves {
		fmt.Fprintln(d.out, move)
	}

	// Every inventory machine is rewritten, not just the two that changed, so
	// disk state is deterministic and a rerun is a full reconciliation.
	// Each machine receives only the one identity assigned to it.
	prepared, err := d.placementTargets(inv, inv.nodes)
	if err != nil {
		return err
	}
	if err := d.phase(ctx, prepared, "identity", d.installIdentity); err != nil {
		return err
	}
	fmt.Fprintf(d.out, "pushed assigned keys to %d machine(s); no service was restarted\n", len(prepared.selected))
	// place only rewrites disk. Until the affected nodes restart they keep
	// serving their OLD identity, so the fleet disagrees with placement.json and
	// nothing has actually moved. Name the next command explicitly: this is the
	// one step an operator forgets, and the symptom (no change at all) looks
	// exactly like place having silently failed. A no-op place produces no moves
	// and needs no restart, so stay quiet there.
	if len(moves) > 0 {
		fmt.Fprintln(d.out, "placement.json now disagrees with the running fleet; apply it with:")
		fmt.Fprintln(d.out, "  fleet apply-placement")
	}
	return nil
}

// ApplyPlacement reconciles the runtime to placement.json: it restarts exactly
// the machines that are not already serving their assigned identity.
func (d *Deployer) ApplyPlacement(ctx context.Context) error {
	inv, err := d.inventory()
	if err != nil {
		return err
	}
	if !inv.created {
		return fmt.Errorf("apply-placement requires a complete deployment/network.env; run l1 create first")
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
	for _, note := range notes {
		fmt.Fprintln(d.out, note)
	}
	if len(restart) == 0 {
		fmt.Fprintln(d.out, "every running node already serves its assigned identity; nothing to apply")
		return nil
	}

	prepared, err := d.placementTargets(inv, restart)
	if err != nil {
		return err
	}
	// The identity phase makes a rerun converge after an interrupted place:
	// a restart alone would just reload the same stale key from disk.
	for _, phase := range []struct {
		name   string
		action func(context.Context, deployment, nodeDeployment) error
	}{
		{"identity", d.installIdentity},
		{"stop", d.stop},
		{"start", d.start},
		{"readiness", d.waitL1Ready},
	} {
		if err := d.phase(ctx, prepared, phase.name, phase.action); err != nil {
			return err
		}
	}
	fmt.Fprintf(d.out, "applied placement to %d node(s)\n", len(restart))
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
		return next, []string{fmt.Sprintf("identity %s is already on node %d; nothing moved, keys rewritten", identityLetter, node)}, nil
	}
	next[node] = identityLetter
	next[source] = displaced
	moves := []string{
		fmt.Sprintf("identity %s: node %d -> node %d", identityLetter, source, node),
		fmt.Sprintf("identity %s: node %d -> node %d", displaced, node, source),
	}
	return next, moves, nil
}

// placementProbe is what apply-placement observes about one machine.
type placementProbe struct {
	active  bool
	enabled bool
	// nodeID is the runtime identity reported by the node API. It is only
	// populated for an active service.
	nodeID string
}

// planApply is the pure half of ApplyPlacement: given what each machine is
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
