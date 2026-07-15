package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	warpmessage "github.com/ava-labs/avalanchego/vms/platformvm/warp/message"
	pwallet "github.com/ava-labs/avalanchego/wallet/chain/p/wallet"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/vset"
)

// setWeight moves ONE validator's weight: sign against the signing set (the
// committee under the committee model, the L1's own set when self-managed),
// submit the SetL1ValidatorWeightTx, done.
func setWeight(ctx context.Context, cfg config, node string, weight uint64) {
	pc := platformvm.NewClient(cfg.net.API)
	vs, err := vset.Fetch(ctx, pc, cfg.subnetID, 1)
	if err != nil {
		fatalf("%v", err)
	}
	v := resolveValidator(cfg, vs, node)
	signers, err := loadSigners(cfg.stakingDir, cfg.signTier())
	if err != nil {
		fatalf("%v", err)
	}
	signSet, err := signingSet(ctx, pc, cfg, vs)
	if err != nil {
		fatalf("%v", err)
	}
	if err := submitWeight(cfg, makeWallet(ctx, cfg), signers, signSet, v, weight); err != nil {
		fatalf("%v (safe to re-run: each run refetches the set and re-signs)", err)
	}
}

// signingSet is the canonical warp set the P-chain verifies weight messages
// against: the MANAGER subnet's committee under the committee model, the main
// L1's own set (the caller's freshly fetched mainVS) when self-managed.
func signingSet(ctx context.Context, pc *platformvm.Client, cfg config, mainVS []vset.Validator) (validators.WarpSet, error) {
	if cfg.committee() {
		cvs, err := vset.Fetch(ctx, pc, cfg.managerSubnetID, 1)
		if err != nil {
			return validators.WarpSet{}, fmt.Errorf("fetch committee (manager subnet %s): %w", cfg.managerSubnetID, err)
		}
		return vset.WarpSet(cvs)
	}
	return vset.WarpSet(mainVS)
}

// submitWeight signs and submits one SetL1ValidatorWeightTx moving v to
// weight. signSet is the canonical set the signatures are verified against;
// cfg.signChainID() is the warp sourceChainID.
func submitWeight(cfg config, wallet pwallet.Wallet, signers []bls.Signer, signSet validators.WarpSet, v vset.Validator, weight uint64) error {
	payload, err := warpmessage.NewL1ValidatorWeight(v.ValidationID, v.MinNonce, weight)
	if err != nil {
		return fmt.Errorf("build L1ValidatorWeight: %w", err)
	}
	unsigned, err := addressedCall(cfg.net.NetworkID, cfg.signChainID(), cfg.managerAddr, payload.Bytes())
	if err != nil {
		return fmt.Errorf("build warp message: %w", err)
	}
	signed, err := signAndAggregate(unsigned, signSet, signers)
	if err != nil {
		return err
	}

	action := fmt.Sprintf("weight %d -> %d", v.Weight, weight)
	if weight == 0 {
		action = fmt.Sprintf("remove (weight %d -> 0)", v.Weight)
	}
	fmt.Printf("%s %s (%s), nonce %d: submitting SetL1ValidatorWeightTx...\n", v.NodeID, v.ValidationID, action, v.MinNonce)
	tx, err := wallet.IssueSetL1ValidatorWeightTx(signed.Bytes())
	if err != nil {
		return fmt.Errorf("SetL1ValidatorWeightTx: %w", err)
	}
	fmt.Printf("accepted: %s\n", tx.ID())
	return nil
}

// target is one desired name=weight assignment from `apply --weights`.
type target struct {
	name   string
	weight uint64
}

// parseTargets parses the --weights CSV, preserving order and rejecting
// duplicates.
func parseTargets(csv string) ([]target, error) {
	var out []target
	seen := map[string]bool{}
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, w, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("bad --weights entry %q (want name=weight)", part)
		}
		weight, err := strconv.ParseUint(strings.TrimSpace(w), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad weight in %q: %v", part, err)
		}
		name = strings.TrimSpace(name)
		if seen[name] {
			return nil, fmt.Errorf("%s listed twice in --weights", name)
		}
		seen[name] = true
		out = append(out, target{name: name, weight: weight})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--weights is empty")
	}
	return out, nil
}

// orderTargets drops no-ops and orders the remaining changes ALL RAISES FIRST,
// then lowers (input order preserved within each group). Converge-first rule:
// raising the replacements before lowering the old carriers means the live
// total weight never passes through a low window mid-seesaw, so the L1 keeps
// (or regains) quorum at every intermediate step.
func orderTargets(ts []target, current map[string]uint64) []target {
	var raises, lowers []target
	for _, t := range ts {
		switch cur := current[t.name]; {
		case t.weight > cur:
			raises = append(raises, t)
		case t.weight < cur:
			lowers = append(lowers, t)
		}
	}
	return append(raises, lowers...)
}

// weightStep is one intermediate SetL1ValidatorWeightTx in a staggered
// converge: drive validator name to weight (which may be an intermediate
// value on the way to its final target, not the target itself).
type weightStep struct {
	name   string
	weight uint64
}

// maxStepDelta is the most weight a single step may add or remove: 20% of the
// current live total. Ported from the deleted C-chain converge engine
// (cmd/reconcile/weights.go nextWeight, maxDelta = total/5), which moved
// weight in bounded increments so the churn-limited manager absorbed each one.
// That engine relied on the contract's churn tracker to space the effects;
// self-signed apply has no such tracker, so planSteps caps the per-step delta
// AND apply spaces the steps in wall-clock time (settleWait). Both are needed:
// a step larger than this concentrates too much weight in one proposer-lag
// window and can wedge the L1 under load.
func maxStepDelta(total uint64) uint64 {
	d := total / 5
	if d == 0 {
		d = 1
	}
	return d
}

// nextWeight returns the furthest weight toward desired reachable in ONE step
// without moving more than maxStepDelta of the live total.
func nextWeight(current, desired, total uint64) uint64 {
	max := maxStepDelta(total)
	if desired > current {
		if desired-current > max {
			return current + max
		}
		return desired
	}
	if current-desired > max {
		return current - max
	}
	return desired
}

// maxPlanSteps bounds the simulated step sequence; each step moves at least 1
// toward its target so a real plan terminates far below this. A hit means a
// bug, not a legitimate plan.
const maxPlanSteps = 1000

// planSteps expands ordered targets into a staggered sequence of
// single-validator weight steps. It replays the converge locally: raises to
// completion FIRST (so the live total never dips through a low window
// mid-seesaw and quorum is preserved), then lowers, each step capped by
// nextWeight so no single proposer-lag window absorbs more than ~20% of the
// live total. The final step for every validator is EXACTLY its target
// (nextWeight lands on desired once the remaining delta fits under the cap).
// Pure and deterministic: drives the offline planner test. total is the whole
// L1's live total weight; non-target validators are baked into it and never
// move.
func planSteps(ordered []target, current map[string]uint64, total uint64) []weightStep {
	cur := make(map[string]uint64, len(current))
	for k, v := range current {
		cur[k] = v
	}
	var steps []weightStep
	for len(steps) < maxPlanSteps {
		var pick *target
		for i := range ordered { // raises first
			if ordered[i].weight > cur[ordered[i].name] {
				pick = &ordered[i]
				break
			}
		}
		if pick == nil {
			for i := range ordered { // then lowers
				if ordered[i].weight < cur[ordered[i].name] {
					pick = &ordered[i]
					break
				}
			}
		}
		if pick == nil {
			return steps // converged
		}
		c := cur[pick.name]
		to := nextWeight(c, pick.weight, total)
		steps = append(steps, weightStep{name: pick.name, weight: to})
		total = total - c + to
		cur[pick.name] = to
	}
	return steps
}

// settleWait is the pause between staggered weight steps. Post-Durango the
// proposer schedule and consensus vote weight only pick up a committed weight
// change after RecentlyAcceptedWindowTTL (30s) plus a couple of blocks, so
// each step must clear that window before the next lands: otherwise the steps'
// effects stack inside one window and reproduce the one-burst concentration
// this staggering exists to prevent. Slower apply is the deliberate trade
// (correctness over speed for weight shifts under load).
const settleWait = 35 * time.Second

// applyStepAttempts x the vset fetch retries bounds how often one weight step
// is retried against transient rejections (stale proposer context, stale
// load-balanced reads).
const applyStepAttempts = 3

// verifyTimeout bounds the per-tx on-chain verification poll.
const verifyTimeout = 90 * time.Second

// apply converges the on-chain weights to the --weights targets, one
// SetL1ValidatorWeightTx at a time, re-reading the registered set before
// every tx and verifying each change on-chain before the next.
func apply(ctx context.Context, cfg config, weightsCSV string) {
	ts, err := parseTargets(weightsCSV)
	if err != nil {
		fatalf("apply: %v", err)
	}
	pc := platformvm.NewClient(cfg.net.API)
	vs, err := vset.Fetch(ctx, pc, cfg.subnetID, len(ts))
	if err != nil {
		fatalf("%v", err)
	}

	// Resolve every name and snapshot current weights BEFORE any tx: a typo'd
	// name aborts the whole apply instead of surfacing mid-seesaw.
	byName := make(map[string]vset.Validator, len(ts))
	current := make(map[string]uint64, len(ts))
	for _, t := range ts {
		v := resolveValidator(cfg, vs, t.name)
		byName[t.name] = v
		current[t.name] = v.Weight
	}

	ordered := orderTargets(ts, current)
	if len(ordered) == 0 {
		fmt.Println("weights already as desired")
		return
	}

	// Live total across the WHOLE registered set (not just the targets):
	// per-step bound is a fraction of it, and non-target validators still
	// count toward the weight the L1 votes with.
	var liveTotal uint64
	for i := range vs {
		liveTotal += vs[i].Weight
	}
	steps := planSteps(ordered, current, liveTotal)

	fmt.Printf("applying %d weight change(s) in %d staggered step(s), raises first:\n", len(ordered), len(steps))
	for _, t := range ordered {
		fmt.Printf("  %s: %d -> %d\n", t.name, current[t.name], t.weight)
	}
	fmt.Printf("each step moves <=20%% of the live total, then waits %s for the proposer schedule to catch up before the next (prevents the one-burst weight concentration that wedges the L1 under load)\n", settleWait)

	signers, err := loadSigners(cfg.stakingDir, cfg.signTier())
	if err != nil {
		fatalf("%v", err)
	}
	wallet := makeWallet(ctx, cfg)
	for i, s := range steps {
		fmt.Printf("step %d/%d: %s -> %d\n", i+1, len(steps), s.name, s.weight)
		st := target{name: s.name, weight: s.weight}
		if err := applyStep(ctx, cfg, pc, wallet, signers, byName[s.name].NodeID, st, len(vs)); err != nil {
			fatalf("apply %s: %v (already-applied steps are on-chain; re-run the same apply to resume, it skips converged validators)", s.name, err)
		}
		if i < len(steps)-1 {
			fmt.Printf("  settling %s before the next step...\n", settleWait)
			select {
			case <-ctx.Done():
				fatalf("apply: %v (already-applied steps are on-chain; re-run the same apply to resume)", ctx.Err())
			case <-time.After(settleWait):
			}
		}
	}
	fmt.Println("applied: all targets verified on-chain")
}

// applyStep drives one validator to its target weight: fresh set read, local
// sign, submit, verify on-chain. Retried a few times because P-chain warp
// verification runs at the proposer's height (which can lag) and the public
// API is load-balanced (reads can be stale); every retry re-reads and
// re-signs fresh.
func applyStep(ctx context.Context, cfg config, pc *platformvm.Client, wallet pwallet.Wallet, signers []bls.Signer, nodeID ids.NodeID, t target, minSet int) error {
	var lastErr error
	for attempt := 1; attempt <= applyStepAttempts; attempt++ {
		if attempt > 1 {
			fmt.Printf("  %s: retrying (%v)\n", t.name, lastErr)
			time.Sleep(2 * time.Second)
		}
		vs, err := vset.Fetch(ctx, pc, cfg.subnetID, minSet)
		if err != nil {
			lastErr = err
			continue
		}
		var v *vset.Validator
		for i := range vs {
			if vs[i].NodeID == nodeID {
				v = &vs[i]
				break
			}
		}
		if v == nil {
			lastErr = fmt.Errorf("%s (%s) not in the fetched set", t.name, nodeID)
			continue
		}
		if v.Weight == t.weight {
			return nil // already there (converged earlier, or a resumed re-run)
		}
		signSet, err := signingSet(ctx, pc, cfg, vs)
		if err != nil {
			lastErr = err
			continue
		}
		if err := submitWeight(cfg, wallet, signers, signSet, *v, t.weight); err != nil {
			lastErr = err
			continue
		}
		if err := waitWeight(ctx, pc, v.ValidationID, t.weight); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

// waitWeight polls the P-chain until the validator's weight reads back as
// target, proving the tx applied before the next one is planned.
func waitWeight(ctx context.Context, pc *platformvm.Client, validationID ids.ID, targetWeight uint64) error {
	deadline := time.Now().Add(verifyTimeout)
	for {
		l1v, _, err := pc.GetL1Validator(ctx, validationID)
		if err == nil && l1v.Weight == targetWeight {
			return nil
		}
		// Weight 0 removes the validator; "not found" IS the applied state.
		if targetWeight == 0 && err != nil && strings.Contains(err.Error(), "not found") {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("verify weight: %w", err)
			}
			return fmt.Errorf("weight still %d (want %d) after %s", l1v.Weight, targetWeight, verifyTimeout)
		}
		time.Sleep(2 * time.Second)
	}
}
