package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
	warpmessage "github.com/ava-labs/avalanchego/vms/platformvm/warp/message"
	warppayload "github.com/ava-labs/avalanchego/vms/platformvm/warp/payload"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/vset"
)

const testNetworkID = uint32(1337)

// syntheticSet builds a canonical warp set of n freshly generated validators,
// weight 1 each, via the same FlattenValidatorSet the P-chain uses.
func syntheticSet(t *testing.T, n int) ([]bls.Signer, validators.WarpSet) {
	t.Helper()
	signers := make([]bls.Signer, n)
	m := make(map[ids.NodeID]*validators.GetValidatorOutput, n)
	for i := range signers {
		s, err := localsigner.New()
		if err != nil {
			t.Fatalf("localsigner.New: %v", err)
		}
		signers[i] = s
		nodeID := ids.GenerateTestNodeID()
		m[nodeID] = &validators.GetValidatorOutput{NodeID: nodeID, PublicKey: s.PublicKey(), Weight: 1}
	}
	warpSet, err := validators.FlattenValidatorSet(m)
	if err != nil {
		t.Fatalf("FlattenValidatorSet: %v", err)
	}
	return signers, warpSet
}

func testWeightMessage(t *testing.T) (*warp.UnsignedMessage, *warpmessage.L1ValidatorWeight, ids.ID, []byte) {
	t.Helper()
	chainID := ids.GenerateTestID()
	managerAddr := []byte{0xde, 0xad, 0xbe, 0xef, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	payload, err := warpmessage.NewL1ValidatorWeight(ids.GenerateTestID(), 7, 42)
	if err != nil {
		t.Fatalf("NewL1ValidatorWeight: %v", err)
	}
	unsigned, err := addressedCall(testNetworkID, chainID, managerAddr, payload.Bytes())
	if err != nil {
		t.Fatalf("addressedCall: %v", err)
	}
	return unsigned, payload, chainID, managerAddr
}

// TestSignAndAggregateQuorum signs an L1ValidatorWeight message with 3
// generated BLS keys against a synthetic 3-validator canonical set and
// verifies it at the P-chain's 67/100 quorum: 3 of 3 passes, 1 of 3 fails.
func TestSignAndAggregateQuorum(t *testing.T) {
	signers, warpSet := syntheticSet(t, 3)
	unsigned, _, _, _ := testWeightMessage(t)

	// 3 of 3: signAndAggregate verifies internally; verify once more
	// explicitly against the set to make the assertion visible here.
	signed, err := signAndAggregate(unsigned, warpSet, signers)
	if err != nil {
		t.Fatalf("signAndAggregate with all 3 keys: %v", err)
	}
	if err := signed.Signature.Verify(unsigned, testNetworkID, warpSet, quorumNum, quorumDen); err != nil {
		t.Fatalf("Verify 3-of-3 at %d/%d: %v", quorumNum, quorumDen, err)
	}

	// 1 of 3 (weight 1 of 3 < 67/100): must fail quorum.
	_, err = signAndAggregate(unsigned, warpSet, signers[:1])
	if !errors.Is(err, warp.ErrInsufficientWeight) {
		t.Fatalf("signAndAggregate with 1 of 3 keys: want ErrInsufficientWeight, got %v", err)
	}
}

// TestCommitteeQuorum is the manager-L1 committee model's core evidence: an
// L1ValidatorWeight message signed against a synthetic 4-equal-weight
// committee (the smallest that survives one signer loss) VERIFIES at 3-of-4
// (75% >= 67%) and FAILS at 2-of-4 (50% < 67%) with ErrInsufficientWeight -
// exactly the check the P-chain runs against the manager subnet's set.
func TestCommitteeQuorum(t *testing.T) {
	signers, committee := syntheticSet(t, 4)
	unsigned, _, _, _ := testWeightMessage(t)

	// 3 of 4: passes. signAndAggregate verifies internally; assert again here.
	signed, err := signAndAggregate(unsigned, committee, signers[:3])
	if err != nil {
		t.Fatalf("signAndAggregate with 3 of 4 committee keys: %v", err)
	}
	if err := signed.Signature.Verify(unsigned, testNetworkID, committee, quorumNum, quorumDen); err != nil {
		t.Fatalf("Verify 3-of-4 at %d/%d: %v", quorumNum, quorumDen, err)
	}

	// 2 of 4 (50% < 67%): must fail the quorum.
	if _, err := signAndAggregate(unsigned, committee, signers[:2]); !errors.Is(err, warp.ErrInsufficientWeight) {
		t.Fatalf("signAndAggregate with 2 of 4 committee keys: want ErrInsufficientWeight, got %v", err)
	}
}

// committeeVSet builds a []vset.Validator of n equal-weight (1) members with
// fresh BLS keys. The first `active` are ACTIVE (balance > 0); the rest are
// INACTIVE (balance 0), which is how a drained committee member reads from the
// P-chain: getL1Validator still returns their key, but their balance is 0.
func committeeVSet(t *testing.T, n, active int) ([]bls.Signer, []vset.Validator) {
	t.Helper()
	signers := make([]bls.Signer, n)
	vs := make([]vset.Validator, n)
	for i := range signers {
		s, err := localsigner.New()
		if err != nil {
			t.Fatalf("localsigner.New: %v", err)
		}
		signers[i] = s
		var balance uint64
		if i < active {
			balance = 1_000_000 // ACTIVE: has continuous-fee runway
		}
		vs[i] = vset.Validator{
			NodeID:    ids.GenerateTestNodeID(),
			Weight:    1,
			Balance:   balance,
			PublicKey: s.PublicKey(),
		}
	}
	return signers, vs
}

// TestCommitteeQuorumWithInactiveMember is the drained-committee-member
// regression (the release-blocker): a 4-registered committee with one INACTIVE
// (balance-0) member. The P-chain excludes the inactive member from the signer
// list (nil key) but keeps its weight in the total, so vset.WarpSet must build
// a bitset over the FILTERED 3 while the quorum denominator stays 4. Signing
// with all keys we hold then VERIFIES iff active weight >= 67% of the total:
// 3-of-4 (75%) passes, 2-of-4 (50%) fails with ErrInsufficientWeight.
func TestCommitteeQuorumWithInactiveMember(t *testing.T) {
	unsigned, _, _, _ := testWeightMessage(t)

	// 4 registered, 3 active. WarpSet drops the drained member from the indexed
	// set but keeps its weight in TotalWeight.
	signers, vs := committeeVSet(t, 4, 3)
	warpSet, err := vset.WarpSet(vs)
	if err != nil {
		t.Fatalf("vset.WarpSet: %v", err)
	}
	if got := len(warpSet.Validators); got != 3 {
		t.Fatalf("filtered signer set: got %d validators, want 3 (inactive member must be excluded)", got)
	}
	if warpSet.TotalWeight != 4 {
		t.Fatalf("TotalWeight: got %d, want 4 (inactive member's weight must stay in the denominator)", warpSet.TotalWeight)
	}

	// We hold every committee key on disk (including the inactive member's), so
	// pass them all: signAndAggregate signs only those in the filtered set.
	signed, err := signAndAggregate(unsigned, warpSet, signers)
	if err != nil {
		t.Fatalf("signAndAggregate 3-active-of-4: %v", err)
	}
	if err := signed.Signature.Verify(unsigned, testNetworkID, warpSet, quorumNum, quorumDen); err != nil {
		t.Fatalf("Verify 3-active-of-4 at %d/%d: %v", quorumNum, quorumDen, err)
	}

	// 2 active of 4 (50% < 67%): the inactive members' retained weight dilutes
	// the quorum below threshold, so signing must FAIL.
	signers2, vs2 := committeeVSet(t, 4, 2)
	warpSet2, err := vset.WarpSet(vs2)
	if err != nil {
		t.Fatalf("vset.WarpSet: %v", err)
	}
	if got := len(warpSet2.Validators); got != 2 {
		t.Fatalf("filtered signer set: got %d validators, want 2", got)
	}
	if _, err := signAndAggregate(unsigned, warpSet2, signers2); !errors.Is(err, warp.ErrInsufficientWeight) {
		t.Fatalf("signAndAggregate 2-active-of-4: want ErrInsufficientWeight, got %v", err)
	}
}

// TestWeightMessageRoundTrip re-parses the built message bytes through
// avalanchego's own parsers and asserts every field survives.
func TestWeightMessageRoundTrip(t *testing.T) {
	unsigned, payload, chainID, managerAddr := testWeightMessage(t)

	parsedUnsigned, err := warp.ParseUnsignedMessage(unsigned.Bytes())
	if err != nil {
		t.Fatalf("ParseUnsignedMessage: %v", err)
	}
	if parsedUnsigned.NetworkID != testNetworkID {
		t.Errorf("NetworkID: got %d, want %d", parsedUnsigned.NetworkID, testNetworkID)
	}
	if parsedUnsigned.SourceChainID != chainID {
		t.Errorf("SourceChainID: got %s, want %s", parsedUnsigned.SourceChainID, chainID)
	}

	call, err := warppayload.ParseAddressedCall(parsedUnsigned.Payload)
	if err != nil {
		t.Fatalf("ParseAddressedCall: %v", err)
	}
	if !bytes.Equal(call.SourceAddress, managerAddr) {
		t.Errorf("SourceAddress: got %x, want %x", call.SourceAddress, managerAddr)
	}

	parsedWeight, err := warpmessage.ParseL1ValidatorWeight(call.Payload)
	if err != nil {
		t.Fatalf("ParseL1ValidatorWeight: %v", err)
	}
	if parsedWeight.ValidationID != payload.ValidationID {
		t.Errorf("ValidationID: got %s, want %s", parsedWeight.ValidationID, payload.ValidationID)
	}
	if parsedWeight.Nonce != payload.Nonce {
		t.Errorf("Nonce: got %d, want %d", parsedWeight.Nonce, payload.Nonce)
	}
	if parsedWeight.Weight != payload.Weight {
		t.Errorf("Weight: got %d, want %d", parsedWeight.Weight, payload.Weight)
	}
}
