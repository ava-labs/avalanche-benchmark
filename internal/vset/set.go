// Package vset is the shared view of our L1's registered validator set: the
// on-chain side (platform.getCurrentValidators + platform.getL1Validator,
// joined per validator) and the local side (the staking/node-ids.env manifest
// and the staking/l1/<key> identity directories). cmd/l1 signs and moves
// weights from it, cmd/reconcile and cmd/fuji-wallet only read it.
package vset

import (
	"context"
	"fmt"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/vms/platformvm"
)

// Validator is one registered L1 validator, joined across
// platform.getCurrentValidators (membership, validationID) and
// platform.getL1Validator (live weight, balance, minNonce, BLS key).
type Validator struct {
	NodeID       ids.NodeID
	ValidationID ids.ID
	Weight       uint64
	Balance      uint64 // remaining continuous-fee balance, nAVAX
	MinNonce     uint64
	PublicKey    *bls.PublicKey
}

// Active mirrors the P-chain's L1Validator.IsActive (Weight != 0 &&
// EndAccumulatedFee != 0): a registered validator whose continuous-fee balance
// has drained to 0 is INACTIVE. getL1Validator ALWAYS returns the stored BLS
// key regardless of activity, so PublicKey != nil is NOT the activity signal;
// the drained-balance test is. An inactive validator keeps its weight in the
// total but the P-chain drops its key from the canonical signer set (see
// WarpSet), so it can neither sign nor be indexed in a warp bitset.
func (v Validator) Active() bool { return v.Weight != 0 && v.Balance != 0 }

// fetchRetries x fetchDelay bounds how long Fetch waits out a stale read from
// a load-balanced public API (verified live: right after a conversion one
// backend transiently served a 0-validator set).
const (
	fetchRetries = 5
	fetchDelay   = 2 * time.Second
)

// Fetch reads the registered validator set fresh from the P-chain. A result
// with fewer than min validators (or a transient RPC error) is retried a few
// times before failing: the public APIs are load-balanced and individual
// backends can serve stale state.
//
// The BLS keys deliberately do NOT come from getCurrentValidators: for L1
// validators the API returns the key in a top-level publicKey field that the
// avalanchego client type drops (Signer is always nil), so each validator's
// key, live weight, balance and minNonce are read from getL1Validator.
func Fetch(ctx context.Context, pc *platformvm.Client, subnetID ids.ID, min int) ([]Validator, error) {
	var lastErr error
	for attempt := 0; attempt < fetchRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(fetchDelay)
		}
		vs, err := pc.GetCurrentValidators(ctx, subnetID, nil)
		if err != nil {
			lastErr = fmt.Errorf("platform.getCurrentValidators: %w", err)
			continue
		}
		if len(vs) < min {
			lastErr = fmt.Errorf("platform.getCurrentValidators returned %d validators, want >= %d (stale load-balanced read?)", len(vs), min)
			continue
		}
		out := make([]Validator, 0, len(vs))
		for _, v := range vs {
			if v.ValidationID == nil {
				return nil, fmt.Errorf("validator %s has no validationID (not an L1 validator?)", v.NodeID)
			}
			l1v, _, err := pc.GetL1Validator(ctx, *v.ValidationID)
			if err != nil {
				return nil, fmt.Errorf("platform.getL1Validator(%s): %w", *v.ValidationID, err)
			}
			out = append(out, Validator{
				NodeID:       v.NodeID,
				ValidationID: *v.ValidationID,
				Weight:       l1v.Weight,
				Balance:      l1v.Balance,
				MinNonce:     l1v.MinNonce,
				PublicKey:    l1v.PublicKey,
			})
		}
		return out, nil
	}
	return nil, lastErr
}

// WarpSet flattens the fetched validators into the canonical warp set,
// exactly as the P-chain will at tx-verification time.
//
// It mirrors the P-chain's effectivePublicKey: an INACTIVE validator (drained
// continuous-fee balance) is handed to FlattenValidatorSet with a nil public
// key, so its weight still counts toward TotalWeight (the quorum denominator)
// but it is dropped from the indexed signer list. Passing its live key instead
// would build a bitset with more indices than the verifier's filtered set and
// get the tx rejected ("NumIndices >= NumFilteredValidators"). getL1Validator
// returns the key for active and inactive validators alike, so the filtering
// has to happen here.
func WarpSet(vs []Validator) (validators.WarpSet, error) {
	m := make(map[ids.NodeID]*validators.GetValidatorOutput, len(vs))
	for _, v := range vs {
		pk := v.PublicKey
		if !v.Active() {
			pk = nil
		}
		m[v.NodeID] = &validators.GetValidatorOutput{
			NodeID:    v.NodeID,
			PublicKey: pk,
			Weight:    v.Weight,
		}
	}
	return validators.FlattenValidatorSet(m)
}
