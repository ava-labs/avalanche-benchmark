package oraclerelay

import (
	"math/big"
	"testing"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
	warppayload "github.com/ava-labs/avalanchego/vms/platformvm/warp/payload"
	ethcommon "github.com/ava-labs/libevm/common"
)

// buildEventData reproduces what the Warp precompile writes to a SendWarpMessage
// log: the unsigned message bytes ABI-encoded as a single dynamic `bytes`.
func buildEventData(t *testing.T, source ethcommon.Address, sub Submission) ([]byte, []byte) {
	t.Helper()
	payload := make([]byte, 0, 128)
	payload = append(payload, sub.AssetID.Bytes()...)
	payload = append(payload, leftPad32(sub.Price.Bytes())...)
	payload = append(payload, leftPad32(sub.UpdatedAt.Bytes())...)
	payload = append(payload, leftPad32(sub.Seq.Bytes())...)

	call, err := warppayload.NewAddressedCall(source.Bytes(), payload)
	if err != nil {
		t.Fatalf("addressed call: %v", err)
	}
	unsigned, err := warp.NewUnsignedMessage(1, ids.GenerateTestID(), call.Bytes())
	if err != nil {
		t.Fatalf("unsigned message: %v", err)
	}
	msgBytes := unsigned.Bytes()

	data := make([]byte, 0, 64+len(msgBytes))
	data = append(data, leftPad32(big.NewInt(32).Bytes())...)
	data = append(data, leftPad32(big.NewInt(int64(len(msgBytes))).Bytes())...)
	data = append(data, msgBytes...)
	if rem := len(data) % 32; rem != 0 {
		data = append(data, make([]byte, 32-rem)...)
	}
	return data, msgBytes
}

func TestUnpackEventMessageAndParseSubmission(t *testing.T) {
	want := Submission{
		AssetID:   assetBTC,
		Price:     big.NewInt(6000012345678),
		UpdatedAt: big.NewInt(1_753_000_000),
		Seq:       big.NewInt(42),
	}
	source := ethcommon.HexToAddress("0x000000000000000000000000000000000000FEED")
	data, msgBytes := buildEventData(t, source, want)

	got, err := UnpackEventMessage(data)
	if err != nil {
		t.Fatalf("unpack event message: %v", err)
	}
	if string(got) != string(msgBytes) {
		t.Fatalf("event message bytes mismatch:\n got %x\nwant %x", got, msgBytes)
	}

	sub, err := ParseSubmission(got)
	if err != nil {
		t.Fatalf("parse submission: %v", err)
	}
	if sub.AssetID != want.AssetID {
		t.Fatalf("assetID = %s, want %s", sub.AssetID.Hex(), want.AssetID.Hex())
	}
	if sub.Price.Cmp(want.Price) != 0 {
		t.Fatalf("price = %s, want %s", sub.Price, want.Price)
	}
	if sub.UpdatedAt.Cmp(want.UpdatedAt) != 0 {
		t.Fatalf("updatedAt = %s, want %s", sub.UpdatedAt, want.UpdatedAt)
	}
	if sub.Seq.Cmp(want.Seq) != 0 {
		t.Fatalf("seq = %s, want %s", sub.Seq, want.Seq)
	}
}

func TestDecodePricePayloadRejectsThreeWord(t *testing.T) {
	threeWord := make([]byte, 96)
	if _, err := decodePricePayload(threeWord); err == nil {
		t.Fatal("expected a loud error for the pre-seq 3-word payload")
	}
}

func TestAssetName(t *testing.T) {
	if got := AssetName(assetBTC); got != "BTC-USD" {
		t.Fatalf("BTC name = %q", got)
	}
	if got := AssetName(assetUSDC); got != "USDC-USD" {
		t.Fatalf("USDC name = %q", got)
	}
	unknown := ethcommon.HexToHash("0xdead")
	if got := AssetName(unknown); got != unknown.Hex() {
		t.Fatalf("unknown name = %q, want hex", got)
	}
}

func TestPackSubmitPrice(t *testing.T) {
	data := packSubmitPrice(assetUSDC, big.NewInt(2500000000))
	if len(data) != 4+64 {
		t.Fatalf("calldata length = %d, want 68", len(data))
	}
	for i, b := range submitPriceSelector {
		if data[i] != b {
			t.Fatalf("selector byte %d = %#x, want %#x", i, data[i], b)
		}
	}
	if got := ethcommon.BytesToHash(data[4:36]); got != assetUSDC {
		t.Fatalf("assetID word = %s, want %s", got.Hex(), assetUSDC.Hex())
	}
	if got := new(big.Int).SetBytes(data[36:68]); got.Int64() != 2500000000 {
		t.Fatalf("price word = %s", got)
	}
}

func TestPackReceivePrice(t *testing.T) {
	data := packReceivePrice(0)
	if len(data) != 4+32 {
		t.Fatalf("calldata length = %d, want 36", len(data))
	}
	for i, b := range receivePriceSelector {
		if data[i] != b {
			t.Fatalf("selector byte %d = %#x, want %#x", i, data[i], b)
		}
	}
	if got := new(big.Int).SetBytes(data[4:36]); got.Sign() != 0 {
		t.Fatalf("index word = %s, want 0", got)
	}
}
