package setweight

import (
	"errors"
	"testing"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
)

func TestValidateWeight(t *testing.T) {
	for _, weight := range []uint64{DeadWeight, SpareWeight, ActiveWeight} {
		if err := ValidateWeight(weight); err != nil {
			t.Errorf("ValidateWeight(%d): %v", weight, err)
		}
	}
	for _, weight := range []uint64{0, 2, 999, 1001, 99999, 100001} {
		if err := ValidateWeight(weight); err == nil {
			t.Errorf("ValidateWeight(%d) succeeded", weight)
		}
	}
}

func TestCanonicalWarpSetKeepsInactiveWeightInQuorum(t *testing.T) {
	activeSigner, err := localsigner.New()
	if err != nil {
		t.Fatal(err)
	}
	inactiveSigner, err := localsigner.New()
	if err != nil {
		t.Fatal(err)
	}
	set, err := canonicalWarpSet([]registeredValidator{
		{NodeID: ids.GenerateTestNodeID(), Weight: 1000, Balance: 1, PublicKey: activeSigner.PublicKey()},
		{NodeID: ids.GenerateTestNodeID(), Weight: 1000, Balance: 0, PublicKey: inactiveSigner.PublicKey()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Validators) != 1 || set.TotalWeight != 2000 {
		t.Fatalf("unexpected canonical set: validators=%d totalWeight=%d", len(set.Validators), set.TotalWeight)
	}
}

func TestSignAndAggregateEnforcesProtocolQuorum(t *testing.T) {
	signers := make([]bls.Signer, 4)
	validatorSet := make(map[ids.NodeID]*validators.GetValidatorOutput, 4)
	for i := range signers {
		signer, err := localsigner.New()
		if err != nil {
			t.Fatal(err)
		}
		signers[i] = signer
		nodeID := ids.GenerateTestNodeID()
		validatorSet[nodeID] = &validators.GetValidatorOutput{
			NodeID:    nodeID,
			PublicKey: signer.PublicKey(),
			Weight:    1000,
		}
	}
	canonical, err := validators.FlattenValidatorSet(validatorSet)
	if err != nil {
		t.Fatal(err)
	}
	unsigned, err := warp.NewUnsignedMessage(1, ids.GenerateTestID(), []byte("weight"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signAndAggregate(unsigned, canonical, signers[:3]); err != nil {
		t.Fatalf("3 of 4 must satisfy quorum: %v", err)
	}
	if _, err := signAndAggregate(unsigned, canonical, signers[:2]); !errors.Is(err, warp.ErrInsufficientWeight) {
		t.Fatalf("2 of 4 must fail with insufficient weight, got %v", err)
	}
}
