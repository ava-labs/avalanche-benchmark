package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/api/info"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	pwallet "github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	ethcommon "github.com/ava-labs/libevm/common"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/fujikey"
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
//	                               cap with churnPeriodSeconds=0)
//	pchain.weight != contract   -> aggregate the FINAL (highest-nonce) warp
//	                               message, deliver one SetL1ValidatorWeightTx
//	                               (P-chain nonce skipping collapses the
//	                               ratchet intermediates)
//	receivedNonce < sentNonce   -> aggregate the P-chain ack, complete on the
//	                               contract (bookkeeping, keeps contract state
//	                               equal to P-chain state for observation)
//
// Raises are ordered before lowers in every phase, so the fleet never passes
// through a low-total-weight window: liveness is preserved mid-seesaw.

const weightRounds = 100 // initiate-step bound; a full DC seesaw needs ~10

const (
	// weightConverge* pace the outer retry of the whole converge sequence. The
	// binding constraint is Fuji primary-network signature coverage climbing
	// past the 67% quorum as a fresh C-chain warp message propagates (minutes),
	// so retry for ~20 min before deferring to a manual `reconcile apply`.
	weightConvergeAttempts = 20
	weightRetryBackoff     = 60 * time.Second
	weightConvergeTimeout  = 40 * time.Minute
)

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
	cfg       *config
	subnetID  ids.ID
	cChainID  ids.ID
	cli       *valmgr.Client
	pClient   *platformvm.Client
	pchainURI string
	kc        *secp256k1fx.Keychain
	wallet    pwallet.Wallet // built lazily; only P-chain deliveries need it
	targets   []stakingTarget
}

// pchainURI returns the public Fuji API base (PCHAIN_API override). The
// fleet's own RPC tier is follow-only and can never serve platform.*.
func pchainURI() string {
	return envOr("PCHAIN_API", "https://api.avax-test.network")
}

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
	uri := pchainURI()
	cli, err := valmgr.Dial(ctx, uri+"/ext/bc/C/rpc", key, ethcommon.HexToAddress(managerHex))
	if err != nil {
		return nil, err
	}
	cChainID, err := info.NewClient(uri).GetBlockchainID(ctx, "C")
	if err != nil {
		return nil, fmt.Errorf("info.getBlockchainID(C): %w", err)
	}

	e := &weightEngine{
		cfg:       cfg,
		subnetID:  subnetID,
		cChainID:  cChainID,
		cli:       cli,
		pClient:   platformvm.NewClient(uri),
		pchainURI: uri,
		kc:        secp256k1fx.NewKeychain(key),
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
// every state-changing command after the process passes; also runnable alone
// via `reconcile apply`. Idempotent and resumable at any point.
func reconcileWeights(cfg *config, intents []MachineIntent) {
	if os.Getenv("MANAGER_ADDRESS") == "" {
		fmt.Println("[3/3] weights: SKIPPED — MANAGER_ADDRESS not set (pre-manager deploy; on-chain weights are immutable)")
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
	// propagates. So we retry patiently with a long backoff, not seconds — the
	// chain stays healthy on the current weights throughout (delivery only moves
	// weight once it lands). Re-run `reconcile apply` to resume past the timeout.
	var lastErr error
	for attempt := 1; attempt <= weightConvergeAttempts; attempt++ {
		if attempt > 1 {
			fmt.Printf("  weights: attempt %d/%d in %s (last: %v)\n",
				attempt, weightConvergeAttempts, weightRetryBackoff, lastErr)
			select {
			case <-ctx.Done():
				fatalf("weights: %v (re-run `reconcile apply` to resume; every step is idempotent)", ctx.Err())
			case <-time.After(weightRetryBackoff):
			}
		}
		if lastErr = e.converge(ctx); lastErr == nil {
			fmt.Println("  weights: converged (contract == P-chain == desired)")
			return
		}
	}
	fatalf("weights: %v (re-run `reconcile apply` to resume; every step is idempotent)", lastErr)
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

// convergeContract ratchets every validator's CONTRACT weight to desired,
// raises first, each step capped by the live churn budget.
func (e *weightEngine) convergeContract(ctx context.Context) error {
	for round := 0; round < weightRounds; round++ {
		total, err := e.cli.L1TotalWeight(ctx)
		if err != nil {
			return fmt.Errorf("l1TotalWeight: %w", err)
		}
		type step struct {
			t       stakingTarget
			current uint64
		}
		var raise, lower *step
		for _, t := range e.targets {
			v, err := e.cli.GetValidator(ctx, t.validationID)
			if err != nil {
				return fmt.Errorf("getValidator(%s): %w", e.slotName(t), err)
			}
			switch {
			case v.Weight < t.desired && raise == nil:
				raise = &step{t, v.Weight}
			case v.Weight > t.desired && lower == nil:
				lower = &step{t, v.Weight}
			}
		}
		next := raise
		if next == nil {
			next = lower
		}
		if next == nil {
			return nil // contract fully converged
		}
		to := nextWeight(next.current, next.t.desired, total)
		fmt.Printf("  weights: %s initiate %d -> %d (target %d, churn budget %d)\n",
			e.slotName(next.t), next.current, to, next.t.desired, total/5)
		if err := e.cli.InitiateWeightUpdate(ctx, next.t.validationID, to); err != nil {
			return fmt.Errorf("initiate %s -> %d: %w", e.slotName(next.t), to, err)
		}
	}
	return fmt.Errorf("contract weights did not converge within %d steps", weightRounds)
}

// deliverToPChain issues one SetL1ValidatorWeightTx per validator whose
// P-chain weight differs from the contract's, delivering only the FINAL
// (highest-nonce) message. Raises land before lowers.
func (e *weightEngine) deliverToPChain(ctx context.Context) error {
	type delivery struct {
		t             stakingTarget
		nonce, weight uint64
		raise         bool
	}
	var raises, lowers []delivery
	for _, t := range e.targets {
		v, err := e.cli.GetValidator(ctx, t.validationID)
		if err != nil {
			return fmt.Errorf("getValidator(%s): %w", e.slotName(t), err)
		}
		pv, _, err := e.pClient.GetL1Validator(ctx, t.validationID)
		if err != nil {
			return fmt.Errorf("platform.getL1Validator(%s): %w", e.slotName(t), err)
		}
		if pv.Weight == v.Weight {
			continue
		}
		d := delivery{t: t, nonce: v.SentNonce, weight: v.Weight, raise: v.Weight > pv.Weight}
		if d.raise {
			raises = append(raises, d)
		} else {
			lowers = append(lowers, d)
		}
	}
	for _, d := range append(raises, lowers...) {
		unsigned, err := valmgr.WeightMessage(constants.FujiID, e.cChainID, e.cli.Manager, d.t.validationID, d.nonce, d.weight)
		if err != nil {
			return err
		}
		fmt.Printf("  weights: %s deliver weight %d (nonce %d) to the P-chain\n", e.slotName(d.t), d.weight, d.nonce)
		signed, err := valmgr.Aggregate(ctx, unsigned, nil)
		if err != nil {
			return fmt.Errorf("aggregate weight message for %s: %w", e.slotName(d.t), err)
		}
		w, err := e.pWallet(ctx)
		if err != nil {
			return err
		}
		if _, err := w.IssueSetL1ValidatorWeightTx(signed.Bytes()); err != nil {
			return fmt.Errorf("SetL1ValidatorWeightTx for %s: %w", e.slotName(d.t), err)
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
		// The P-chain signs the ack only against its exact current state.
		unsigned, err := valmgr.WeightAckMessage(constants.FujiID, t.validationID, pv.MinNonce-1, pv.Weight)
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
			bad = append(bad, fmt.Sprintf("%s desired=%d contract=%d pchain=%d nonces=%d/%d",
				e.slotName(t), t.desired, v.Weight, pv.Weight, v.ReceivedNonce, v.SentNonce))
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
	w, err := primary.MakePWallet(ctx, e.pchainURI, e.kc, primary.WalletConfig{})
	if err != nil {
		return nil, fmt.Errorf("make P-chain wallet: %w", err)
	}
	e.wallet = w
	return w, nil
}

// weightsReport prints the desired vs on-chain (P-chain) weight per staking
// slot for `status`. Read-only, best-effort: any error is reported as a note
// rather than failing the health snapshot.
func weightsReport(cfg *config, intents []MachineIntent) {
	if os.Getenv("MANAGER_ADDRESS") == "" {
		fmt.Println("weights: MANAGER_ADDRESS not set (immutable pre-manager deploy)")
		return
	}
	subnetID, err := ids.FromString(cfg.subnetID)
	if err != nil {
		fmt.Printf("weights: parse SUBNET_ID: %v\n", err)
		return
	}
	targets, err := stakingTargets(cfg, subnetID, intents)
	if err != nil {
		fmt.Printf("weights: %v\n", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pClient := platformvm.NewClient(pchainURI())
	converged := true
	var lines []string
	for _, t := range targets {
		pv, _, err := pClient.GetL1Validator(ctx, t.validationID)
		if err != nil {
			fmt.Printf("weights: platform.getL1Validator(%s): %v\n", cfg.topo.MachineName(t.slot), err)
			return
		}
		mark := ""
		if pv.Weight != t.desired {
			mark = "  <- PENDING"
			converged = false
		}
		lines = append(lines, fmt.Sprintf("  %-3s desired=%-10d pchain=%-10d%s",
			cfg.topo.MachineName(t.slot), t.desired, pv.Weight, mark))
	}
	if converged {
		fmt.Println("weights: converged (P-chain == desired)")
		return
	}
	fmt.Println("weights: PENDING — run ./scripts/failover/failover.sh apply (or wait for the running reconcile)")
	for _, l := range lines {
		fmt.Println(l)
	}
}
