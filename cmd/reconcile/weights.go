package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/keychain"
	"github.com/ava-labs/avalanchego/utils/rpc"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	pchain "github.com/ava-labs/avalanchego/wallet/chain/p"
	pbuilder "github.com/ava-labs/avalanchego/wallet/chain/p/builder"
	psigner "github.com/ava-labs/avalanchego/wallet/chain/p/signer"
	pwallet "github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	walletcommon "github.com/ava-labs/avalanchego/wallet/subnet/primary/common"
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
// through a low-total-weight window: liveness is preserved mid-seesaw. A raise
// is additionally delivered to the P-chain EAGERLY, as soon as its own contract
// ratchet finishes, so consensus gains the new site's weight per validator
// instead of after the whole seesaw.

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
	cfg      *config
	subnetID ids.ID
	cChainID ids.ID
	cli      *valmgr.Client
	pClient  *platformvm.Client
	kc       *secp256k1fx.Keychain
	wallet   pwallet.Wallet // built lazily; only P-chain deliveries need it
	targets  []stakingTarget
}

// The whole seesaw runs off publicnode's per-chain Fuji RPC, never touching the
// aggressively rate-limited api.avax-test.network (the fleet's egress IP is
// throttled there and 429s on the first hit). Both hosts are overridable.

// cchainRPCURL is where every ValidatorManager eth_call and initiate/complete
// tx goes.
func cchainRPCURL() string {
	return envOr("CCHAIN_RPC", "https://avalanche-fuji-c-chain-rpc.publicnode.com")
}

// pchainReadClient returns the platformvm client used for reads AND wallet tx
// issuance. publicnode serves the P-chain API at /ext/bc/P but NOT at /ext/P
// (the only path platformvm.NewClient builds), so construct it on the exact URL.
func pchainReadClient() *platformvm.Client {
	url := envOr("PCHAIN_RPC", "https://avalanche-fuji-p-chain-rpc.publicnode.com/ext/bc/P")
	return &platformvm.Client{Requester: rpc.NewEndpointRequester(url)}
}

// fujiCChainIDStr is the Fuji C-chain blockchain ID: the ValidatorManager's
// chain and the warp source chain. A fixed network constant, hardcoded so we
// never call info.getBlockchainID (publicnode does not serve /ext/info, and the
// official API rate-limits it).
const fujiCChainIDStr = "yH8D7ThNJkxmtkuv2jgBa4P1Rn3Qpr4pPr7QYNfcdoS6k6HWp"

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
	cChainID, err := ids.FromString(fujiCChainIDStr)
	if err != nil {
		return nil, err
	}

	e := &weightEngine{
		cfg:      cfg,
		subnetID: subnetID,
		cChainID: cChainID,
		cli:      cli,
		pClient:  pchainReadClient(),
		kc:       secp256k1fx.NewKeychain(key),
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
		// Eager delivery: the moment a RAISE reaches its target on the contract,
		// push it to the P-chain so consensus gains the weight without waiting
		// for the other validators' ratchets. Best-effort — right after the
		// initiate, Fuji signature coverage may still be under quorum; the
		// deliverToPChain phase is the retried catch-up. Lowers are never
		// delivered eagerly: a lower landing on the P-chain before a
		// still-undelivered raise would open a low-total-weight window.
		if next == raise && to == next.t.desired {
			if err := e.deliverValidator(ctx, next.t); err != nil {
				fmt.Printf("  weights: %s eager delivery deferred (%v)\n", e.slotName(next.t), err)
			}
		}
	}
	return fmt.Errorf("contract weights did not converge within %d steps", weightRounds)
}

// deliverValidator issues one SetL1ValidatorWeightTx for t if its P-chain
// weight differs from the contract's, delivering only the FINAL
// (highest-nonce) message.
func (e *weightEngine) deliverValidator(ctx context.Context, t stakingTarget) error {
	v, err := e.cli.GetValidator(ctx, t.validationID)
	if err != nil {
		return fmt.Errorf("getValidator(%s): %w", e.slotName(t), err)
	}
	pv, _, err := e.pClient.GetL1Validator(ctx, t.validationID)
	if err != nil {
		return fmt.Errorf("platform.getL1Validator(%s): %w", e.slotName(t), err)
	}
	if pv.Weight == v.Weight {
		return nil
	}
	unsigned, err := valmgr.WeightMessage(constants.FujiID, e.cChainID, e.cli.Manager, t.validationID, v.SentNonce, v.Weight)
	if err != nil {
		return err
	}
	fmt.Printf("  weights: %s deliver weight %d (nonce %d) to the P-chain\n", e.slotName(t), v.Weight, v.SentNonce)
	signed, err := valmgr.Aggregate(ctx, unsigned, nil)
	if err != nil {
		return fmt.Errorf("aggregate weight message for %s: %w", e.slotName(t), err)
	}
	w, err := e.pWallet(ctx)
	if err != nil {
		return err
	}
	if _, err := w.IssueSetL1ValidatorWeightTx(signed.Bytes()); err != nil {
		return fmt.Errorf("SetL1ValidatorWeightTx for %s: %w", e.slotName(t), err)
	}
	return nil
}

// deliverToPChain delivers every validator whose P-chain weight differs from
// the contract's. Raises land before lowers. This is the authoritative
// catch-up behind the eager per-validator deliveries in convergeContract.
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
		if pv.Weight == v.Weight {
			continue
		}
		if v.Weight > pv.Weight {
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
// serve, and the network ID is the Fuji constant. This is a faithful copy of
// MakePWallet's body with the info dependency swapped for constants.FujiID.
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
		NetworkID:         constants.FujiID,
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
	pClient := pchainReadClient()
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
	fmt.Println("weights: PENDING — re-run the ./fleet weight that set this tier (Fuji signature coverage may still be catching up)")
	for _, l := range lines {
		fmt.Println(l)
	}
}
