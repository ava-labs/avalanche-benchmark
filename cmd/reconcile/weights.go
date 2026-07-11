package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	ethcommon "github.com/ava-labs/libevm/common"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fujikey"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/netcfg"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

// On-chain weight reconciliation, initiate-and-forget. The desired state is
// the intents' weights; the current state is read fresh from the
// ValidatorManager contract (C-chain) on every step, so any crash or
// transient failure is recovered by simply re-running: every action is
// derived from observation, never from memory.
//
// This flow NEVER talks to the P-chain, not even reads: everything it needs
// is visible on the contract. Delivering the emitted warp message to the
// P-chain (SetL1ValidatorWeightTx) and acking back to the contract
// (completeValidatorWeightUpdate) is the external warp-courier daemon's job,
// and the ack it writes (receivedNonce) is the proof the P-chain applied the
// update.
//
// This loop only INITIATES: where contract.weight != desired it fires
// initiateValidatorWeightUpdate txs (ratcheted in steps of <=20% of the live
// total: the churn cap with churnPeriodSeconds=0; the churn math is
// deterministic, so the WHOLE ratchet is simulated locally and fired as one
// burst of consecutive-nonce txs). It then polls the contract until, for
// every slot, weight == desired and sentNonce == receivedNonce. A convergence
// stall therefore points at the courier, not at this command.
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

// waitPrinter gates the between-attempts status output, display only (the
// retry schedule is untouched): a status line prints when its text changes,
// otherwise as a heartbeat at most every heartbeat interval; after stallAfter
// with no text change it adds a single escalation line naming the
// warp-courier service (the one place the courier appears in normal output),
// repeated at most every stallAfter.
type waitPrinter struct {
	heartbeat  time.Duration
	stallAfter time.Duration
	last       string
	lastPrint  time.Time
	lastChange time.Time
	lastStall  time.Time
}

// tick decides what to print for the current status text. stallable marks
// waiting-on-delivery states, the only ones that escalate to the courier
// line; any text change (including hard errors) always prints immediately.
func (w *waitPrinter) tick(msg string, now time.Time, stallable bool) []string {
	if msg != w.last {
		w.last = msg
		w.lastChange = now
		w.lastPrint = now
		w.lastStall = time.Time{}
		return []string{msg}
	}
	var out []string
	if now.Sub(w.lastPrint) >= w.heartbeat {
		w.lastPrint = now
		out = append(out, msg)
	}
	if stallable && now.Sub(w.lastChange) >= w.stallAfter &&
		(w.lastStall.IsZero() || now.Sub(w.lastStall) >= w.stallAfter) {
		w.lastStall = now
		out = append(out, fmt.Sprintf("no delivery progress for %s, check the warp-courier service",
			now.Sub(w.lastChange).Truncate(time.Minute)))
	}
	return out
}

// notConvergedError is the expected waiting state (initiates landed, ack not
// observed yet), not a failure: status is the short changed-state line the
// waitPrinter shows, Error() keeps the full per-slot detail for the final
// timeout message.
type notConvergedError struct{ status, detail string }

func (e *notConvergedError) Error() string { return e.detail }

// shortAddr abbreviates a 0x-hex address for the one-line banner; the full
// address is in network.env and `status` output.
func shortAddr(hex string) string {
	if len(hex) <= 12 {
		return hex
	}
	return hex[:6] + ".." + hex[len(hex)-4:]
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
	targets  []stakingTarget
	intents  []MachineIntent // for the raise-gate's fleet health probe (node RPCs only)
}

// The whole seesaw runs off the network.env-configured per-chain RPC, never
// touching rate-limited default endpoints unless configured to.

// cchainRPCURL is where every ValidatorManager eth_call and initiate tx goes.
func cchainRPCURL() string {
	return netcfg.Get().CChainRPC
}

// newWeightEngine wires the C-chain client and the slot -> validationID
// mapping.
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
		intents:  intents,
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

	// The whole sequence is retried: each pass re-observes everything, so a
	// transient failure just repeats the remaining work. Once the contract
	// matches desired, every further attempt is a pure poll waiting for the
	// courier to deliver to the P-chain and ack back. Re-run the
	// `fleet weight` command to resume past the timeout.
	//
	// Engine creation is INSIDE the retry loop: its first RPC (eth_chainId in
	// valmgr.Dial) crashed the whole command on a transient 429 from a
	// rate-limited public C-chain endpoint (Cloudflare 1015 on
	// api.avax.network, 2026-07-11), aborting every scenario in the torture
	// loop within a second of starting. A config error (bad key, bad subnet)
	// repeats visibly in the attempt lines instead; Ctrl-C it.
	var e *weightEngine
	var lastErr error
	w := &waitPrinter{heartbeat: 30 * time.Second, stallAfter: 2 * time.Minute}
	for attempt := 1; attempt <= weightConvergeAttempts; attempt++ {
		if attempt > 1 {
			msg, stallable := lastErr.Error(), false
			var nc *notConvergedError
			if errors.As(lastErr, &nc) {
				msg, stallable = nc.status, true
			}
			for _, l := range w.tick(msg, time.Now(), stallable) {
				fmt.Printf("  weights: %s\n", l)
			}
			select {
			case <-ctx.Done():
				fatalf("weights: %v (last: %v; re-run the `fleet weight` command to resume; every step is idempotent; if the initiates landed, check the warp-courier daemon)", ctx.Err(), lastErr)
			case <-time.After(delayForAttempt(attempt)):
			}
		}
		if e == nil {
			var err error
			if e, err = newWeightEngine(ctx, cfg, intents); err != nil {
				lastErr = err
				e = nil
				continue
			}
			fmt.Printf("[3/3] weights: reconciling via ValidatorManager %s\n", shortAddr(e.cli.Manager.Hex()))
		}
		if lastErr = e.converge(ctx); lastErr == nil {
			fmt.Println("  weights: converged")
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
		// Raise-gate: never initiate a weight RAISE for a node that is not
		// serving at the fleet tip. A behind or unreachable node given more
		// stake wins proposer slots on stale heights and can self-finalize a
		// sibling block (the documented fork wedge); the raise stays deferred,
		// re-checked on every retry, and fires as soon as the node is near tip.
		// Lowers are NEVER gated: taking weight off a sick node is the failover
		// direction and must always work. Health is probed over the fleet's own
		// node RPCs (checkHealth); this flow still never touches the P-chain.
		health := e.cfg.checkHealth(e.intents)
		targets, deferred := gateRaises(e.targets, current, health, e.cfg.topo)
		steps := planSeesaw(targets, current, total)
		if len(steps) == 0 {
			if len(deferred) > 0 {
				return fmt.Errorf("raise(s) deferred: %s", strings.Join(deferred, "; "))
			}
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

// raiseGated is THE raise-gate decision, pure for testing: a transition to a
// HIGHER weight is deferred unless the node is SERVING (which after
// markCatchingUp means at the fleet tip, within catchUpThreshold). CATCHING UP,
// BOOTSTRAPPING, and DOWN (including unreachable/cordoned) all gate the raise.
// Lower-or-equal transitions are never gated: shedding weight off a sick node
// is the failover direction.
func raiseGated(current, desired uint64, state nodeHealth) bool {
	return desired > current && state != healthServing
}

// gateRaises returns a copy of targets with every gated raise neutralized
// (desired set to the observed current weight, so planSeesaw plans nothing for
// it this round) plus one human line per deferral. The gated raise is retried
// on the next convergence pass with fresh health.
func gateRaises(targets []stakingTarget, current map[ids.ID]uint64, health []healthResult, t Topology) ([]stakingTarget, []string) {
	out := make([]stakingTarget, len(targets))
	var deferred []string
	for i, tg := range targets {
		out[i] = tg
		h := health[tg.slot]
		if raiseGated(current[tg.validationID], tg.desired, h.state) {
			out[i].desired = current[tg.validationID]
			// No block number here: the deferral text stays stable while the
			// node catches up, so the change-gated printer shows it once per
			// state change instead of every probe.
			deferred = append(deferred, fmt.Sprintf(
				"%s is %s: raise %d -> %d deferred until it is near tip",
				t.MachineName(tg.slot), h.state, current[tg.validationID], tg.desired))
		}
	}
	return out, deferred
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

// slotConverged is THE convergence predicate, per slot, from contract state
// alone: the weight matches desired AND every emitted weight update has been
// acked back (sentNonce == receivedNonce). The receivedNonce only advances
// when the warp courier delivers the update to the P-chain and posts the
// P-chain-signed ack, so nonce equality IS the proof the P-chain applied it;
// no P-chain read is needed (or made) anywhere in this flow.
func slotConverged(v valmgr.Validator, desired uint64) bool {
	return v.Weight == desired && v.SentNonce == v.ReceivedNonce
}

// verifyConverged re-reads the contract and demands slotConverged for every
// slot. A pending slot while the weight already matches desired means the
// warp courier has not delivered/acked yet; that is returned as a
// notConvergedError so the waitPrinter shows a short waiting line and only
// the final timeout error carries the full detail.
func (e *weightEngine) verifyConverged(ctx context.Context) error {
	var short, detail []string
	for _, t := range e.targets {
		v, err := e.cli.GetValidator(ctx, t.validationID)
		if err != nil {
			return err
		}
		if !slotConverged(v, t.desired) {
			detail = append(detail, fmt.Sprintf("%s desired=%d contract=%d nonces=%d/%d",
				e.slotName(t), t.desired, v.Weight, v.ReceivedNonce, v.SentNonce))
			s := fmt.Sprintf("%s nonces %d/%d", e.slotName(t), v.ReceivedNonce, v.SentNonce)
			if v.Weight != t.desired {
				s = fmt.Sprintf("%s contract %d of %d, nonces %d/%d",
					e.slotName(t), v.Weight, t.desired, v.ReceivedNonce, v.SentNonce)
			}
			short = append(short, s)
		}
	}
	if len(detail) > 0 {
		return &notConvergedError{
			status: "waiting for delivery, " + strings.Join(short, ", "),
			detail: "not converged: " + strings.Join(detail, "; "),
		}
	}
	return nil
}

func (e *weightEngine) slotName(t stakingTarget) string {
	return e.cfg.topo.MachineName(t.slot)
}

// fetchContractValidators reads every staking slot's CURRENT ValidatorManager
// state in one pass (slot index -> contract Validator). This is the single
// batch of on-chain reads a status invocation makes, all C-chain eth_calls,
// zero P-chain: reportHealth and weightsReport both consume the result. Any
// failure (RPC flaky, MANAGER_ADDRESS unset) returns a nil map and the
// reason; callers degrade to desired-weight display, never crash.
func fetchContractValidators(cfg *config, intents []MachineIntent) (map[int]valmgr.Validator, error) {
	managerHex := os.Getenv("MANAGER_ADDRESS")
	if managerHex == "" {
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
	cli, err := valmgr.DialReader(ctx, cchainRPCURL(), ethcommon.HexToAddress(managerHex))
	if err != nil {
		return nil, err
	}
	actual := make(map[int]valmgr.Validator, len(targets))
	for _, t := range targets {
		v, err := cli.GetValidator(ctx, t.validationID)
		if err != nil {
			return nil, fmt.Errorf("getValidator(%s): %w", cfg.topo.MachineName(t.slot), err)
		}
		actual[t.slot] = v
	}
	return actual, nil
}

// contractWeights projects the fetched contract state down to the
// slot -> weight map the stake-tier display consumes. nil in, nil out.
func contractWeights(vals map[int]valmgr.Validator) map[int]uint64 {
	if vals == nil {
		return nil
	}
	w := make(map[int]uint64, len(vals))
	for s, v := range vals {
		w[s] = v.Weight
	}
	return w
}

// weightsReport prints one "weights: converged|pending" line for `status`,
// judged purely from contract state: every slot must pass slotConverged
// (weight == desired, sentNonce == receivedNonce). Best-effort: a fetch error
// is reported as a note rather than failing the health snapshot.
//
// health is the same snapshot reportHealth just printed: a slot whose raise
// the raise-gate is deferring (desired > contract while the node is not
// SERVING) says so explicitly, so an operator watching a PENDING table sees
// WHY nothing moves instead of suspecting the courier (2026-07-11: 36 min of
// unexplained PENDING while `weight` was correctly deferring raises for down
// nodes).
func weightsReport(cfg *config, intents []MachineIntent, vals map[int]valmgr.Validator, fetchErr error, health []healthResult) {
	if fetchErr != nil {
		fmt.Printf("weights: %v\n", fetchErr)
		return
	}
	converged := true
	var lines []string
	for _, s := range cfg.topo.StakingSlots() {
		v := vals[s]
		mark := ""
		if !slotConverged(v, intents[s].Weight) {
			mark = "  <- PENDING"
			if raiseGated(v.Weight, intents[s].Weight, health[s].state) {
				mark = fmt.Sprintf("  <- raise deferred: node is %s, fires once it is near tip", health[s].state)
			}
			converged = false
		}
		lines = append(lines, fmt.Sprintf("  %-3s desired=%-10d contract=%-10d nonces=%d/%d%s",
			cfg.topo.MachineName(s), intents[s].Weight, v.Weight, v.ReceivedNonce, v.SentNonce, mark))
	}
	if converged {
		fmt.Println("weights: converged (contract weight == desired, sentNonce == receivedNonce)")
		return
	}
	fmt.Println("weights: PENDING - initiates are fired by `./fleet weight`; P-chain delivery and the contract ack are the warp-courier daemon's job (check it if this persists)")
	for _, l := range lines {
		fmt.Println(l)
	}
}
