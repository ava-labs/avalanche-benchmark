package setweight

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/utils/rpc"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
	commonopts "github.com/ava-labs/avalanchego/wallet/subnet/primary/common"
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

func TestSubmitEmptyManagementSetNudgesOnceAndRequiresManualRetry(t *testing.T) {
	original := errors.New("unknown validator: NumIndices (0) >= NumFilteredValidators (0)")
	nudgeID := ids.GenerateTestID()
	wallet := &fakeWallet{
		weightErr: original,
		baseTx:    &txs.Tx{TxID: nudgeID},
	}
	var output bytes.Buffer
	err := submitAndVerify(
		context.Background(),
		fakeClient{},
		wallet,
		testMessage(t),
		1,
		registeredValidator{NodeID: ids.GenerateTestNodeID(), Weight: ActiveWeight},
		DeadWeight,
		&output,
	)
	if err == nil {
		t.Fatal("expected visible failure after nudge")
	}
	if wallet.weightCalls != 1 || wallet.baseCalls != 1 {
		t.Fatalf("calls: weight=%d base=%d, want one each", wallet.weightCalls, wallet.baseCalls)
	}
	for _, expected := range []string{"weight transaction was not accepted", "rerun set-weight", nudgeID.String()} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("error %q does not contain %q", err, expected)
		}
	}
	if !strings.Contains(output.String(), nudgeID.String()) {
		t.Fatalf("nudge transaction is not printed: %q", output.String())
	}
	if !errors.Is(err, original) {
		t.Fatalf("error does not wrap original rejection: %v", err)
	}
}

func TestSubmitOtherFailureDoesNotNudge(t *testing.T) {
	wallet := &fakeWallet{weightErr: errors.New("insufficient funds")}
	err := submitAndVerify(
		context.Background(),
		fakeClient{},
		wallet,
		testMessage(t),
		1,
		registeredValidator{NodeID: ids.GenerateTestNodeID(), Weight: ActiveWeight},
		DeadWeight,
		&bytes.Buffer{},
	)
	if err == nil || wallet.baseCalls != 0 {
		t.Fatalf("unrelated failure must not nudge: err=%v baseCalls=%d", err, wallet.baseCalls)
	}
}

func testMessage(t *testing.T) *warp.Message {
	t.Helper()
	unsigned, err := warp.NewUnsignedMessage(1, ids.GenerateTestID(), []byte("test"))
	if err != nil {
		t.Fatal(err)
	}
	message, err := warp.NewMessage(unsigned, &warp.BitSetSignature{})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

type fakeWallet struct {
	weightErr   error
	baseErr     error
	baseTx      *txs.Tx
	weightCalls int
	baseCalls   int
}

func (f *fakeWallet) IssueSetL1ValidatorWeightTx([]byte, ...commonopts.Option) (*txs.Tx, error) {
	f.weightCalls++
	return nil, f.weightErr
}

func (f *fakeWallet) IssueBaseTx([]*avax.TransferableOutput, ...commonopts.Option) (*txs.Tx, error) {
	f.baseCalls++
	return f.baseTx, f.baseErr
}

type fakeClient struct{}

func (fakeClient) GetCurrentValidators(context.Context, ids.ID, []ids.NodeID, ...rpc.Option) ([]platformvm.ClientPermissionlessValidator, error) {
	return nil, nil
}

func (fakeClient) GetHeight(context.Context, ...rpc.Option) (uint64, error) {
	return 0, nil
}

func (fakeClient) GetL1Validator(context.Context, ids.ID, ...rpc.Option) (platformvm.L1Validator, uint64, error) {
	return platformvm.L1Validator{}, 0, nil
}
