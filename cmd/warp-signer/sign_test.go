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
