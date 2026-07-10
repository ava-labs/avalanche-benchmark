package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/rpc"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	ethcommon "github.com/ava-labs/libevm/common"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fujikey"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/netcfg"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

// On-chain weight reconciliation, initiate-and-forget. The desired state is
// the intents' weights; the current state is read fresh from the
// ValidatorManager contract (C-chain) and the P-chain on every step, so any
// crash or transient failure is recovered by simply re-running: every action
// is derived from observation, never from memory.
//
// This loop only INITIATES: where contract.weight != desired it fires
// initiateValidatorWeightUpdate txs (ratcheted in steps of <=20% of the live
// total: the churn cap with churnPeriodSeconds=0; the churn math is
// deterministic, so the WHOLE ratchet is simulated locally and fired as one
// burst of consecutive-nonce txs). Delivering the emitted warp message to the
// P-chain (SetL1ValidatorWeightTx) and acking back to the contract
// (completeValidatorWeightUpdate) is the external warp-courier daemon's job;
// this loop just polls until contract == P-chain == desired and
// receivedNonce == sentNonce. A convergence stall therefore points at the
// courier, not at this command.
//
// Raises are ordered before lowers in the burst, so the fleet never passes
// through a low-total-weight window: liveness is preserved mid-seesaw.

const weightRounds = 100 // bound on simulated steps per plan and burst rounds; a full DC seesaw needs ~10 steps in 1 round

const (
	// weightConverge* pace the outer poll of the converge sequence. Early
	// retries are fast (the courier's per-block debounce delivers within
	// seconds on a healthy day), then back off to 2m for the slow transient:
	// primary-network signature coverage climbing past the 67% quorum as a
	// fresh C-chain warp message propagates (minutes, the courier retries on
	// its own). 24 attempts on the escalating schedule below is ~36 min of
	// waiting before deferring to a manual re-run of the `fleet weight`
	// command.
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
	cli      *valmgr.Client
	pClient  *platformvm.Client
	targets  []stakingTarget
}

// The whole seesaw runs off the network.env-configured per-chain RPCs, never
// touching rate-limited default endpoints unless configured to.

// cchainRPCURL is where every ValidatorManager eth_call and initiate tx goes.
func cchainRPCURL() string {
	return netcfg.Get().CChainRPC
}

// pchainReadClient returns the platformvm client used for reads. publicnode
// serves the P-chain API at /ext/bc/P but NOT at /ext/P (the only path
// platformvm.NewClient builds), so construct it on the exact URL.
func pchainReadClient() *platformvm.Client {
	return &platformvm.Client{Requester: rpc.NewEndpointRequester(netcfg.Get().PChainRPC)}
}

// newWeightEngine wires the C-chain client, P-chain client and the
// slot -> validationID mapping.
func newWeightEngine(ctx context.Context, cfg *config, intents []MachineIntent) (*weightEngine, error) {
	managerHex := os.Getenv("MANAGER_ADDRESS")
	if managerHex == "" {
		return nil, fmt.Errorf("MANAGER_ADDRESS is not set: this deploy has no ValidatorManager, weights cannot be reconciled (subnets created before C-chain managed weights are immutable)")
	}
	// The ValidatorManager is owned by a PoAManager wrapper (so the courier's
	// completes are permissionless); initiates must go through it.
	poaHex := os.Getenv("POA_MANAGER_ADDRESS")
	if poaHex == "" {
		return nil, fmt.Errorf("POA_MANAGER_ADDRESS is not set: initiates go through the PoAManager that owns the ValidatorManager (add it to network.env)")
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
	cli.InitiateVia = ethcommon.HexToAddress(poaHex)

	e := &weightEngine{
		cfg:      cfg,
		subnetID: subnetID,
		cli:      cli,
		pClient:  pchainReadClient(),
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
	fmt.Printf("[3/3] weights: reconciling ValidatorManager %s via PoAManager %s (subnet %s)\n", e.cli.Manager.Hex(), e.cli.InitiateVia.Hex(), e.subnetID)
	fmt.Println("  weights: this command only initiates on the contract; P-chain delivery and the contract ack are the warp-courier daemon's job (a stall here means check the courier)")

	// The whole sequence is retried: each pass re-observes everything, so a
	// transient failure just repeats the remaining work. Once the contract
	// matches desired, every further attempt is a pure poll waiting for the
	// courier to deliver to the P-chain and ack back. Re-run the
	// `fleet weight` command to resume past the timeout.
	var lastErr error
	for attempt := 1; attempt <= weightConvergeAttempts; attempt++ {
		if attempt > 1 {
			delay := delayForAttempt(attempt)
			fmt.Printf("  weights: attempt %d/%d in %s (last: %v)\n",
				attempt, weightConvergeAttempts, delay, lastErr)
			select {
			case <-ctx.Done():
				fatalf("weights: %v (re-run the `fleet weight` command to resume; every step is idempotent; if the initiates landed, check the warp-courier daemon)", ctx.Err())
			case <-time.After(delay):
			}
		}
		if lastErr = e.converge(ctx); lastErr == nil {
			fmt.Println("  weights: converged (contract == P-chain == desired)")
			return
		}
	}
	fatalf("weights: %v (re-run the `fleet weight` command to resume; every step is idempotent; if the initiates landed, check the warp-courier daemon)", lastErr)
}

func (e *weightEngine) converge(ctx context.Context) error {
	if err := e.convergeContract(ctx); err != nil {
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

// verifyConverged re-reads everything and demands desired == contract ==
// P-chain, receivedNonce == sentNonce. The P-chain weight and the contract's
// receivedNonce only move when the warp-courier daemon delivers and acks, so
// "not converged" here while the contract already matches desired means the
// courier has not caught up yet (or is down).
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
			if v.Weight == t.desired {
				line += " (initiates landed; waiting on the warp courier to deliver/ack)"
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

// fetchActualWeights reads every staking slot's CURRENT P-chain weight in one
// pass (slot index -> weight). This is the single batch of P-chain reads a
// status invocation makes: reportHealth and weightsReport both consume the
// result. Any failure (RPC flaky, MANAGER_ADDRESS unset) returns a nil map
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
	fmt.Println("weights: PENDING - initiates are fired by `./fleet weight`; P-chain delivery is the warp-courier daemon's job (check it if this persists)")
	for _, l := range lines {
		fmt.Println(l)
	}
}
