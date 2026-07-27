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
			"public P-chain API is missing the %s validator set (local height %d, upstream height %d)",
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
			"P-chain node is synchronized at height %d and the public API shows both validator sets\n",
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
// height, then the validator view. The order matters: a local height read after
// the upstream sample can only understate progress, never overstate it.
//
// The local node runs with --p-chain-follow-only and therefore rejects every
// platform.* call forever, so its height is observed rather than queried and
// the validator sets come from the public API.
func (d *Deployer) freezeGateState(ctx context.Context, prepared deployment) (freezeGate, error) {
	network, err := config.LoadNetworkEnvironment(filepath.Join(d.root, ".env"))
	if err != nil {
		return freezeGate{}, err
	}
	public := platformvm.NewClient(network.PChainAPI)
	upstreamHeight, err := public.GetHeight(ctx)
	if err != nil {
		return freezeGate{}, fmt.Errorf("read upstream P-chain height from %s: %w", network.PChainAPI, err)
	}

	observation, err := d.observePChain(ctx, prepared, prepared.pchain)
	if err != nil {
		return freezeGate{}, fmt.Errorf(
			"observe P-chain node %d (%s): %w",
			prepared.pchain.node.Number, prepared.pchain.node.Host, err,
		)
	}
	if !observation.heightOK {
		return freezeGate{}, fmt.Errorf(
			"P-chain node %d (%s): local height is not observable in %s",
			prepared.pchain.node.Number, prepared.pchain.node.Host, pchainLogPath(prepared.pchain),
		)
	}

	manager, err := public.GetCurrentValidators(ctx, prepared.managerSubnetID, nil)
	if err != nil {
		return freezeGate{}, fmt.Errorf("read management validator set from %s: %w", network.PChainAPI, err)
	}
	main, err := public.GetCurrentValidators(ctx, prepared.subnetID, nil)
	if err != nil {
		return freezeGate{}, fmt.Errorf("read main validator set from %s: %w", network.PChainAPI, err)
	}

	return freezeGate{
		upstreamHeight: upstreamHeight,
		localHeight:    observation.height,
		managerVisible: containsValidators(manager, prepared.expectedManager),
		mainVisible:    containsValidators(main, prepared.expectedMain),
	}, nil
}
