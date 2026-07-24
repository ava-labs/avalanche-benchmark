package creation

import (
	"encoding/json"
	"strings"
	"testing"

	ethcommon "github.com/ava-labs/libevm/common"
)

const genesisTestTemplate = `{
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
}`

func TestRenderGenesisFundsOnlyDerivedAddress(t *testing.T) {
	address := ethcommon.HexToAddress("0x1234567890123456789012345678901234567890")

	rendered, err := RenderGenesis([]byte(genesisTestTemplate), []ethcommon.Address{address}, nil, nil)
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
	if _, err := RenderGenesis(template, []ethcommon.Address{{}}, nil, nil); err == nil {
		t.Fatal("expected static allocation to be rejected")
	}
}

func TestRenderGenesisBakesContractCodeAndStorage(t *testing.T) {
	funded := ethcommon.HexToAddress("0x1234567890123456789012345678901234567890")
	feeder := ethcommon.HexToAddress("0xAbcDef0123456789abCDef0123456789ABcdEF01")
	contract := ContractAllocation{
		Address:     AggregatorAddress,
		RuntimeCode: "0x6001",
		Storage: map[ethcommon.Hash]ethcommon.Hash{
			{}: ethcommon.BytesToHash(feeder.Bytes()),
		},
	}

	rendered, err := RenderGenesis([]byte(genesisTestTemplate), []ethcommon.Address{funded, feeder}, []ContractAllocation{contract}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var document genesisDocument
	if err := json.Unmarshal(rendered, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Alloc) != 3 {
		t.Fatalf("expected three allocations, got %d", len(document.Alloc))
	}
	baked := document.Alloc[allocKey(AggregatorAddress)]
	if baked.Code != "0x6001" || baked.Balance != "0x0" {
		t.Fatalf("unexpected contract allocation: %+v", baked)
	}
	slotZero := ethcommon.Hash{}.Hex()
	if baked.Storage[slotZero] != ethcommon.BytesToHash(feeder.Bytes()).Hex() {
		t.Fatalf("feeder slot not initialized: %+v", baked.Storage)
	}
	if document.Alloc[allocKey(feeder)].Balance != genesisBalance {
		t.Fatal("feeder address was not funded")
	}
}

func TestRenderGenesisFeeManagerAdmin(t *testing.T) {
	admin := ethcommon.HexToAddress("0xAbcDef0123456789abCDef0123456789ABcdEF01")
	withFeeManager := []byte(strings.Replace(genesisTestTemplate, `"config":{"chainId":99999}`, `"config":{"chainId":99999,"feeManagerConfig":{"blockTimestamp":7}}`, 1))

	rendered, err := RenderGenesis(withFeeManager, []ethcommon.Address{admin}, nil, &admin)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(rendered, &document); err != nil {
		t.Fatal(err)
	}
	feeManager := document["config"].(map[string]any)["feeManagerConfig"].(map[string]any)
	if feeManager["blockTimestamp"] != float64(7) {
		t.Fatalf("template fields must survive injection: %+v", feeManager)
	}
	admins := feeManager["adminAddresses"].([]any)
	if len(admins) != 1 || admins[0] != admin.Hex() {
		t.Fatalf("admin not injected: %+v", admins)
	}

	if _, err := RenderGenesis(withFeeManager, []ethcommon.Address{admin}, nil, nil); err == nil {
		t.Fatal("feeManagerConfig without an admin must be rejected")
	}
	if _, err := RenderGenesis([]byte(genesisTestTemplate), []ethcommon.Address{admin}, nil, &admin); err == nil {
		t.Fatal("admin without feeManagerConfig must be rejected")
	}
}

func TestRenderGenesisRejectsBadContracts(t *testing.T) {
	funded := []ethcommon.Address{ethcommon.HexToAddress("0x1234567890123456789012345678901234567890")}
	for name, contract := range map[string]ContractAllocation{
		"empty code":       {Address: AggregatorAddress, RuntimeCode: ""},
		"unprefixed code":  {Address: AggregatorAddress, RuntimeCode: "6001"},
		"funded collision": {Address: funded[0], RuntimeCode: "0x6001"},
	} {
		if _, err := RenderGenesis([]byte(genesisTestTemplate), funded, []ContractAllocation{contract}, nil); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}
