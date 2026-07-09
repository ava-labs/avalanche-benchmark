package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/keychain"
	"github.com/ava-labs/avalanchego/utils/rpc"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	platformapi "github.com/ava-labs/avalanchego/vms/platformvm/api"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	avalancheWarp "github.com/ava-labs/avalanchego/vms/platformvm/warp"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	pchain "github.com/ava-labs/avalanchego/wallet/chain/p"
	pbuilder "github.com/ava-labs/avalanchego/wallet/chain/p/builder"
	psigner "github.com/ava-labs/avalanchego/wallet/chain/p/signer"
	pwallet "github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	walletcommon "github.com/ava-labs/avalanchego/wallet/subnet/primary/common"
	ethcommon "github.com/ava-labs/libevm/common"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fujikey"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/netcfg"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

// On-chain weight reconciliation. The desired state is the intents' weights;
// the current state is read fresh from the ValidatorManager contract (Fuji
// C-chain) and the Fuji P-chain on every step, so any crash or transient
// failure is recovered by simply re-running: every action is derived from
// observation, never from memory. Flow per validator:
//
//	contract.weight != desired  -> initiateValidatorWeightUpdate (ratcheted in
//	                               steps of <=20% of the live total: the churn
//	                               cap with churnPeriodSeconds=0). The churn
//	                               math is deterministic, so the WHOLE ratchet
//	                               is simulated locally and fired as one burst
//	                               of consecutive-nonce txs landing in one or
//	                               two blocks, not one receipt wait per step.
//	pchain.minNonce <= sentNonce -> aggregate the FINAL (highest-nonce) warp
//	                               message, deliver one SetL1ValidatorWeightTx
//	                               (P-chain nonce skipping collapses the
//	                               ratchet intermediates; fires even when the
//	                               weights already match, because the ack for
//	                               sentNonce needs the P-chain to record it)
//	receivedNonce < sentNonce   -> aggregate the P-chain ack, complete on the
//	                               contract (bookkeeping, keeps contract state
//	                               equal to P-chain state for observation)
//
// Raises are ordered before lowers in every phase, so the fleet never passes
// through a low-total-weight window: liveness is preserved mid-seesaw.

const weightRounds = 100 // bound on simulated steps per plan and burst rounds; a full DC seesaw needs ~10 steps in 1 round

const (
	// weightConverge* pace the outer retry of the whole converge sequence.
	// Early retries are fast (the private aggregator's cold start fails once
	// then succeeds in seconds), then back off to 2m for the slow transient:
	// Fuji primary-network signature coverage climbing past the 67% quorum as
	// a fresh C-chain warp message propagates (minutes). 24 attempts on the
	// escalating schedule below is ~36 min of waiting before deferring to a
	// manual re-run of the `fleet weight` command.
	weightConvergeAttempts = 24
	weightConvergeTimeout  = 40 * time.Minute
)

// weightRetrySchedule is the delay before attempts 2, 3, ...; past the end it
// stays at the last entry.
var weightRetrySchedule = []time.Duration{
	time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second,
	30 * time.Second, time.Minute, 2 * time.Minute,
}

// delayForAttempt returns the sleep before the given attempt (attempt >= 2).
func delayForAttempt(attempt int) time.Duration {
	i := attempt - 2
	if i < 0 {
		i = 0
	}
	if i >= len(weightRetrySchedule) {
		i = len(weightRetrySchedule) - 1
	}
	return weightRetrySchedule[i]
}

// nextWeight returns the furthest weight toward desired reachable in ONE
// initiateValidatorWeightUpdate, given the churn cap: with period 0 each op
// may move at most 20% of the tracker total (equality passes: the contract
// reverts only when 20*total < delta*100, i.e. delta > total/5).
func nextWeight(current, desired, total uint64) uint64 {
	maxDelta := total / 5
	if maxDelta == 0 {
		maxDelta = 1
	}
	if desired > current {
		if desired-current > maxDelta {
			return current + maxDelta
		}
		return desired
	}
	if current-desired > maxDelta {
		return current - maxDelta
	}
	return desired
}

// stakingTarget is one registered validator's desired vs observed state.
type stakingTarget struct {
	slot         int
	validationID ids.ID
	desired      uint64
}

type weightEngine struct {
	cfg      *config
	subnetID ids.ID
	cChainID ids.ID
	cli      *valmgr.Client
	pClient  *platformvm.Client
	kc       *secp256k1fx.Keychain
	wallet   pwallet.Wallet // built lazily; only P-chain deliveries need it
	targets  []stakingTarget
	// lastFailedAggregate remembers, per validationID, the signed message
	// bytes of the last under-quorum failure. A retry that aggregates to the
	// SAME bytes is Glacier's per-message cache serving a stale aggregate; the
	// only escape is re-aggregating from the primary aggregator only.
	lastFailedAggregate map[ids.ID][]byte
}

// The whole seesaw runs off publicnode's per-chain Fuji RPC, never touching the
// aggressively rate-limited api.avax-test.network (the fleet's egress IP is
// throttled there and 429s on the first hit). Both hosts are overridable.

// cchainRPCURL is where every ValidatorManager eth_call and initiate/complete
// tx goes.
func cchainRPCURL() string {
	return netcfg.Get().CChainRPC
}

// pchainReadClient returns the platformvm client used for reads AND wallet tx
// issuance. publicnode serves the P-chain API at /ext/bc/P but NOT at /ext/P
// (the only path platformvm.NewClient builds), so construct it on the exact URL.
func pchainReadClient() *platformvm.Client {
	return &platformvm.Client{Requester: rpc.NewEndpointRequester(netcfg.Get().PChainRPC)}
}

// gasPriceMultiplier mirrors wallet/chain/p.gasPriceMultiplier (headroom for
// issuing several txs in a row).
const gasPriceMultiplier = 2

// newWeightEngine wires the C-chain client, P-chain client and the
// slot -> validationID mapping. Returns nil (with a notice) when the deploy
// has no manager configured: nothing to reconcile on-chain.
func newWeightEngine(ctx context.Context, cfg *config, intents []MachineIntent) (*weightEngine, error) {
	managerHex := os.Getenv("MANAGER_ADDRESS")
	if managerHex == "" {
		return nil, fmt.Errorf("MANAGER_ADDRESS is not set: this deploy has no ValidatorManager, weights cannot be reconciled (subnets created before C-chain managed weights are immutable)")
	}
	subnetID, err := ids.FromString(cfg.subnetID)
	if err != nil {
		return nil, fmt.Errorf("parse SUBNET_ID: %w", err)
	}
	key, err := fujikey.Load(cfg.repoDir + "/staking/fuji-wallet.key")
	if err != nil {
		return nil, fmt.Errorf("load fuji wallet key: %w", err)
	}
	cli, err := valmgr.Dial(ctx, cchainRPCURL(), key, ethcommon.HexToAddress(managerHex))
	if err != nil {
		return nil, err
	}
	// The network's C-chain blockchain ID is a fixed constant (netcfg),
	// hardcoded so we never call info.getBlockchainID (publicnode does not
	// serve /ext/info, and the official API rate-limits it).
	cChainID, err := ids.FromString(netcfg.Get().CChainID)
	if err != nil {
		return nil, err
	}

	e := &weightEngine{
		cfg:                 cfg,
		subnetID:            subnetID,
		cChainID:            cChainID,
		cli:                 cli,
		pClient:             pchainReadClient(),
		kc:                  secp256k1fx.NewKeychain(key),
		lastFailedAggregate: make(map[ids.ID][]byte),
	}
	e.targets, err = stakingTargets(cfg, subnetID, intents)
	return e, err
}

// stakingTargets derives every registered validator's validationID from the
// committed NodeIDs: the conversion tx sorted validators by NodeID bytes, so
// the conversion index (validationID input) is recomputed the same way.
func stakingTargets(cfg *config, subnetID ids.ID, intents []MachineIntent) ([]stakingTarget, error) {
	slots := cfg.topo.StakingSlots()
	nodeIDs := make([]ids.NodeID, len(slots))
	for k, s := range slots {
		id, err := ids.NodeIDFromString(cfg.nodeIDForKey(cfg.topo.KeyOf(s)))
		if err != nil {
			return nil, fmt.Errorf("parse NodeID for key %d: %w", cfg.topo.KeyOf(s), err)
		}
		nodeIDs[k] = id
	}
	conv := valmgr.ConversionIndices(nodeIDs)
	targets := make([]stakingTarget, len(slots))
	for k, s := range slots {
		targets[k] = stakingTarget{
			slot:         s,
			validationID: valmgr.ValidationID(subnetID, uint32(conv[k])),
			desired:      intents[s].Weight,
		}
	}
	return targets, nil
}

// reconcileWeights converges the on-chain weights to the intents. Called by
// every state-changing command after the process passes; re-running the same
// `fleet weight` command resumes it. Idempotent and resumable at any point.
func reconcileWeights(cfg *config, intents []MachineIntent) {
	if os.Getenv("MANAGER_ADDRESS") == "" {
		fmt.Println("[3/3] weights: SKIPPED - MANAGER_ADDRESS not set (pre-manager deploy; on-chain weights are immutable)")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), weightConvergeTimeout)
	defer cancel()

	e, err := newWeightEngine(ctx, cfg, intents)
	if err != nil {
		fatalf("weights: %v", err)
	}
	fmt.Printf("[3/3] weights: reconciling via ValidatorManager %s (subnet %s)\n", e.cli.Manager.Hex(), e.subnetID)

	// The whole sequence is retried: each pass re-observes everything, so a
	// transient failure just repeats the remaining work. The dominant transient
	// is Fuji signature coverage: a SetL1ValidatorWeightTx carries a warp
	// message signed by the Fuji PRIMARY NETWORK, and Fuji validators only sign
	// a C-chain-originated message once they have synced the C-chain block that
	// emitted the initiate. Right after an initiate, coverage sits below the 67%
	// quorum (measured ~52% on 2026-07-07) and climbs over MINUTES as the block
	// propagates. The schedule retries fast first (aggregator cold starts
	// resolve in seconds) and settles at 2m for the slow coverage climb; the
	// chain stays healthy on the current weights throughout (delivery only moves
	// weight once it lands). Re-run the `fleet weight` command to resume past the timeout.
	var lastErr error
	for attempt := 1; attempt <= weightConvergeAttempts; attempt++ {
		if attempt > 1 {
			delay := delayForAttempt(attempt)
			fmt.Printf("  weights: attempt %d/%d in %s (last: %v)\n",
				attempt, weightConvergeAttempts, delay, lastErr)
			select {
			case <-ctx.Done():
				fatalf("weights: %v (re-run the `fleet weight` command to resume; every step is idempotent)", ctx.Err())
			case <-time.After(delay):
			}
		}
		if lastErr = e.converge(ctx); lastErr == nil {
			fmt.Println("  weights: converged (contract == P-chain == desired)")
			return
		}
		// The precheck normally catches under-quorum before issuing; if the
		// P-chain still rejects and deliverValidator did not already identify a
		// stale proposer context, give the operator the coverage explanation.
		if strings.Contains(lastErr.Error(), avalancheWarp.ErrInsufficientWeight.Error()) &&
			!strings.Contains(lastErr.Error(), errCoverage.Error()) &&
			!strings.Contains(lastErr.Error(), errStaleProposerContext.Error()) {
			lastErr = fmt.Errorf("%w (%v)", lastErr, errCoverage)
		}
	}
	fatalf("weights: %v (re-run the `fleet weight` command to resume; every step is idempotent)", lastErr)
}

func (e *weightEngine) converge(ctx context.Context) error {
	if err := e.convergeContract(ctx); err != nil {
		return err
	}
	if err := e.deliverToPChain(ctx); err != nil {
		return err
	}
	if err := e.completeOnContract(ctx); err != nil {
		return err
	}
	return e.verifyConverged(ctx)
}

// convergeContract ratchets every validator's CONTRACT weight to desired.
// Each round observes once, simulates the full seesaw locally (planSeesaw)
// and fires every initiate in one burst; a clean batch converges in a single
// round and the next round verifies it observed nothing left to do. A step
// that reverts (state drift, budget mispredict) just costs its gas: the next
// round re-observes and plans a corrective batch.
func (e *weightEngine) convergeContract(ctx context.Context) error {
	for round := 0; round < weightRounds; round++ {
		total, err := e.cli.L1TotalWeight(ctx)
		if err != nil {
			return fmt.Errorf("l1TotalWeight: %w", err)
		}
		current := make(map[ids.ID]uint64, len(e.targets))
		names := make(map[ids.ID]string, len(e.targets))
		for _, t := range e.targets {
			v, err := e.cli.GetValidator(ctx, t.validationID)
			if err != nil {
				return fmt.Errorf("getValidator(%s): %w", e.slotName(t), err)
			}
			current[t.validationID] = v.Weight
			names[t.validationID] = e.slotName(t)
		}
		steps := planSeesaw(e.targets, current, total)
		if len(steps) == 0 {
			return nil // contract fully converged
		}
		fmt.Printf("  weights: firing %d initiates in one burst:\n", len(steps))
		for _, s := range steps {
			fmt.Printf("    %s -> %d\n", names[s.ValidationID], s.Weight)
		}
		if err := e.cli.InitiateWeightUpdates(ctx, steps); err != nil {
			return err
		}
	}
	return fmt.Errorf("contract weights did not converge within %d rounds", weightRounds)
}

// planSeesaw simulates the contract's churn tracker (period 0: every op
// re-seeds the 20% budget from the running total) and returns the exact
// initiate sequence that converges current -> desired: first unfinished raise,
// else first unfinished lower, each step capped by nextWeight. Deterministic,
// so the burst it plans is what the contract will accept.
func planSeesaw(targets []stakingTarget, observed map[ids.ID]uint64, total uint64) []valmgr.WeightStep {
	current := make(map[ids.ID]uint64, len(observed))
	for id, w := range observed {
		current[id] = w
	}
	var steps []valmgr.WeightStep
	for len(steps) < weightRounds {
		var pick *stakingTarget
		for i := range targets {
			if current[targets[i].validationID] < targets[i].desired {
				pick = &targets[i]
				break
			}
		}
		if pick == nil {
			for i := range targets {
				if current[targets[i].validationID] > targets[i].desired {
					pick = &targets[i]
					break
				}
			}
		}
		if pick == nil {
			return steps
		}
		cur := current[pick.validationID]
		to := nextWeight(cur, pick.desired, total)
		steps = append(steps, valmgr.WeightStep{ValidationID: pick.validationID, Weight: to})
		total = total - cur + to
		current[pick.validationID] = to
	}
	return steps
}

// needsDelivery reports whether the contract's latest weight message (nonce
// sentNonce) has not yet been recorded by the P-chain. Delivering nonce N sets
// the P-chain's minNonce to N+1, so undelivered means minNonce <= sentNonce.
// This must NOT be a weight comparison: an abandoned seesaw can leave the
// weights equal while the contract's nonce is ahead (demote-then-repromote),
// and the ack for sentNonce can never be signed until that nonce is delivered.
// SetL1ValidatorWeightTx with an unchanged weight but a higher nonce is legal
// (ACP-77 nonce skipping) and exists exactly to advance minNonce.
// sentNonce == 0 means no initiate ever happened: nothing to deliver.
func needsDelivery(sentNonce, minNonce uint64) bool {
	return sentNonce > 0 && minNonce <= sentNonce
}

// deliverValidator issues one SetL1ValidatorWeightTx for t if the contract's
// FINAL (highest-nonce) message has not reached the P-chain yet.
func (e *weightEngine) deliverValidator(ctx context.Context, t stakingTarget) error {
	v, err := e.cli.GetValidator(ctx, t.validationID)
	if err != nil {
		return fmt.Errorf("getValidator(%s): %w", e.slotName(t), err)
	}
	pv, _, err := e.pClient.GetL1Validator(ctx, t.validationID)
	if err != nil {
		return fmt.Errorf("platform.getL1Validator(%s): %w", e.slotName(t), err)
	}
	if !needsDelivery(v.SentNonce, pv.MinNonce) {
		return nil
	}
	unsigned, err := valmgr.WeightMessage(netcfg.Get().NetworkID, e.cChainID, e.cli.Manager, t.validationID, v.SentNonce, v.Weight)
	if err != nil {
		return err
	}
	if pv.Weight == v.Weight {
		fmt.Printf("  weights: %s weights match at %d but P-chain nonce %d lags contract nonce %d: delivering nonce %d to unblock acks\n",
			e.slotName(t), v.Weight, pv.MinNonce, v.SentNonce, v.SentNonce)
	} else {
		fmt.Printf("  weights: %s deliver weight %d (nonce %d) to the P-chain\n", e.slotName(t), v.Weight, v.SentNonce)
	}
	signed, err := valmgr.Aggregate(ctx, unsigned, nil)
	if err != nil {
		return fmt.Errorf("aggregate weight message for %s: %w", e.slotName(t), err)
	}
	// Byte-identical to the last under-quorum failure: Glacier's per-message
	// cache is serving the same stale aggregate on every retry (it can never
	// heal on its own while the primary-network set drifts). Sidestep the cache
	// by re-aggregating from the primary aggregator ONLY.
	if prev, ok := e.lastFailedAggregate[t.validationID]; ok && bytes.Equal(signed.Bytes(), prev) {
		fmt.Printf("  weights: %s aggregate is cached and under quorum (byte-identical to the last failed attempt); re-aggregating from the primary aggregator only\n", e.slotName(t))
		signed, err = valmgr.AggregatePrimary(ctx, unsigned, nil)
		if err != nil {
			return fmt.Errorf("primary-only re-aggregation for %s (Glacier cache is under quorum): %w", e.slotName(t), err)
		}
	}
	verified, err := e.precheckQuorum(ctx, signed, e.slotName(t))
	if err != nil {
		e.lastFailedAggregate[t.validationID] = signed.Bytes()
		return err
	}
	w, err := e.pWallet(ctx)
	if err != nil {
		return err
	}
	if _, err := w.IssueSetL1ValidatorWeightTx(signed.Bytes()); err != nil {
		if strings.Contains(err.Error(), avalancheWarp.ErrInsufficientWeight.Error()) {
			e.lastFailedAggregate[t.validationID] = signed.Bytes()
		}
		// Stale proposer context: "signature is invalid" is always it (never a
		// coverage problem), and "signature weight is insufficient" is it too
		// WHEN our own full verification just passed at the current height (the
		// bitset summed low only at the node's frozen pre-drift context;
		// measured live 2026-07-09: same aggregate 67.74% at tip 286989, 40.59%
		// at preferred 286984, byte-identical to the P-chain's reject).
		stale := strings.Contains(err.Error(), avalancheWarp.ErrInvalidSignature.Error()) ||
			(verified && strings.Contains(err.Error(), avalancheWarp.ErrInsufficientWeight.Error()))
		if stale {
			e.nudgePChain(ctx, w)
			return fmt.Errorf("SetL1ValidatorWeightTx for %s: %w (%v)", e.slotName(t), err, errStaleProposerContext)
		}
		return fmt.Errorf("SetL1ValidatorWeightTx for %s: %w", e.slotName(t), err)
	}
	delete(e.lastFailedAggregate, t.validationID)
	return nil
}

// errStaleProposerContext explains a P-chain warp reject that our own full
// precheck passed: the node verifies IssueTx warp messages at its preferred
// block's PROPOSER CONTEXT height (or GetMinimumHeight), not the tip
// (platformvm block/executor manager.VerifyTx). If the Fuji primary set
// changed since the preferred block was proposed, the aggregate's signer
// bitset resolves to different validators at that stale height; depending on
// how the canonical indices shift this surfaces as "signature is invalid"
// (BLS key mismatch, seen 2026-07-08) or as "signature weight is insufficient"
// (the shifted bits undercount; the weight check runs before the BLS check and
// masks it, seen 2026-07-09: same aggregate 67.74% at tip, 40.59% one height
// earlier). Re-aggregating cannot help until a NEW P-chain block refreshes the
// context, and on a quiet Fuji P-chain no block may come for ~40min.
var errStaleProposerContext = fmt.Errorf("the P-chain verifies at its preferred block's proposer-context height, which predates a primary-network validator-set change; a no-op BaseTx was issued to force a fresh block, the next retry should pass")

// nudgePChain issues a no-op BaseTx (all change back to our own key) purely to
// land a fresh P-chain block: the new block carries a current proposer-context
// height, unsticking warp verification after a validator-set change on an
// otherwise idle Fuji P-chain. Our stuck SetL1ValidatorWeightTx cannot do this
// itself because it is rejected at admission, before ever reaching a block.
// Cost: one base fee. Best-effort: on failure the retry loop just waits for a
// third-party block as before.
func (e *weightEngine) nudgePChain(ctx context.Context, w pwallet.Wallet) {
	if _, err := w.IssueBaseTx(nil, walletcommon.WithContext(ctx)); err != nil {
		fmt.Printf("  weights: P-chain nudge BaseTx failed (%v); waiting for a third-party block to refresh the proposer context\n", err)
		return
	}
	fmt.Println("  weights: issued a no-op BaseTx to advance the P-chain proposer context past the validator-set change")
}

// pchainQuorum mirrors the warp quorum the P-chain enforces on
// SetL1ValidatorWeightTx: 67/100 of the Fuji primary-network stake.
const (
	pchainQuorumNum = 67
	pchainQuorumDen = 100
)

// precheckQuorum runs the P-chain's exact warp verification (signer weight AND
// the BLS aggregate) against the CURRENT proposed-height Fuji primary-network
// validator set before issuing. Two failure modes it catches (both measured
// live 2026-07-08):
//   - Glacier does NOT error on under-quorum: it returns a signedMessage with
//     whatever weight it collected.
//   - Glacier CACHES the aggregate per message (identical bytes across calls,
//     hours apart; quorumPercentage/signingSubnetId/justification do not bust
//     the key). The signer bitset indexes the canonical validator set at
//     aggregation time; as the Fuji set drifts, the cached bitset can map to
//     entirely different validators (seen: same signature summing 40.6% then
//     67.7% as the set changed), so a weight-only check is not enough: only
//     the full Verify catches a stale mapping.
//
// A rejected precheck costs nothing: the message stays undelivered and the
// next attempt re-checks against the then-current set. Fall-through (cannot
// read the set) is loud and lets the P-chain judge.
// It returns verified=true only when the full Verify ran and passed: that is
// the evidence that a subsequent P-chain weight-insufficient reject is a stale
// proposer context, not a coverage problem.
func (e *weightEngine) precheckQuorum(ctx context.Context, signed *avalancheWarp.Message, name string) (bool, error) {
	vdrs, err := e.pClient.GetValidatorsAt(ctx, constants.PrimaryNetworkID, platformapi.ProposedHeight)
	if err != nil {
		fmt.Printf("  weights: %s precheck SKIPPED (platform.getValidatorsAt: %v); issuing and letting the P-chain judge\n", name, err)
		return false, nil
	}
	warpSet, err := validators.FlattenValidatorSet(vdrs)
	if err != nil {
		fmt.Printf("  weights: %s precheck SKIPPED (flatten validator set: %v); issuing and letting the P-chain judge\n", name, err)
		return false, nil
	}
	if err := signed.Signature.Verify(&signed.UnsignedMessage, netcfg.Get().NetworkID, warpSet, pchainQuorumNum, pchainQuorumDen); err != nil {
		return false, fmt.Errorf("aggregate for %s fails P-chain warp verification against the current proposed-height primary-network set (%v): %w",
			name, err, errCoverage)
	}
	return true, nil
}

// errCoverage tags every under-quorum failure (local precheck or a
// P-chain reject) with the operator explanation.
var errCoverage = fmt.Errorf("primary-network signature coverage, not a local fault: primary-network validators sign a C-chain warp message only after syncing the block that emitted it, so coverage climbs over minutes and retries continue automatically; if the Glacier FALLBACK served this aggregate it may also be a stale per-message cache entry, which the next attempt sidesteps by re-aggregating from the primary aggregator only")

// deliverToPChain delivers every validator whose latest contract nonce has not
// reached the P-chain. Raises land before lowers (weight-neutral nonce
// catch-ups ride with the raises).
func (e *weightEngine) deliverToPChain(ctx context.Context) error {
	var raises, lowers []stakingTarget
	for _, t := range e.targets {
		v, err := e.cli.GetValidator(ctx, t.validationID)
		if err != nil {
			return fmt.Errorf("getValidator(%s): %w", e.slotName(t), err)
		}
		pv, _, err := e.pClient.GetL1Validator(ctx, t.validationID)
		if err != nil {
			return fmt.Errorf("platform.getL1Validator(%s): %w", e.slotName(t), err)
		}
		if !needsDelivery(v.SentNonce, pv.MinNonce) {
			continue
		}
		if v.Weight >= pv.Weight {
			raises = append(raises, t)
		} else {
			lowers = append(lowers, t)
		}
	}
	for _, t := range append(raises, lowers...) {
		if err := e.deliverValidator(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// completeOnContract closes the loop: deliver the P-chain-signed ack so the
// contract's receivedNonce catches up to sentNonce.
func (e *weightEngine) completeOnContract(ctx context.Context) error {
	for _, t := range e.targets {
		v, err := e.cli.GetValidator(ctx, t.validationID)
		if err != nil {
			return fmt.Errorf("getValidator(%s): %w", e.slotName(t), err)
		}
		if v.ReceivedNonce >= v.SentNonce {
			continue
		}
		pv, _, err := e.pClient.GetL1Validator(ctx, t.validationID)
		if err != nil {
			return fmt.Errorf("platform.getL1Validator(%s): %w", e.slotName(t), err)
		}
		if pv.MinNonce == 0 {
			continue // nothing delivered yet; the next pass delivers first
		}
		if pv.MinNonce-1 <= v.ReceivedNonce {
			// The P-chain's latest signable ack (MinNonce-1) is one the
			// contract already has: re-sending it cannot advance receivedNonce.
			// Delivery of sentNonce must land first; the next pass does that.
			continue
		}
		// The P-chain signs the ack only against its exact current state.
		unsigned, err := valmgr.WeightAckMessage(netcfg.Get().NetworkID, t.validationID, pv.MinNonce-1, pv.Weight)
		if err != nil {
			return err
		}
		fmt.Printf("  weights: %s complete (ack nonce %d, weight %d)\n", e.slotName(t), pv.MinNonce-1, pv.Weight)
		signed, err := valmgr.Aggregate(ctx, unsigned, nil)
		if err != nil {
			return fmt.Errorf("aggregate ack for %s: %w", e.slotName(t), err)
		}
		if err := e.cli.CompleteWeightUpdate(ctx, signed.Bytes()); err != nil {
			return fmt.Errorf("completeValidatorWeightUpdate for %s: %w", e.slotName(t), err)
		}
	}
	return nil
}

// verifyConverged re-reads everything and demands desired == contract ==
// P-chain, receivedNonce == sentNonce.
func (e *weightEngine) verifyConverged(ctx context.Context) error {
	var bad []string
	for _, t := range e.targets {
		v, err := e.cli.GetValidator(ctx, t.validationID)
		if err != nil {
			return err
		}
		pv, _, err := e.pClient.GetL1Validator(ctx, t.validationID)
		if err != nil {
			return err
		}
		if v.Weight != t.desired || pv.Weight != t.desired || v.ReceivedNonce != v.SentNonce {
			line := fmt.Sprintf("%s desired=%d contract=%d pchain=%d nonces=%d/%d",
				e.slotName(t), t.desired, v.Weight, pv.Weight, v.ReceivedNonce, v.SentNonce)
			if v.Weight == t.desired && pv.Weight == t.desired {
				line += fmt.Sprintf(" (weights match; P-chain nonce %d lags contract nonce %d, next pass delivers nonce %d to unblock the ack)",
					pv.MinNonce, v.SentNonce, v.SentNonce)
			}
			bad = append(bad, line)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("not converged: %s", strings.Join(bad, "; "))
	}
	return nil
}

func (e *weightEngine) slotName(t stakingTarget) string {
	return e.cfg.topo.MachineName(t.slot)
}

// pWallet lazily builds the fee-paying P-chain wallet (UTXO fetch is a
// network round trip; deliveries are the only phase that needs it).
func (e *weightEngine) pWallet(ctx context.Context) (pwallet.Wallet, error) {
	if e.wallet != nil {
		return e.wallet, nil
	}
	w, err := makePWalletNoInfo(ctx, e.pClient, e.kc)
	if err != nil {
		return nil, fmt.Errorf("make P-chain wallet: %w", err)
	}
	e.wallet = w
	return w, nil
}

// makePWalletNoInfo builds a P-chain wallet WITHOUT any /ext/info call. Stock
// primary.MakePWallet fetches the network ID from info, which publicnode's
// per-chain P RPC does not serve; every other input (AVAX asset, fee
// config/state, UTXOs, tx issuance) is a P-chain API call publicnode does
// serve, and the network ID is a per-network constant. This is a faithful copy
// of MakePWallet's body with the info dependency swapped for netcfg's ID.
func makePWalletNoInfo(ctx context.Context, pClient *platformvm.Client, kc keychain.Keychain) (pwallet.Wallet, error) {
	addrs := kc.Addresses()

	avaxAssetID, err := pClient.GetStakingAssetID(ctx, constants.PrimaryNetworkID)
	if err != nil {
		return nil, fmt.Errorf("get AVAX asset id: %w", err)
	}
	feeConfig, err := pClient.GetFeeConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("get fee config: %w", err)
	}
	_, gasPrice, _, err := pClient.GetFeeState(ctx)
	if err != nil {
		return nil, fmt.Errorf("get fee state: %w", err)
	}
	pctx := &pbuilder.Context{
		NetworkID:         netcfg.Get().NetworkID,
		AVAXAssetID:       avaxAssetID,
		ComplexityWeights: feeConfig.Weights,
		GasPrice:          gasPriceMultiplier * gasPrice,
	}

	utxos := walletcommon.NewUTXOs()
	if err := primary.AddAllUTXOs(ctx, utxos, pClient, txs.Codec,
		constants.PlatformChainID, constants.PlatformChainID, addrs.List()); err != nil {
		return nil, fmt.Errorf("fetch P-chain UTXOs: %w", err)
	}
	owners, err := platformvm.GetOwners(pClient, ctx, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("get owners: %w", err)
	}

	pBackend := pwallet.NewBackend(walletcommon.NewChainUTXOs(constants.PlatformChainID, utxos), owners)
	return pwallet.New(
		pchain.NewClient(pClient, pBackend),
		pbuilder.New(addrs, pctx, pBackend),
		psigner.New(kc, pBackend),
	), nil
}

// fetchActualWeights reads every staking slot's CURRENT P-chain weight in one
// pass (slot index -> weight). This is the single batch of P-chain reads a
// status invocation makes: reportHealth and weightsReport both consume the
// result. Any failure (Fuji flaky, MANAGER_ADDRESS unset) returns a nil map
// and the reason; callers degrade to desired-weight display, never crash.
func fetchActualWeights(cfg *config, intents []MachineIntent) (map[int]uint64, error) {
	if os.Getenv("MANAGER_ADDRESS") == "" {
		return nil, fmt.Errorf("MANAGER_ADDRESS not set (immutable pre-manager deploy)")
	}
	subnetID, err := ids.FromString(cfg.subnetID)
	if err != nil {
		return nil, fmt.Errorf("parse SUBNET_ID: %w", err)
	}
	targets, err := stakingTargets(cfg, subnetID, intents)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pClient := pchainReadClient()
	actual := make(map[int]uint64, len(targets))
	for _, t := range targets {
		pv, _, err := pClient.GetL1Validator(ctx, t.validationID)
		if err != nil {
			return nil, fmt.Errorf("platform.getL1Validator(%s): %w", cfg.topo.MachineName(t.slot), err)
		}
		actual[t.slot] = pv.Weight
	}
	return actual, nil
}

// weightsReport prints the desired vs on-chain (P-chain) weight per staking
// slot for `status`, from the weights fetchActualWeights already read.
// Best-effort: a fetch error is reported as a note rather than failing the
// health snapshot.
func weightsReport(cfg *config, intents []MachineIntent, actual map[int]uint64, fetchErr error) {
	if fetchErr != nil {
		fmt.Printf("weights: %v\n", fetchErr)
		return
	}
	converged := true
	var lines []string
	for _, s := range cfg.topo.StakingSlots() {
		mark := ""
		if actual[s] != intents[s].Weight {
			mark = "  <- PENDING"
			converged = false
		}
		lines = append(lines, fmt.Sprintf("  %-3s desired=%-10d pchain=%-10d%s",
			cfg.topo.MachineName(s), intents[s].Weight, actual[s], mark))
	}
	if converged {
		fmt.Println("weights: converged (P-chain == desired)")
		return
	}
	fmt.Println("weights: PENDING - re-run the ./fleet weight that set this tier (primary-network signature coverage may still be catching up)")
	for _, l := range lines {
		fmt.Println(l)
	}
}
