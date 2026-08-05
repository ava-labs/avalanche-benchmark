package oraclerelay

import (
	"errors"
	"fmt"

	ethcommon "github.com/ava-labs/libevm/common"
)

// predicateDelimiter separates the signed Warp message from the zero padding
// inside an access-list predicate. subnet-evm's verifier trims trailing zeros
// and requires this byte at the end, so the packer must append it even when the
// message is already a multiple of 32 bytes; otherwise the last real byte would
// be indistinguishable from padding. This mirrors avalanchego
// vms/evm/predicate.New/Bytes exactly.
const predicateDelimiter = 0xff

var (
	errPredicateMissingDelimiter = errors.New("predicate has no delimiter")
	errPredicateExcessPadding    = errors.New("predicate included excess padding")
	errPredicateWrongDelimiter   = errors.New("predicate has the wrong delimiter")
)

// PackPredicate chunks a signed Warp message into 32-byte access-list storage
// keys: the message, the 0xff delimiter, and zero padding to a multiple of 32.
func PackPredicate(message []byte) []ethcommon.Hash {
	unpaddedChunks := len(message) / ethcommon.HashLength
	chunks := make([]ethcommon.Hash, unpaddedChunks+1)
	for i := range chunks[:unpaddedChunks] {
		chunks[i] = ethcommon.Hash(message[ethcommon.HashLength*i:])
	}
	// The delimiter always lands in a fresh final chunk; when the message is an
	// exact multiple of 32 that chunk holds only the delimiter and padding.
	copy(chunks[unpaddedChunks][:], message[ethcommon.HashLength*unpaddedChunks:])
	chunks[unpaddedChunks][len(message)%ethcommon.HashLength] = predicateDelimiter
	return chunks
}

// UnpackPredicate reverses PackPredicate, rejecting malformed encodings so the
// round-trip is provably lossless.
func UnpackPredicate(chunks []ethcommon.Hash) ([]byte, error) {
	padded := make([]byte, ethcommon.HashLength*len(chunks))
	for i, chunk := range chunks {
		copy(padded[ethcommon.HashLength*i:], chunk[:])
	}
	trimmed := ethcommon.TrimRightZeroes(padded)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: length (%d)", errPredicateMissingDelimiter, len(chunks))
	}
	expectedLen := (len(trimmed) + ethcommon.HashLength - 1) / ethcommon.HashLength
	if expectedLen != len(chunks) {
		return nil, fmt.Errorf("%w: got %d chunks, expected %d", errPredicateExcessPadding, len(chunks), expectedLen)
	}
	delimiterIndex := len(trimmed) - 1
	if trimmed[delimiterIndex] != predicateDelimiter {
		return nil, errPredicateWrongDelimiter
	}
	return trimmed[:delimiterIndex], nil
}
