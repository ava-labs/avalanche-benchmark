package oraclerelay

import (
	"context"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	platformapi "github.com/ava-labs/avalanchego/vms/platformvm/api"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
)

// Quorum numerator/denominator for local verification, matching setweight:
// verifying the aggregate before submitting is the same fail-fast ethos.
const (
	quorumNumerator   = 67
	quorumDenominator = 100
)

// validatorState adapts platformvm.Client to warp.ValidatorState so we can fetch
// the canonical oracle validator set at the exact P-chain height the main chain's
// Warp verifier will use.
type validatorState struct {
	client *platformvm.Client
}

func (v validatorState) GetValidatorSet(ctx context.Context, height uint64, subnetID ids.ID) (map[ids.NodeID]*validators.GetValidatorOutput, error) {
	return v.client.GetValidatorsAt(ctx, subnetID, platformapi.Height(height))
}

// canonicalSet returns the oracle subnet's canonical Warp validator set at
// pChainHeight. Bit positions in the aggregated signature index into this
// ordering, so it must match the verifier's view exactly.
func canonicalSet(ctx context.Context, pChain *platformvm.Client, pChainHeight uint64, subnetID ids.ID) (validators.WarpSet, error) {
	return warp.GetCanonicalValidatorSetFromSubnetID(ctx, validatorState{client: pChain}, pChainHeight, subnetID)
}
