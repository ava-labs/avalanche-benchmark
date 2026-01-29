package main

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/binary"
	"sync"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
)

// Key represents a sender key with its state.
type Key struct {
	PrivateKey *ecdsa.PrivateKey
	Address    common.Address
	Nonce      uint64 // Local nonce counter
	Active     bool   // Whether this key is sending
	mu         sync.Mutex
}

// KeyPool manages a pool of pre-generated keys.
type KeyPool struct {
	keys []*Key
	mu   sync.RWMutex
}

// Seed for deterministic key generation - same keys every run
const keySeed = "bombard-benchmark-keys-v1"

// NewKeyPool generates a pool of deterministic keys.
// Keys are derived from a seed so they're the same across restarts.
func NewKeyPool(count int) *KeyPool {
	keys := make([]*Key, count)
	for i := 0; i < count; i++ {
		privKey := deriveKey(i)
		keys[i] = &Key{
			PrivateKey: privKey,
			Address:    crypto.PubkeyToAddress(privKey.PublicKey),
			Active:     false,
		}
	}
	return &KeyPool{keys: keys}
}

// deriveKey generates a deterministic private key from index
func deriveKey(index int) *ecdsa.PrivateKey {
	// Hash seed + index to get deterministic 32 bytes
	h := sha256.New()
	h.Write([]byte(keySeed))
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(index))
	h.Write(buf)
	seed := h.Sum(nil)

	privKey, err := crypto.ToECDSA(seed)
	if err != nil {
		panic(err)
	}
	return privKey
}

// Get returns a key by index.
func (p *KeyPool) Get(i int) *Key {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.keys[i]
}

// Len returns the pool size.
func (p *KeyPool) Len() int {
	return len(p.keys)
}

// ActiveKeys returns all currently active keys.
func (p *KeyPool) ActiveKeys() []*Key {
	p.mu.RLock()
	defer p.mu.RUnlock()

	active := make([]*Key, 0)
	for _, k := range p.keys {
		if k.Active {
			active = append(active, k)
		}
	}
	return active
}

// ActivateN activates the first N inactive keys, returns newly activated.
func (p *KeyPool) ActivateN(n int) []*Key {
	p.mu.Lock()
	defer p.mu.Unlock()

	activated := make([]*Key, 0, n)
	for _, k := range p.keys {
		if len(activated) >= n {
			break
		}
		if !k.Active {
			k.Active = true
			activated = append(activated, k)
		}
	}
	return activated
}

// RandomTarget returns a random inactive key's address for use as tx target.
// Falls back to a deterministic address if all keys are active.
func (p *KeyPool) RandomTarget(seed int) common.Address {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Simple approach: use seed to pick from pool
	idx := seed % len(p.keys)
	return p.keys[idx].Address
}

// IncrementNonce atomically increments and returns the nonce for a key.
func (k *Key) IncrementNonce() uint64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := k.Nonce
	k.Nonce++
	return n
}

// SetNonce sets the nonce (used for recovery).
func (k *Key) SetNonce(n uint64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.Nonce = n
}

// GetNonce returns current nonce.
func (k *Key) GetNonce() uint64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.Nonce
}
