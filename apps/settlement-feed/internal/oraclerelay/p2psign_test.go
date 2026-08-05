package oraclerelay

import (
	"testing"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
)

// warpSetOf builds a canonical set from freshly generated keys, all weight 1000.
func warpSetOf(t *testing.T, count int) (validators.WarpSet, []bls.Signer) {
	t.Helper()
	warpSet := validators.WarpSet{}
	signers := make([]bls.Signer, 0, count)
	for i := 0; i < count; i++ {
		signer, err := localsigner.New()
		if err != nil {
			t.Fatal(err)
		}
		signers = append(signers, signer)
		warpSet.Validators = append(warpSet.Validators, &validators.Warp{
			PublicKey:      signer.PublicKey(),
			PublicKeyBytes: bls.PublicKeyToUncompressedBytes(signer.PublicKey()),
			Weight:         1000,
			NodeIDs:        []ids.NodeID{ids.GenerateTestNodeID()},
		})
		warpSet.TotalWeight += 1000
	}
	return warpSet, signers
}

func unsignedFixture(t *testing.T) *warp.UnsignedMessage {
	t.Helper()
	unsigned, err := warp.NewUnsignedMessage(5, ids.GenerateTestID(), []byte("price update"))
	if err != nil {
		t.Fatal(err)
	}
	return unsigned
}

func TestAggregateByIndexQuorum(t *testing.T) {
	warpSet, signers := warpSetOf(t, 4)
	unsigned := unsignedFixture(t)

	// 3 of 4 equal-weight signatures = 75% ≥ 67%: must verify.
	byIndex := make(map[int]*bls.Signature)
	for _, index := range []int{0, 2, 3} {
		signature, err := signers[index].Sign(unsigned.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		byIndex[index] = signature
	}
	if _, err := aggregateByIndex(unsigned, warpSet, byIndex); err != nil {
		t.Fatalf("3/4 quorum must verify: %v", err)
	}

	// 2 of 4 = 50% < 67%: must fail local verification.
	delete(byIndex, 3)
	if _, err := aggregateByIndex(unsigned, warpSet, byIndex); err == nil {
		t.Fatal("2/4 below quorum must fail verification")
	}
}

func TestAggregateByIndexBitOrdering(t *testing.T) {
	warpSet, signers := warpSetOf(t, 3)
	unsigned := unsignedFixture(t)

	// All three signatures, inserted out of order: canonical bit positions must
	// still verify (aggregateByIndex sorts by index).
	byIndex := make(map[int]*bls.Signature)
	for _, index := range []int{2, 0, 1} {
		signature, err := signers[index].Sign(unsigned.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		byIndex[index] = signature
	}
	signed, err := aggregateByIndex(unsigned, warpSet, byIndex)
	if err != nil {
		t.Fatal(err)
	}
	bitSet, ok := signed.Signature.(*warp.BitSetSignature)
	if !ok {
		t.Fatalf("signature type %T, want *warp.BitSetSignature", signed.Signature)
	}
	if err := bitSet.Verify(unsigned, unsigned.NetworkID, warpSet, quorumNumerator, quorumDenominator); err != nil {
		t.Fatalf("full-set aggregate must verify: %v", err)
	}
}

func TestResponseMuxRoutesAndForgets(t *testing.T) {
	mux := newResponseMux()
	replies := make(chan signatureReply, 1)
	mux.expect(7, replies)

	nodeID := ids.GenerateTestNodeID()
	mux.deliver(7, signatureReply{nodeID: nodeID, responseData: []byte("sig")})
	select {
	case reply := <-replies:
		if reply.nodeID != nodeID || string(reply.responseData) != "sig" {
			t.Fatalf("unexpected reply: %+v", reply)
		}
	default:
		t.Fatal("expected a routed reply")
	}

	// A requestID delivers exactly once; the second delivery is dropped.
	mux.deliver(7, signatureReply{nodeID: nodeID})
	select {
	case <-replies:
		t.Fatal("requestID must be forgotten after first delivery")
	default:
	}

	// forget removes a registered waiter outright.
	mux.expect(8, replies)
	mux.forget(8)
	mux.deliver(8, signatureReply{nodeID: nodeID})
	select {
	case <-replies:
		t.Fatal("forgotten requestID must not deliver")
	default:
	}
}

func TestParseStakingAddresses(t *testing.T) {
	addresses, err := ParseStakingAddresses("172.31.17.174:9651, 10.0.0.2:9653")
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 2 || addresses[0].Port() != 9651 || addresses[1].Port() != 9653 {
		t.Fatalf("unexpected addresses: %v", addresses)
	}
	for _, invalid := range []string{"", " , ", "no-port", "1.2.3.4:notaport"} {
		if _, err := ParseStakingAddresses(invalid); err == nil {
			t.Fatalf("%q must be rejected", invalid)
		}
	}
}
