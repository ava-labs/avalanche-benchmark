package setweight

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/utils/rpc"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
	proposerblock "github.com/ava-labs/avalanchego/vms/proposervm/block"
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

func TestValidateIdentityUsesLettersNotNodeNumbers(t *testing.T) {
	nodes := []config.Node{
		{Number: 10, Role: config.RoleValidator},
		{Number: 20, Role: config.RoleRPC},
	}
	if err := validateIdentity(nodes, "a"); err != nil {
		t.Fatalf("validator identity a: %v", err)
	}
	for _, name := range []string{"b", "c", "10"} {
		if err := validateIdentity(nodes, name); err == nil {
			t.Errorf("validateIdentity(%q) succeeded", name)
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

func TestGateManagementConversionAllowsVisibleHeight(t *testing.T) {
	epochs := &fakeEpochClient{epochs: []proposerblock.Epoch{{Number: 8, PChainHeight: 101, StartTime: 1_000}}}
	waits := 0
	nudges := 0
	err := gateManagementConversion(
		context.Background(),
		epochs,
		101,
		5*time.Minute,
		time.Unix(1_100, 0),
		func(context.Context, time.Duration) error {
			waits++
			return nil
		},
		func() (ids.ID, error) {
			nudges++
			return ids.GenerateTestID(), nil
		},
		&bytes.Buffer{},
	)
	if err != nil || waits != 0 || nudges != 0 {
		t.Fatalf("ready gate: err=%v waits=%d nudges=%d", err, waits, nudges)
	}
}

func TestFindConversionHeightUsesTimestampThenTransactionID(t *testing.T) {
	conversionTime := time.Unix(100, 0)
	target := ids.GenerateTestID()
	blocks := fakeBlockReader{blocks: make(map[uint64]fakeBlock)}
	for height := uint64(0); height <= 15; height++ {
		timestamp := time.Unix(99, 0)
		switch {
		case height >= 13:
			timestamp = time.Unix(101, 0)
		case height >= 11:
			timestamp = conversionTime
		}
		blocks.blocks[height] = fakeBlock{timestamp: timestamp}
	}
	blocks.blocks[12] = fakeBlock{timestamp: conversionTime, txIDs: []ids.ID{target}}

	height, err := findConversionHeight(context.Background(), blocks, 15, conversionTime, target)
	if err != nil {
		t.Fatal(err)
	}
	if height != 12 {
		t.Fatalf("height=%d, want 12", height)
	}
}

func TestGateManagementConversionWaitsThenContinues(t *testing.T) {
	epochs := &fakeEpochClient{epochs: []proposerblock.Epoch{
		{Number: 8, PChainHeight: 100, StartTime: 1_000},
		{Number: 9, PChainHeight: 101, StartTime: 1_300},
	}}
	var waited time.Duration
	nudges := 0
	var output bytes.Buffer
	err := gateManagementConversion(
		context.Background(),
		epochs,
		101,
		5*time.Minute,
		time.Unix(1_135, 0),
		func(_ context.Context, duration time.Duration) error {
			waited = duration
			return nil
		},
		func() (ids.ID, error) {
			nudges++
			return ids.GenerateTestID(), nil
		},
		&output,
	)
	if err != nil || waited != 2*time.Minute+45*time.Second || nudges != 1 {
		t.Fatalf("waiting gate: err=%v waited=%s nudges=%d", err, waited, nudges)
	}
	for _, expected := range []string{"sleeping for 2m45s", "1970-01-01 09:21:40 JST", "issued P-chain Warp context nudge"} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("output %q does not contain %q", output.String(), expected)
		}
	}
}

func TestGateManagementConversionUsesSecondChildBlockWhenNeeded(t *testing.T) {
	epochs := &fakeEpochClient{epochs: []proposerblock.Epoch{
		{Number: 8, PChainHeight: 100, StartTime: 1_000},
		{Number: 8, PChainHeight: 100, StartTime: 1_000},
		{Number: 9, PChainHeight: 101, StartTime: 1_300},
	}}
	waits := 0
	nudges := 0
	var output bytes.Buffer
	err := gateManagementConversion(
		context.Background(),
		epochs,
		101,
		5*time.Minute,
		time.Unix(1_301, 0),
		func(context.Context, time.Duration) error {
			waits++
			return nil
		},
		func() (ids.ID, error) {
			nudges++
			return ids.GenerateTestID(), nil
		},
		&output,
	)
	if err != nil || waits != 0 || nudges != 2 {
		t.Fatalf("sealable gate: err=%v waits=%d nudges=%d", err, waits, nudges)
	}
	if strings.Count(output.String(), "issued P-chain Warp context nudge") != 2 {
		t.Fatalf("expected two visible nudges, got %q", output.String())
	}
}

func TestGateManagementConversionStopsAfterTwoNudges(t *testing.T) {
	epochs := &fakeEpochClient{epochs: []proposerblock.Epoch{{Number: 8, PChainHeight: 100, StartTime: 1_000}}}
	nudges := 0
	err := gateManagementConversion(
		context.Background(),
		epochs,
		101,
		5*time.Minute,
		time.Unix(1_301, 0),
		func(context.Context, time.Duration) error { return nil },
		func() (ids.ID, error) {
			nudges++
			return ids.GenerateTestID(), nil
		},
		&bytes.Buffer{},
	)
	if err == nil || nudges != 2 {
		t.Fatalf("stale gate: err=%v nudges=%d", err, nudges)
	}
	if !strings.Contains(err.Error(), "after two P-chain context nudges") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSubmitOtherFailureDoesNotNudge(t *testing.T) {
	wallet := &fakeWallet{weightErr: errors.New("insufficient funds")}
	err := submitAndVerify(
		context.Background(),
		fakeClient{},
		wallet,
		testMessage(t),
		"a",
		registeredValidator{NodeID: ids.GenerateTestNodeID(), Weight: ActiveWeight},
		DeadWeight,
		&bytes.Buffer{},
	)
	if err == nil || wallet.weightCalls != 1 {
		t.Fatalf("submit failure: err=%v weightCalls=%d", err, wallet.weightCalls)
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
	weightCalls int
}

func (f *fakeWallet) IssueSetL1ValidatorWeightTx([]byte, ...commonopts.Option) (*txs.Tx, error) {
	f.weightCalls++
	return nil, f.weightErr
}

type fakeEpochClient struct {
	epochs []proposerblock.Epoch
	err    error
	reads  int
}

type fakeBlock struct {
	timestamp time.Time
	txIDs     []ids.ID
}

type fakeBlockReader struct {
	blocks map[uint64]fakeBlock
}

func (f fakeBlockReader) Block(_ context.Context, height uint64) (time.Time, []ids.ID, error) {
	block := f.blocks[height]
	return block.timestamp, block.txIDs, nil
}

func (f *fakeEpochClient) GetCurrentEpoch(context.Context, ...rpc.Option) (proposerblock.Epoch, error) {
	if f.err != nil {
		return proposerblock.Epoch{}, f.err
	}
	index := f.reads
	if index >= len(f.epochs) {
		index = len(f.epochs) - 1
	}
	f.reads++
	return f.epochs[index], nil
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
