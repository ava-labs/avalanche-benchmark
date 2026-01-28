package bombard

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/binary"
	"log"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
)

// Key represents an Ethereum key pair.
type Key struct {
	PrivateKey *ecdsa.PrivateKey
	Address    common.Address
}

// Package-level config variables
var (
	batchSize      = 50
	keyCount       = 600
	timeoutSeconds = 10
)

// Deterministic seed for key generation - same seed = same keys every run
const keySeed = "avalanche-bombard-benchmark-seed-v1"

func generateKeys(count int) []*Key {
	keys := make([]*Key, count)
	for i := 0; i < count; i++ {
		// Derive deterministic private key from seed + index
		h := sha256.New()
		h.Write([]byte(keySeed))
		binary.Write(h, binary.BigEndian, uint64(i))
		seed := h.Sum(nil)

		privateKey, err := crypto.ToECDSA(seed)
		if err != nil {
			log.Fatalf("failed to derive key %d: %v", i, err)
		}
		keys[i] = &Key{
			PrivateKey: privateKey,
			Address:    crypto.PubkeyToAddress(privateKey.PublicKey),
		}
	}
	return keys
}
