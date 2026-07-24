package oraclerelay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/utils/set"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	platformapi "github.com/ava-labs/avalanchego/vms/platformvm/api"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
)

// Quorum numerator/denominator for local verification, matching setweight: we
// hold 100% of the oracle weight so this always passes, but verifying locally
// before submitting is the same fail-fast ethos.
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

// signAndAggregate signs the unsigned message with every local signer that
// appears in the canonical set, sets the matching canonical bits, aggregates in
// canonical order, and verifies locally before returning. Lifted from
// setweight.signAndAggregate (unexported there); keep the two in sync.
func signAndAggregate(unsigned *warp.UnsignedMessage, validatorSet validators.WarpSet, signers []bls.Signer) (*warp.Message, error) {
	byPublicKey := make(map[string]bls.Signer, len(signers))
	for _, signer := range signers {
		byPublicKey[string(bls.PublicKeyToUncompressedBytes(signer.PublicKey()))] = signer
	}

	signerBits := set.NewBits()
	signatures := make([]*bls.Signature, 0, len(signers))
	for index, validator := range validatorSet.Validators {
		signer, ok := byPublicKey[string(validator.PublicKeyBytes)]
		if !ok {
			continue
		}
		signature, err := signer.Sign(unsigned.Bytes())
		if err != nil {
			return nil, fmt.Errorf("sign Warp message: %w", err)
		}
		signatures = append(signatures, signature)
		signerBits.Add(index)
	}
	if len(signatures) == 0 {
		return nil, fmt.Errorf("none of the %d local oracle keys are in the %d-validator canonical signing set", len(signers), len(validatorSet.Validators))
	}
	aggregated, err := bls.AggregateSignatures(signatures)
	if err != nil {
		return nil, fmt.Errorf("aggregate Warp signatures: %w", err)
	}
	bitSetSignature := &warp.BitSetSignature{Signers: signerBits.Bytes()}
	copy(bitSetSignature.Signature[:], bls.SignatureToBytes(aggregated))
	if err := bitSetSignature.Verify(unsigned, unsigned.NetworkID, validatorSet, quorumNumerator, quorumDenominator); err != nil {
		return nil, fmt.Errorf("local oracle quorum verification: %w", err)
	}
	message, err := warp.NewMessage(unsigned, bitSetSignature)
	if err != nil {
		return nil, fmt.Errorf("build signed Warp message: %w", err)
	}
	return message, nil
}

// loadOracleSigners loads the BLS secret of every oracle-validator identity from
// deployment/identities/<letter>/signer.key. Control holds all keys by design;
// the oracle nodes never sign their own Warp messages.
func loadOracleSigners(deploymentDirectory string, public creation.Public) ([]bls.Signer, error) {
	signers := make([]bls.Signer, 0)
	for _, node := range public.Nodes {
		if node.Role != config.RoleOracleValidator {
			continue
		}
		if _, err := identity.Index(node.Identity); err != nil {
			return nil, fmt.Errorf("oracle identity %s: %w", node.Identity, err)
		}
		path := filepath.Join(deploymentDirectory, "identities", node.Identity, "signer.key")
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read oracle signer %s: %w", path, err)
		}
		signer, err := localsigner.FromBytes(contents)
		if err != nil {
			return nil, fmt.Errorf("load oracle signer %s: %w", path, err)
		}
		signers = append(signers, signer)
	}
	if len(signers) == 0 {
		return nil, fmt.Errorf("no oracle-validator identities found in %s", filepath.Join(deploymentDirectory, "public.json"))
	}
	return signers, nil
}
