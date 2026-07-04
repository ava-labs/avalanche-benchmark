// Package fujikey loads the generated (gitignored) Fuji wallet key shared by
// cmd/fuji-wallet and cmd/create-l1.
package fujikey

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
)

// Load reads a hex-encoded secp256k1 private key (as written by fuji-wallet gen).
func Load(keyPath string) (*secp256k1.PrivateKey, error) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", keyPath, err)
	}
	return secp256k1.ToPrivateKey(b)
}
