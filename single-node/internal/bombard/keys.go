package bombard

import (
	"crypto/ecdsa"
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

func generateKeys(count int) []*Key {
	keys := make([]*Key, count)
	for i := 0; i < count; i++ {
		privateKey, err := crypto.GenerateKey()
		if err != nil {
			log.Fatalf("failed to generate key: %v", err)
		}
		keys[i] = &Key{
			PrivateKey: privateKey,
			Address:    crypto.PubkeyToAddress(privateKey.PublicKey),
		}
	}
	return keys
}
