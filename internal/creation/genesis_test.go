package creation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// genesisTestFundedTemplate carries the $genesis-funds line the shipped
// templates carry.
var genesisTestFundedTemplate = strings.Replace(
	genesisTestTemplate,
	`"alloc":{}`,
	`"alloc":{"$genesis-funds":{"balance":"`+genesisBalance+`"}}`,
	1,
)

// genesisTestCreated is an arbitrary fixed creation instant used across tests.
var genesisTestCreated = time.Unix(1785000000, 0)

func TestRenderGenesisResolvesGenesisFundsPlaceholder(t *testing.T) {
	address := ethcommon.HexToAddress("0x1234567890123456789012345678901234567890")

	rendered, err := RenderGenesis([]byte(genesisTestFundedTemplate), &address, nil, nil, nil, genesisTestCreated)
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
	if strings.Contains(string(rendered), GenesisFundsPlaceholder) {
		t.Fatal("the placeholder key must not survive rendering")
	}
}

// The template owns the allocations: an operator prefunds accounts and
// prebakes contracts by literal address, and removing $genesis-funds only
// removes the bombard account.
func TestRenderGenesisTemplateAllocationsPassThrough(t *testing.T) {
	template := strings.Replace(genesisTestTemplate, `"alloc":{}`, `"alloc":{
		"0xAbcDef0123456789abCDef0123456789ABcdEF01":{"balance":"0x100"},
		"00000000000000000000000000000000000000cc":{"code":"0x6001","storage":{"0x00":"0x01"},"nonce":"0x1"}
	}`, 1)

	rendered, err := RenderGenesis([]byte(template), nil, nil, nil, nil, genesisTestCreated)
	if err != nil {
		t.Fatal(err)
	}
	var document genesisDocument
	if err := json.Unmarshal(rendered, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Alloc) != 2 {
		t.Fatalf("expected two allocations, got %d", len(document.Alloc))
	}
	funded := document.Alloc[allocKey(ethcommon.HexToAddress("0xAbcDef0123456789abCDef0123456789ABcdEF01"))]
	if funded.Balance != "0x100" {
		t.Fatalf("prefund did not pass through: %+v", funded)
	}
	baked := document.Alloc[allocKey(ethcommon.HexToAddress("0x00000000000000000000000000000000000000cc"))]
	if baked.Code != "0x6001" || baked.Storage["0x00"] != "0x01" || baked.Nonce != "0x1" {
		t.Fatalf("prebaked contract did not pass through: %+v", baked)
	}
	if baked.Balance != "0x0" {
		t.Fatalf("code-only allocation must default to a zero balance: %+v", baked)
	}
}

// Removing the $genesis-funds line is a supported choice: the render
// succeeds, the chain just has no bombard account.
func TestRenderGenesisWithoutPlaceholderFundsNothing(t *testing.T) {
	address := ethcommon.HexToAddress("0x1234567890123456789012345678901234567890")
	rendered, err := RenderGenesis([]byte(genesisTestTemplate), &address, nil, nil, nil, genesisTestCreated)
	if err != nil {
		t.Fatal(err)
	}
	var document genesisDocument
	if err := json.Unmarshal(rendered, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Alloc) != 0 {
		t.Fatalf("expected no allocations, got %d", len(document.Alloc))
	}
	if TemplateFundsGenesisAccount([]byte(genesisTestTemplate)) {
		t.Fatal("template without the placeholder must report no genesis funds")
	}
	if !TemplateFundsGenesisAccount([]byte(genesisTestFundedTemplate)) {
		t.Fatal("template with the placeholder must report genesis funds")
	}
}

func TestRenderGenesisRejectsBadAllocations(t *testing.T) {
	address := ethcommon.HexToAddress("0x1234567890123456789012345678901234567890")
	replace := func(alloc string) []byte {
		return []byte(strings.Replace(genesisTestTemplate, `"alloc":{}`, `"alloc":{`+alloc+`}`, 1))
	}
	for name, testCase := range map[string]struct {
		template     []byte
		genesisFunds *ethcommon.Address
		wantError    string
	}{
		"unknown placeholder": {
			replace(`"$feeder":{"balance":"0x1"}`), &address, "unknown placeholder",
		},
		"non-address key": {
			replace(`"static":{"balance":"0x1"}`), &address, "20-byte hex address",
		},
		"unsupported allocation field": {
			replace(`"0xAbcDef0123456789abCDef0123456789ABcdEF01":{"mcbalance":"0x1"}`), &address, "unknown field",
		},
		"placeholder with code": {
			replace(`"$genesis-funds":{"balance":"0x1","code":"0x6001"}`), &address, "carries only a balance",
		},
		"placeholder without balance": {
			replace(`"$genesis-funds":{}`), &address, "needs a balance",
		},
		"placeholder without a generated address": {
			replace(`"$genesis-funds":{"balance":"0x1"}`), nil, "no funding address",
		},
		"empty allocation": {
			replace(`"0xAbcDef0123456789abCDef0123456789ABcdEF01":{}`), &address, "a balance or code is required",
		},
		"same address twice": {
			replace(`"0xAbcDef0123456789abCDef0123456789ABcdEF01":{"balance":"0x1"},"abcdef0123456789abcdef0123456789abcdef01":{"balance":"0x2"}`), &address, "already allocated",
		},
	} {
		_, err := RenderGenesis(testCase.template, testCase.genesisFunds, nil, nil, nil, genesisTestCreated)
		if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
			t.Fatalf("%s: expected error containing %q, got %v", name, testCase.wantError, err)
		}
	}
}

// A generated address landing on a template allocation must refuse loudly,
// never overwrite either side.
func TestRenderGenesisRejectsInjectedCollision(t *testing.T) {
	feeder := ethcommon.HexToAddress("0xAbcDef0123456789abCDef0123456789ABcdEF01")
	template := strings.Replace(genesisTestTemplate, `"alloc":{}`, `"alloc":{"0xAbcDef0123456789abCDef0123456789ABcdEF01":{"balance":"0x1"}}`, 1)
	if _, err := RenderGenesis([]byte(template), nil, []ethcommon.Address{feeder}, nil, nil, genesisTestCreated); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("expected a collision error, got %v", err)
	}
}

// A genesis stamped before the network's Granite activation leaves Granite
// inactive at block zero, which silently discards initialMinDelayMS and starts
// the chain at the 2000ms ACP-226 default.
func TestRenderGenesisStampsCreationTime(t *testing.T) {
	rendered, err := RenderGenesis([]byte(genesisTestTemplate), nil, []ethcommon.Address{ethcommon.HexToAddress("0x01")}, nil, nil, genesisTestCreated)
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

	rendered, err := RenderGenesis([]byte(genesisTestFundedTemplate), &funded, []ethcommon.Address{feeder}, []ContractAllocation{contract}, nil, genesisTestCreated)
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

	rendered, err := RenderGenesis(withFeeManager, nil, []ethcommon.Address{admin}, nil, &admin, genesisTestCreated)
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

	if _, err := RenderGenesis(withFeeManager, nil, []ethcommon.Address{admin}, nil, nil, genesisTestCreated); err == nil {
		t.Fatal("feeManagerConfig without an admin must be rejected")
	}
	if _, err := RenderGenesis([]byte(genesisTestTemplate), nil, []ethcommon.Address{admin}, nil, &admin, genesisTestCreated); err == nil {
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
		if _, err := RenderGenesis([]byte(genesisTestTemplate), nil, funded, []ContractAllocation{contract}, nil, genesisTestCreated); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}
