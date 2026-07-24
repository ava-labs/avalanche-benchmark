package oraclerelay

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	ethcommon "github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
)

// loadFeederKey reads deployment/oracle-feeder.key (64 hex chars, no 0x prefix)
// and checks that its EVM address matches the recorded FEEDER_EVM_ADDRESS. The
// feeder key pays for both the oracle-chain feed txs and the main-chain delivery
// txs, so a mismatch is a fatal deployment-integrity error.
func loadFeederKey(path string, expected ethcommon.Address) (*ecdsa.PrivateKey, ethcommon.Address, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, ethcommon.Address{}, fmt.Errorf("read required oracle feeder key %s: %w", path, err)
	}
	raw := strings.TrimSpace(string(contents))
	if strings.HasPrefix(raw, "0x") {
		return nil, ethcommon.Address{}, fmt.Errorf("%s: oracle feeder key must not have a 0x prefix", path)
	}
	keyBytes, err := hex.DecodeString(raw)
	if err != nil || len(keyBytes) != 32 {
		return nil, ethcommon.Address{}, fmt.Errorf("%s: oracle feeder key must be exactly 64 hex characters", path)
	}
	key, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		return nil, ethcommon.Address{}, fmt.Errorf("%s: load oracle feeder key: %w", path, err)
	}
	address := crypto.PubkeyToAddress(key.PublicKey)
	if address != expected {
		return nil, ethcommon.Address{}, fmt.Errorf("%s: oracle feeder key address %s does not match FEEDER_EVM_ADDRESS %s", path, address.Hex(), expected.Hex())
	}
	return key, address, nil
}
