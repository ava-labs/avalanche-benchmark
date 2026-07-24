package oraclerelay

import (
	"testing"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/utils/set"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
)

func newTestSigner(t *testing.T) bls.Signer {
	t.Helper()
	signer, err := localsigner.New()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return signer
}

func fakeCanonicalSet(t *testing.T, signers []bls.Signer, weights []uint64) validators.WarpSet {
	t.Helper()
	set := make(map[ids.NodeID]*validators.GetValidatorOutput, len(signers))
	for i, signer := range signers {
		nodeID := ids.GenerateTestNodeID()
		set[nodeID] = &validators.GetValidatorOutput{
			NodeID:    nodeID,
			PublicKey: signer.PublicKey(),
			Weight:    weights[i],
		}
	}
	warpSet, err := validators.FlattenValidatorSet(set)
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	return warpSet
}

func canonicalIndex(warpSet validators.WarpSet, signer bls.Signer) int {
	want := bls.PublicKeyToUncompressedBytes(signer.PublicKey())
	for i, v := range warpSet.Validators {
		if string(v.PublicKeyBytes) == string(want) {
			return i
		}
	}
	return -1
}

func TestSignAndAggregateAllSigners(t *testing.T) {
	signers := []bls.Signer{newTestSigner(t), newTestSigner(t), newTestSigner(t)}
	warpSet := fakeCanonicalSet(t, signers, []uint64{100, 100, 100})

	unsigned, err := warp.NewUnsignedMessage(1, ids.GenerateTestID(), []byte("price"))
	if err != nil {
		t.Fatalf("unsigned: %v", err)
	}
	message, err := signAndAggregate(unsigned, warpSet, signers)
	if err != nil {
		t.Fatalf("signAndAggregate: %v", err)
	}
	bitSig, ok := message.Signature.(*warp.BitSetSignature)
	if !ok {
		t.Fatalf("signature type = %T, want *BitSetSignature", message.Signature)
	}
	n, err := bitSig.NumSigners()
	if err != nil {
		t.Fatalf("NumSigners: %v", err)
	}
	if n != 3 {
		t.Fatalf("NumSigners = %d, want 3", n)
	}
}

func TestSignAndAggregateSetsCanonicalBitsForSubset(t *testing.T) {
	present := []bls.Signer{newTestSigner(t), newTestSigner(t)}
	absent := newTestSigner(t)
	all := []bls.Signer{present[0], present[1], absent}
	// Present validators hold enough weight to clear the 67% quorum without the
	// absent one.
	warpSet := fakeCanonicalSet(t, all, []uint64{100, 100, 10})

	unsigned, err := warp.NewUnsignedMessage(1, ids.GenerateTestID(), []byte("price"))
	if err != nil {
		t.Fatalf("unsigned: %v", err)
	}
	// Only the present signers are handed to the relay.
	message, err := signAndAggregate(unsigned, warpSet, present)
	if err != nil {
		t.Fatalf("signAndAggregate: %v", err)
	}
	bitSig := message.Signature.(*warp.BitSetSignature)
	bits := set.BitsFromBytes(bitSig.Signers)

	wantBits := set.NewBits()
	for _, signer := range present {
		idx := canonicalIndex(warpSet, signer)
		if idx < 0 {
			t.Fatalf("present signer missing from canonical set")
		}
		wantBits.Add(idx)
	}
	if bits.String() != wantBits.String() {
		t.Fatalf("signer bits = %s, want %s", bits, wantBits)
	}
	if bits.Contains(canonicalIndex(warpSet, absent)) {
		t.Fatalf("absent signer's bit must not be set")
	}
}

func TestSignAndAggregateRejectsUnknownSigner(t *testing.T) {
	setSigners := []bls.Signer{newTestSigner(t)}
	warpSet := fakeCanonicalSet(t, setSigners, []uint64{100})
	stranger := newTestSigner(t)

	unsigned, err := warp.NewUnsignedMessage(1, ids.GenerateTestID(), []byte("price"))
	if err != nil {
		t.Fatalf("unsigned: %v", err)
	}
	if _, err := signAndAggregate(unsigned, warpSet, []bls.Signer{stranger}); err == nil {
		t.Fatal("expected error when no local key is in the canonical set")
	}
}
