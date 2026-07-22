package setweight

import (
	"errors"
	"strings"
	"testing"
	"time"

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

func TestManagementConversionTimeUsesEarliestValidator(t *testing.T) {
	convertedAt := managementConversionTime([]registeredValidator{
		{StartTime: 200},
		{StartTime: 100},
		{StartTime: 0},
	})
	if got, want := convertedAt.Unix(), int64(100); got != want {
		t.Fatalf("creation time = %d, want %d", got, want)
	}
}

func TestExplainEmptyManagementSetFresh(t *testing.T) {
	original := errors.New("unknown validator: NumIndices (0) >= NumFilteredValidators (0)")
	convertedAt := time.Date(2026, time.July, 22, 9, 0, 0, 0, time.UTC)
	err := explainEmptyManagementSet(original, convertedAt, convertedAt.Add(10*time.Second))
	for _, expected := range []string{
		"management validator set was empty",
		"2026-07-22 18:00:00 JST",
		"10s ago",
		"retry in about 20s",
		"no transaction was accepted",
	} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("error %q does not contain %q", err, expected)
		}
	}
	if !errors.Is(err, original) {
		t.Fatalf("error does not wrap original: %v", err)
	}
}

func TestExplainEmptyManagementSetOutsideCreationWindow(t *testing.T) {
	original := errors.New("unknown validator: NumIndices (0) >= NumFilteredValidators (0)")
	convertedAt := time.Date(2026, time.July, 22, 9, 0, 0, 0, time.UTC)
	err := explainEmptyManagementSet(original, convertedAt, convertedAt.Add(time.Hour))
	for _, expected := range []string{
		"2026-07-22 18:00:00 JST",
		"outside the normal 30s conversion window",
		"retry after the P-Chain safe height advances",
	} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("error %q does not contain %q", err, expected)
		}
	}
}

func TestExplainEmptyManagementSetLeavesOtherErrorsAlone(t *testing.T) {
	original := errors.New("insufficient funds")
	if got := explainEmptyManagementSet(original, time.Now(), time.Now()); got != original {
		t.Fatalf("unexpected replacement: %v", got)
	}
}
