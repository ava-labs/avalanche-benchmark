package oraclerelay

import (
	"fmt"
	"math/big"

	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
	warppayload "github.com/ava-labs/avalanchego/vms/platformvm/warp/payload"
	ethcommon "github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
)

// WarpPrecompileAddress is the subnet-evm Warp precompile; the aggregator's
// SendWarpMessage logs are emitted from here and its predicate rides the main
// chain delivery tx's access list keyed by this address.
var WarpPrecompileAddress = ethcommon.HexToAddress("0x0200000000000000000000000000000000000005")

// SendWarpMessageTopic is topics[0] of the subnet-evm Warp precompile event
// event SendWarpMessage(address indexed sender, bytes32 indexed messageID, bytes message).
var SendWarpMessageTopic = crypto.Keccak256Hash([]byte("SendWarpMessage(address,bytes32,bytes)"))

// Contract selectors, mirrored from contracts/artifacts/selectors.json.
var (
	submitPriceSelector  = [4]byte{0x9f, 0x21, 0xbf, 0x2e} // submitPrice(bytes32,uint256)
	receivePriceSelector = [4]byte{0xb8, 0xc1, 0x27, 0x58} // receivePrice(uint32)
	latestPriceSelector  = [4]byte{0x4e, 0xec, 0x7c, 0x2b} // latestPrice(bytes32)
	// receivePrices(uint32) — batched delivery. Derived locally as
	// keccak256("receivePrices(uint32)")[:4]; verified against selectors.json's
	// other entries with the same method. Hardcoded until the contracts agent
	// lands it in selectors.json.
	receivePricesSelector = [4]byte{0x0f, 0xb5, 0x7c, 0xdd}
)

// Mock feed assets. assetId is the keccak256 of the human-readable symbol, the
// same value the feeder passes to submitPrice, so the relay can label prices.
var (
	assetBTC  = crypto.Keccak256Hash([]byte("BTC-USD"))
	assetUSDC = crypto.Keccak256Hash([]byte("USDC-USD"))
)

type assetRef struct {
	name string
	id   ethcommon.Hash
}

// KnownAssets is the fixed set the feeder submits and the relay charts.
var KnownAssets = []assetRef{
	{"BTC-USD", assetBTC},
	{"USDC-USD", assetUSDC},
}

// AssetName returns the symbol for a known assetId, or its hex otherwise.
func AssetName(id ethcommon.Hash) string {
	for _, asset := range KnownAssets {
		if asset.id == id {
			return asset.name
		}
	}
	return id.Hex()
}

// assetIDByName reverses AssetName for the known assets, so the main-chain price
// poller can query latestPrice by symbol.
func assetIDByName(name string) (ethcommon.Hash, bool) {
	for _, asset := range KnownAssets {
		if asset.name == name {
			return asset.id, true
		}
	}
	return ethcommon.Hash{}, false
}

// Submission is the decoded aggregator payload: abi.encode(bytes32 assetId,
// uint256 price, uint256 updatedAt, uint256 seq). seq is a per-asset monotonic
// counter starting at 1, assigned by the aggregator contract.
type Submission struct {
	AssetID   ethcommon.Hash
	Price     *big.Int
	UpdatedAt *big.Int
	Seq       *big.Int
}

// UnpackEventMessage extracts the unsigned Warp message bytes from a
// SendWarpMessage log's data field, which ABI-encodes a single dynamic `bytes`
// argument: a 32-byte offset, a 32-byte length, then the padded payload.
func UnpackEventMessage(data []byte) ([]byte, error) {
	if len(data) < 64 {
		return nil, fmt.Errorf("warp event data is %d bytes, need at least 64", len(data))
	}
	offset := new(big.Int).SetBytes(data[0:32]).Uint64()
	if offset != 32 {
		return nil, fmt.Errorf("warp event data has unexpected bytes offset %d, want 32", offset)
	}
	length := new(big.Int).SetBytes(data[32:64]).Uint64()
	if uint64(len(data)) < 64+length {
		return nil, fmt.Errorf("warp event data claims %d message bytes but only %d remain", length, len(data)-64)
	}
	return data[64 : 64+length], nil
}

// ParseSubmission parses the unsigned Warp message bytes into the price payload
// carried inside its AddressedCall.
func ParseSubmission(unsignedMessageBytes []byte) (Submission, error) {
	unsigned, err := warp.ParseUnsignedMessage(unsignedMessageBytes)
	if err != nil {
		return Submission{}, fmt.Errorf("parse unsigned Warp message: %w", err)
	}
	call, err := warppayload.ParseAddressedCall(unsigned.Payload)
	if err != nil {
		return Submission{}, fmt.Errorf("parse Warp addressed call: %w", err)
	}
	return decodePricePayload(call.Payload)
}

// decodePricePayload reads abi.encode(bytes32, uint256, uint256, uint256): four
// 32-byte words, no dynamic types. A 3-word payload is from a pre-seq genesis and
// is rejected loudly rather than misparsed.
func decodePricePayload(payload []byte) (Submission, error) {
	if len(payload) == 96 {
		return Submission{}, fmt.Errorf("price payload is 96 bytes (3 words); this build expects the 4-word (assetId, price, updatedAt, seq) layout, so the oracle chain genesis is stale")
	}
	if len(payload) != 128 {
		return Submission{}, fmt.Errorf("price payload is %d bytes, want 128", len(payload))
	}
	return Submission{
		AssetID:   ethcommon.BytesToHash(payload[0:32]),
		Price:     new(big.Int).SetBytes(payload[32:64]),
		UpdatedAt: new(big.Int).SetBytes(payload[64:96]),
		Seq:       new(big.Int).SetBytes(payload[96:128]),
	}, nil
}

// packSubmitPrice builds calldata for submitPrice(bytes32 assetId, uint256 price).
func packSubmitPrice(assetID ethcommon.Hash, price *big.Int) []byte {
	data := make([]byte, 0, 4+64)
	data = append(data, submitPriceSelector[:]...)
	data = append(data, assetID.Bytes()...)
	data = append(data, leftPad32(price.Bytes())...)
	return data
}

// packReceivePrice builds calldata for receivePrice(uint32 messageIndex).
func packReceivePrice(index uint32) []byte {
	data := make([]byte, 0, 4+32)
	data = append(data, receivePriceSelector[:]...)
	data = append(data, leftPad32(new(big.Int).SetUint64(uint64(index)).Bytes())...)
	return data
}

// packReceivePrices builds calldata for receivePrices(uint32 count), where count
// is the number of warp predicates carried in the tx's access list (in order).
func packReceivePrices(count uint32) []byte {
	data := make([]byte, 0, 4+32)
	data = append(data, receivePricesSelector[:]...)
	data = append(data, leftPad32(new(big.Int).SetUint64(uint64(count)).Bytes())...)
	return data
}

// packLatestPrice builds calldata for latestPrice(bytes32 assetId).
func packLatestPrice(assetID ethcommon.Hash) []byte {
	data := make([]byte, 0, 4+32)
	data = append(data, latestPriceSelector[:]...)
	data = append(data, assetID.Bytes()...)
	return data
}

// decodeLatestPrice reads the (uint256 price, uint256 updatedAt) return of
// latestPrice: two 32-byte words.
func decodeLatestPrice(output []byte) (*big.Int, uint64, error) {
	if len(output) != 64 {
		return nil, 0, fmt.Errorf("latestPrice returned %d bytes, want 64", len(output))
	}
	price := new(big.Int).SetBytes(output[0:32])
	updatedAt := new(big.Int).SetBytes(output[32:64]).Uint64()
	return price, updatedAt, nil
}

func leftPad32(b []byte) []byte {
	if len(b) >= 32 {
		return b[len(b)-32:]
	}
	padded := make([]byte, 32)
	copy(padded[32-len(b):], b)
	return padded
}
