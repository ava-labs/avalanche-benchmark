package valmgr

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	ethereum "github.com/ava-labs/libevm"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/vms/evm/predicate"
	avalancheWarp "github.com/ava-labs/avalanchego/vms/platformvm/warp"
	warpmessage "github.com/ava-labs/avalanchego/vms/platformvm/warp/message"
	warppayload "github.com/ava-labs/avalanchego/vms/platformvm/warp/payload"
)

// ValidationID derives the validationID of the index-th conversion-time
// validator: sha256(subnetID || uint32BE(index)). All our validators are
// conversion-time (registered once at convert, never re-registered), so this
// covers the whole set forever.
func ValidationID(subnetID ids.ID, index uint32) ids.ID {
	return subnetID.Append(index)
}

// callMsg builds a minimal eth_call message.
func callMsg(to common.Address, data []byte) ethereum.CallMsg {
	return ethereum.CallMsg{To: &to, Data: data}
}

// predicateAccessList packs a signed warp message into the access-list
// predicate slot of the warp precompile, which is how a C-chain tx hands the
// message to getVerifiedWarpMessage(0).
func predicateAccessList(signedMsg []byte) types.AccessList {
	return types.AccessList{{
		Address:     warpPrecompile,
		StorageKeys: predicate.New(signedMsg),
	}}
}

// WeightMessage reconstructs, byte-exactly, the unsigned L1ValidatorWeight
// warp message the manager contract emitted for (validationID, nonce, weight):
// AddressedCall from the manager address, sourced from the C-chain. Because
// the reconstruction is deterministic we never scrape receipts or logs, which
// is what makes every reconcile step re-runnable.
func WeightMessage(
	networkID uint32,
	cChainID ids.ID,
	manager common.Address,
	validationID ids.ID,
	nonce uint64,
	weight uint64,
) (*avalancheWarp.UnsignedMessage, error) {
	payload, err := warpmessage.NewL1ValidatorWeight(validationID, nonce, weight)
	if err != nil {
		return nil, err
	}
	return addressedCallMessage(networkID, cChainID, manager.Bytes(), payload.Bytes())
}

// WeightAckMessage reconstructs the P-chain-sourced L1ValidatorWeight message
// that acknowledges the weight is live on the P-chain (consumed by
// completeValidatorWeightUpdate). P-chain validators sign it against their
// current state, so nonce/weight must match what the P-chain holds.
func WeightAckMessage(networkID uint32, validationID ids.ID, nonce uint64, weight uint64) (*avalancheWarp.UnsignedMessage, error) {
	payload, err := warpmessage.NewL1ValidatorWeight(validationID, nonce, weight)
	if err != nil {
		return nil, err
	}
	return addressedCallMessage(networkID, constants.PlatformChainID, nil, payload.Bytes())
}

// ConversionMessage reconstructs the P-chain-sourced SubnetToL1Conversion
// message for initializeValidatorSet. Aggregate it with the raw subnetID bytes
// as justification (that is how the P-chain looks the conversion up).
func ConversionMessage(networkID uint32, conversionID ids.ID) (*avalancheWarp.UnsignedMessage, error) {
	payload, err := warpmessage.NewSubnetToL1Conversion(conversionID)
	if err != nil {
		return nil, err
	}
	return addressedCallMessage(networkID, constants.PlatformChainID, nil, payload.Bytes())
}

func addressedCallMessage(networkID uint32, sourceChain ids.ID, sourceAddr []byte, payload []byte) (*avalancheWarp.UnsignedMessage, error) {
	call, err := warppayload.NewAddressedCall(sourceAddr, payload)
	if err != nil {
		return nil, err
	}
	return avalancheWarp.NewUnsignedMessage(networkID, sourceChain, call.Bytes())
}

// defaultAggregatorURL is the public, unauthenticated Glacier signature
// aggregator for Fuji. Override with AGGREGATOR_URL.
const defaultAggregatorURL = "https://glacier-api.avax.network/v1/signatureAggregator/fuji/aggregateSignatures"

type aggregateRequest struct {
	Message          string `json:"message"`
	Justification    string `json:"justification,omitempty"`
	QuorumPercentage uint64 `json:"quorumPercentage"`
}

type aggregateResponse struct {
	SignedMessage string `json:"signedMessage"`
	Message       string `json:"message"` // error detail on non-200
}

// Aggregate collects a primary-network BLS aggregate signature over the
// unsigned message via the public aggregator. signingSubnetId is omitted: it
// defaults to the source chain's subnet, and both our source chains (Fuji C
// and the P-chain) are the primary network. Retries transient failures a few
// times; the callers' reconcile loops retry the whole step anyway.
func Aggregate(ctx context.Context, unsigned *avalancheWarp.UnsignedMessage, justification []byte) (*avalancheWarp.Message, error) {
	url := os.Getenv("AGGREGATOR_URL")
	if url == "" {
		url = defaultAggregatorURL
	}
	req := aggregateRequest{
		Message:          "0x" + hex.EncodeToString(unsigned.Bytes()),
		QuorumPercentage: 67,
	}
	if len(justification) > 0 {
		req.Justification = "0x" + hex.EncodeToString(justification)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		signed, err := aggregateOnce(ctx, url, body)
		if err == nil {
			return signed, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func aggregateOnce(ctx context.Context, url string, body []byte) (*avalancheWarp.Message, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("aggregator: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("aggregator read: %w", err)
	}
	var parsed aggregateResponse
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.SignedMessage == "" {
		return nil, fmt.Errorf("aggregator HTTP %d: %s", resp.StatusCode, firstLine(raw))
	}
	msgBytes, err := hex.DecodeString(trim0x(parsed.SignedMessage))
	if err != nil {
		return nil, fmt.Errorf("aggregator signed message hex: %w", err)
	}
	return avalancheWarp.ParseMessage(msgBytes)
}

func trim0x(s string) string {
	if len(s) >= 2 && s[:2] == "0x" {
		return s[2:]
	}
	return s
}

func firstLine(b []byte) string {
	s := string(b)
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		s = string(b[:i])
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
