package fleet

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanchego/vms/platformvm"
)

// freezeGate is the ready-to-freeze decision `fleet status` reports, isolated
// from every API call so it can be tested directly. Heights are carried so a
// refusal names the exact numbers instead of "not ready".
type freezeGate struct {
	upstreamHeight uint64
	localHeight    uint64
	managerVisible bool
	mainVisible    bool
}

// check returns nil when the node may be frozen, or the precise reason it may
// not. Synchronization is checked first: an unsynchronized node's validator
// view is not evidence of anything.
func (g freezeGate) check() error {
	if g.localHeight < g.upstreamHeight {
		return fmt.Errorf(
			"P-chain node is not synchronized: local height %d, upstream height %d, lag %d",
			g.localHeight, g.upstreamHeight, g.upstreamHeight-g.localHeight,
		)
	}
	var missing []string
	if !g.managerVisible {
		missing = append(missing, "management")
	}
	if !g.mainVisible {
		missing = append(missing, "main")
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"local P-chain view is missing the %s validator set (local height %d, upstream height %d)",
			strings.Join(missing, " and "), g.localHeight, g.upstreamHeight,
		)
	}
	return nil
}

// FreezePChain gates on synchronization and validator-set visibility, then
// reconciles the P-chain node into frozen mode. It touches no validator or RPC
// machine and never reseeds, wipes, or restores the P-chain database: freezing
// a synchronized node is a mode change, not a data operation.
func (d *Deployer) FreezePChain(ctx context.Context) error {
	prepared, cleanup, err := d.prepare(frozenMode, false)
	if err != nil {
		return err
	}
	defer cleanup()

	mode, err := d.probePChainMode(ctx, prepared, prepared.pchain)
	if err != nil {
		return err
	}
	if mode == frozenMode {
		// Already frozen: the gate is meaningless because a frozen node does
		// not follow the upstream. Reconcile anyway so the pass converges.
		fmt.Fprintln(d.out, "P-chain node is already frozen; reconciling frozen configuration")
	} else {
		gate, err := d.freezeGateState(ctx, prepared)
		if err != nil {
			return err
		}
		if err := gate.check(); err != nil {
			return fmt.Errorf("refusing to freeze: %w", err)
		}
		fmt.Fprintf(
			d.out,
			"P-chain node is synchronized at height %d and sees both validator sets\n",
			gate.localHeight,
		)
	}

	if err := d.reconcilePChain(ctx, prepared, false); err != nil {
		return err
	}
	fmt.Fprintln(d.out, "P-chain node is running in frozen mode")
	return nil
}

// freezeGateState samples the upstream height, then immediately the local
// height, then the local validator view. The order matters: a local height read
// after the upstream sample can only understate progress, never overstate it.
func (d *Deployer) freezeGateState(ctx context.Context, prepared deployment) (freezeGate, error) {
	network, err := config.LoadNetworkEnvironment(filepath.Join(d.root, ".env"))
	if err != nil {
		return freezeGate{}, err
	}
	upstream := platformvm.NewClient(network.PChainAPI)
	upstreamHeight, err := upstream.GetHeight(ctx)
	if err != nil {
		return freezeGate{}, fmt.Errorf("read upstream P-chain height from %s: %w", network.PChainAPI, err)
	}

	uri := fmt.Sprintf("http://%s:%d", prepared.pchain.node.Host, prepared.pchain.httpPort)
	local := platformvm.NewClient(uri)
	localHeight, err := local.GetHeight(ctx)
	if err != nil {
		return freezeGate{}, fmt.Errorf("read local P-chain height from %s: %w", uri, err)
	}

	manager, err := local.GetCurrentValidators(ctx, prepared.managerSubnetID, nil)
	if err != nil {
		return freezeGate{}, fmt.Errorf("read management validator set from %s: %w", uri, err)
	}
	main, err := local.GetCurrentValidators(ctx, prepared.subnetID, nil)
	if err != nil {
		return freezeGate{}, fmt.Errorf("read main validator set from %s: %w", uri, err)
	}

	return freezeGate{
		upstreamHeight: upstreamHeight,
		localHeight:    localHeight,
		managerVisible: containsValidators(manager, prepared.expectedManager),
		mainVisible:    containsValidators(main, prepared.expectedMain),
	}, nil
}
