package creation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	ethcommon "github.com/ava-labs/libevm/common"
)

func TestRenderGenesisFundsOnlyDerivedAddress(t *testing.T) {
	address := ethcommon.HexToAddress("0x1234567890123456789012345678901234567890")
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

	created := time.Unix(1785000000, 0)
	rendered, err := RenderGenesis(template, address, created)
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
	addressWithoutPrefix := strings.TrimPrefix(address.Hex(), "0x")
	if document.Alloc[addressWithoutPrefix].Balance != genesisBalance {
		t.Fatalf("address %s was not funded", addressWithoutPrefix)
	}
}

func TestRenderGenesisRejectsStaticAllocation(t *testing.T) {
	template := []byte(`{"config":{},"alloc":{"static":{"balance":"1"}}}`)
	if _, err := RenderGenesis(template, ethcommon.Address{}, time.Unix(1785000000, 0)); err == nil {
		t.Fatal("expected static allocation to be rejected")
	}
}

// A genesis stamped before the network's Granite activation leaves Granite
// inactive at block zero, which silently discards initialMinDelayMS and starts
// the chain at the 2000ms ACP-226 default.
func TestRenderGenesisStampsCreationTime(t *testing.T) {
	template := []byte(`{
		"config":{"chainId":99999,"initialMinDelayMS":25},
		"alloc":{},
		"timestamp":"0x0"
	}`)
	created := time.Unix(1785000000, 0)
	rendered, err := RenderGenesis(template, ethcommon.HexToAddress("0x01"), created)
	if err != nil {
		t.Fatal(err)
	}
	var document genesisDocument
	if err := json.Unmarshal(rendered, &document); err != nil {
		t.Fatal(err)
	}
	want := "0x6a64f040"
	if document.Timestamp != want {
		t.Fatalf("genesis timestamp = %q, want %q (the creation time, not the template zero)", document.Timestamp, want)
	}
}
