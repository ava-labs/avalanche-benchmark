package creation

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
)

func TestRenderGenesisFundsOnlyDerivedAddress(t *testing.T) {
	keyBytes, err := hex.DecodeString(strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	key, err := secp256k1.ToPrivateKey(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	template := []byte(`{
		"config":{"chainId":99999},
		"alloc":{},
		"nonce":"0x0",
		"timestamp":"0x0",
		"extraData":"0x00",
		"gasLimit":"0x1",
		"difficulty":"0x0",
		"mixHash":"0x0",
		"coinbase":"0x0",
		"number":"0x0",
		"gasUsed":"0x0",
		"parentHash":"0x0"
	}`)

	rendered, err := RenderGenesis(template, key)
	if err != nil {
		t.Fatal(err)
	}
	var document genesisDocument
	if err := json.Unmarshal(rendered, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Alloc) != 1 {
		t.Fatalf("expected one funded address, got %d", len(document.Alloc))
	}
	address := strings.TrimPrefix(key.EthAddress().Hex(), "0x")
	if document.Alloc[address].Balance != genesisBalance {
		t.Fatalf("derived address %s was not funded", address)
	}
}

func TestRenderGenesisRejectsStaticAllocation(t *testing.T) {
	key, err := secp256k1.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	template := []byte(`{"config":{},"alloc":{"static":{"balance":"1"}}}`)
	if _, err := RenderGenesis(template, key); err == nil {
		t.Fatal("expected static allocation to be rejected")
	}
}
