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
// A selector is either a node number or dc=<tag>; multiple selectors are a
// union; no selector means every candidate. A selector that matches nothing is
// a loud error, because silently doing less than asked is how a drill lies.
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
		return nil, fmt.Errorf("selectors %q contain no node number or dc=<tag>", strings.Join(selectors, " "))
	}

	chosen := make(map[int]struct{}, len(candidates))
	for _, selector := range expanded {
		matched := 0
		if tag, isDC := strings.CutPrefix(selector, "dc="); isDC {
			if tag == "" {
				return nil, fmt.Errorf("selector %q has an empty dc tag", selector)
			}
			for _, node := range candidates {
				if node.DC == tag {
					chosen[node.Number] = struct{}{}
					matched++
				}
			}
		} else {
			number, err := strconv.Atoi(selector)
			if err != nil {
				return nil, fmt.Errorf("selector %q must be a node number or dc=<tag>", selector)
			}
			for _, node := range candidates {
				if node.Number == number {
					chosen[node.Number] = struct{}{}
					matched++
				}
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
		environment: inv.environment,
		chainID:     inv.chainID,
		subnetID:    inv.subnetID,
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

// Start converges the selected L1 machines: stop everything, re-push the
// currently assigned identity, then start everything. The identity push is not
// an optimization opportunity: placement is authoritative and a stale remote
// key must never survive a restart.
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
	if err := d.runPhases(ctx, state, []lifecyclePhase{
		{"stop", d.stop},
		{"identity", d.installIdentity},
		{"start", d.enableAndStart},
	}); err != nil {
		return err
	}
	fmt.Fprintf(d.out, "started %d L1 node(s); not waiting for readiness (needs 75%% of stake connected)\n", len(state.selected))
	fmt.Fprintln(d.out, "watch convergence with: fleet status")
	return nil
}

// Stop disables and gracefully stops the selected services. Databases, logs,
// installed artifacts, and the current remote key are all preserved.
func (d *Deployer) Stop(ctx context.Context, selectors []string) error {
	state, _, err := d.lifecycleTargets(selectors)
	if err != nil {
		return err
	}
	if err := d.runPhases(ctx, state, []lifecyclePhase{{"stop", d.disableAndStop}}); err != nil {
		return err
	}
	fmt.Fprintf(d.out, "stopped %d L1 node(s)\n", len(state.selected))
	return nil
}

// Destroy simulates abrupt machine loss on the selected L1 machines and then
// removes only this L1's chain data. It is local to the L1 and unrelated to
// l1 destroy, which reclaims P-chain balances.
func (d *Deployer) Destroy(ctx context.Context, selectors []string) error {
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
	return nil
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
	unit := serviceName(node)
	return d.runSSH(ctx, deployment, node, fmt.Sprintf(
		"sudo systemctl enable %[1]s && sudo systemctl start %[1]s", unit))
}

func (d *Deployer) disableAndStop(ctx context.Context, deployment deployment, node nodeDeployment) error {
	unit := serviceName(node)
	return d.runSSH(ctx, deployment, node, fmt.Sprintf(
		"if sudo systemctl cat %[1]s >/dev/null 2>&1; then "+
			"sudo systemctl disable %[1]s && sudo systemctl stop %[1]s && "+
			"test \"$(sudo systemctl is-active %[1]s)\" = inactive; fi",
		unit))
}

// killService prevents a systemd restart, SIGKILLs the process, and proves the
// unit is inactive. The trailing stop cancels the pending auto-restart that
// Restart=on-failure schedules for a killed process.
func (d *Deployer) killService(ctx context.Context, deployment deployment, node nodeDeployment) error {
	unit := serviceName(node)
	// Every step is best effort; the single hard gate is the final inactivity
	// check, so an already-stopped or already-failed unit is still destroyable.
	return d.runSSH(ctx, deployment, node, fmt.Sprintf(
		"if sudo systemctl cat %[1]s >/dev/null 2>&1; then "+
			"sudo systemctl disable %[1]s || true; "+
			"sudo systemctl kill -s SIGKILL %[1]s || true; "+
			"sudo systemctl stop %[1]s || true; "+
			"sudo systemctl reset-failed %[1]s >/dev/null 2>&1 || true; "+
			"test \"$(sudo systemctl is-active %[1]s)\" = inactive; fi",
		unit))
}

func (d *Deployer) removeChainData(ctx context.Context, deployment deployment, node nodeDeployment) error {
	// AvalancheGo derives chain-data-dir from data-dir and gives every chain its
	// own <chain-id> subdirectory. Only this L1's directory is removed, so the
	// P-chain database, identity, logs, configuration, and binaries survive.
	return d.runSSH(ctx, deployment, node, fmt.Sprintf(
		"sudo rm -rf %s/%d/chainData/%s",
		remoteDataDir, node.node.Number, deployment.chainID))
}
