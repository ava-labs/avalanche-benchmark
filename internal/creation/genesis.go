package creation

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	ethcommon "github.com/ava-labs/libevm/common"
)

const genesisBalance = "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"

type genesisDocument struct {
	Config     json.RawMessage              `json:"config"`
	Alloc      map[string]genesisAllocation `json:"alloc"`
	Nonce      string                       `json:"nonce"`
	Timestamp  string                       `json:"timestamp"`
	ExtraData  string                       `json:"extraData"`
	GasLimit   string                       `json:"gasLimit"`
	Difficulty string                       `json:"difficulty"`
	MixHash    string                       `json:"mixHash"`
	Coinbase   string                       `json:"coinbase"`
	Number     string                       `json:"number"`
	GasUsed    string                       `json:"gasUsed"`
	ParentHash string                       `json:"parentHash"`
}

type genesisAllocation struct {
	Balance string            `json:"balance"`
	Code    string            `json:"code,omitempty"`
	Storage map[string]string `json:"storage,omitempty"`
}

// ContractAllocation bakes a contract's DEPLOYED bytecode straight into
// Genesis. The contracts are written without constructors or immutables so
// their entire configuration is these explicit storage slots.
type ContractAllocation struct {
	Address     ethcommon.Address
	RuntimeCode string
	Storage     map[ethcommon.Hash]ethcommon.Hash
}

// RenderGenesis injects the funding and contract allocations and stamps the
// genesis with the creation time.
//
// The timestamp is not cosmetic. Network upgrade times come from the network,
// not from the genesis chain config, so a genesis stamped 0 sits decades before
// Granite activated and Granite is therefore inactive AT GENESIS. Subnet-EVM
// seeds the ACP-226 minimum block delay only inside its Granite branch, so a
// zero timestamp silently discards initialMinDelayMS and the chain starts at
// the 2000ms default, converging down over hours. Stamping creation time keeps
// Granite active from block zero on both Fuji and mainnet.
func RenderGenesis(template []byte, funded []ethcommon.Address, contracts []ContractAllocation, feeManagerAdmin *ethcommon.Address, createdAt time.Time) ([]byte, error) {
	var document genesisDocument
	if err := json.Unmarshal(template, &document); err != nil {
		return nil, fmt.Errorf("parse genesis template: %w", err)
	}
	if len(document.Config) == 0 {
		return nil, fmt.Errorf("genesis template: required config is not provided")
	}
	config, err := injectFeeManagerAdmin(document.Config, feeManagerAdmin)
	if err != nil {
		return nil, err
	}
	document.Config = config
	document.Timestamp = fmt.Sprintf("0x%x", createdAt.Unix())
	if len(document.Alloc) != 0 {
		return nil, fmt.Errorf("genesis template: alloc must be empty before funding-key injection")
	}
	if len(funded) == 0 {
		return nil, fmt.Errorf("genesis: at least one funded address is required")
	}
	document.Alloc = make(map[string]genesisAllocation, len(funded)+len(contracts))
	for _, address := range funded {
		document.Alloc[allocKey(address)] = genesisAllocation{Balance: genesisBalance}
	}
	for _, contract := range contracts {
		code := contract.RuntimeCode
		if !strings.HasPrefix(code, "0x") || len(code) <= 2 {
			return nil, fmt.Errorf("genesis contract %s: runtime code must be non-empty 0x-prefixed hex", contract.Address)
		}
		if _, exists := document.Alloc[allocKey(contract.Address)]; exists {
			return nil, fmt.Errorf("genesis contract %s: address is already allocated", contract.Address)
		}
		storage := make(map[string]string, len(contract.Storage))
		for slot, value := range contract.Storage {
			storage[slot.Hex()] = value.Hex()
		}
		document.Alloc[allocKey(contract.Address)] = genesisAllocation{
			Balance: "0x0",
			Code:    code,
			Storage: storage,
		}
	}
	genesis, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render genesis: %w", err)
	}
	return append(genesis, '\n'), nil
}

func allocKey(address ethcommon.Address) string {
	return strings.TrimPrefix(address.Hex(), "0x")
}

// injectFeeManagerAdmin writes the admin key into the template's
// feeManagerConfig. The admin address is generated at keygen, so the template
// cannot carry it; a template with the precompile but no admin (locked
// forever) or an admin with no precompile (silently unused) are both refused.
func injectFeeManagerAdmin(config json.RawMessage, admin *ethcommon.Address) (json.RawMessage, error) {
	var parsed map[string]any
	if err := json.Unmarshal(config, &parsed); err != nil {
		return nil, fmt.Errorf("parse genesis template config: %w", err)
	}
	raw, hasFeeManager := parsed["feeManagerConfig"]
	if admin == nil {
		if hasFeeManager {
			return nil, fmt.Errorf("genesis template: feeManagerConfig requires an admin address, none was provided")
		}
		return config, nil
	}
	if !hasFeeManager {
		return nil, fmt.Errorf("genesis template: fee manager admin provided but template has no feeManagerConfig")
	}
	feeManager, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("genesis template: feeManagerConfig must be an object")
	}
	feeManager["adminAddresses"] = []string{admin.Hex()}
	rendered, err := json.Marshal(parsed)
	if err != nil {
		return nil, fmt.Errorf("render genesis config: %w", err)
	}
	return rendered, nil
}
