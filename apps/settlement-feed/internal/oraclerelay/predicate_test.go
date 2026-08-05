package oraclerelay

import (
	"bytes"
	"testing"

	ethcommon "github.com/ava-labs/libevm/common"
)

func TestPackPredicateRoundTrip(t *testing.T) {
	cases := map[string]int{
		"empty":                 0,
		"one byte":              1,
		"partial chunk":         20,
		"one byte short of 32":  31,
		"exactly 32":            32,
		"one byte past 32":      33,
		"exactly 64":            64,
		"three chunks and some": 70,
	}
	for name, length := range cases {
		t.Run(name, func(t *testing.T) {
			message := make([]byte, length)
			for i := range message {
				message[i] = byte(i%254) + 1 // avoid trailing zeros masking the delimiter
			}
			chunks := PackPredicate(message)
			if len(chunks)*ethcommon.HashLength%ethcommon.HashLength != 0 {
				t.Fatalf("packed length is not a multiple of 32")
			}
			// The delimiter forces a whole extra chunk at exact boundaries.
			wantChunks := length/ethcommon.HashLength + 1
			if len(chunks) != wantChunks {
				t.Fatalf("chunk count = %d, want %d", len(chunks), wantChunks)
			}
			got, err := UnpackPredicate(chunks)
			if err != nil {
				t.Fatalf("unpack: %v", err)
			}
			if !bytes.Equal(got, message) {
				t.Fatalf("round trip mismatch:\n got %x\nwant %x", got, message)
			}
		})
	}
}

func TestPackPredicatePreservesTrailingZeroBytes(t *testing.T) {
	// A message that itself ends in zero bytes must survive: the delimiter marks
	// the true end so those zeros are not trimmed away as padding.
	message := []byte{1, 2, 3, 0, 0, 0}
	got, err := UnpackPredicate(PackPredicate(message))
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if !bytes.Equal(got, message) {
		t.Fatalf("round trip mismatch: got %x, want %x", got, message)
	}
}

func TestUnpackPredicateRejectsMissingDelimiter(t *testing.T) {
	if _, err := UnpackPredicate([]ethcommon.Hash{{}}); err == nil {
		t.Fatal("expected error for all-zero predicate")
	}
}

func TestUnpackPredicateRejectsExcessPadding(t *testing.T) {
	chunks := PackPredicate([]byte{1, 2, 3})
	chunks = append(chunks, ethcommon.Hash{}) // extra zero chunk
	if _, err := UnpackPredicate(chunks); err == nil {
		t.Fatal("expected error for excess padding")
	}
}
