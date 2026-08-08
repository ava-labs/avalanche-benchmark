package creation

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	ethcommon "github.com/ava-labs/libevm/common"
)

const genesisBalance = "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"

// GenesisFundsPlaceholder is the one template alloc key that is not a literal
// address. It resolves to the funding address that keygen generates
// (deployment/genesis-funds.key), which `bombard` spends. The address exists
// only after keygen, so the template cannot state it; the placeholder lets the
// template own the allocation anyway. An operator who removes the line gets a
// chain without a load account, and bombard does not work there. Nothing else
// breaks.
const GenesisFundsPlaceholder = "$genesis-funds"

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
	Nonce   string            `json:"nonce,omitempty"`
}

// ContractAllocation bakes a contract's DEPLOYED bytecode straight into
// Genesis. The contracts are written without constructors or immutables so
// their entire configuration is these explicit storage slots.
type ContractAllocation struct {
	Address     ethcommon.Address
	RuntimeCode string
	Storage     map[ethcommon.Hash]ethcommon.Hash
}

// RenderGenesis renders a chain's genesis from its template and stamps it
// with the creation time.
//
// The template owns the allocations. An alloc key is either the
// $genesis-funds placeholder, which resolves to the generated funding address
// with the balance the template states, or a literal 20-byte hex address,
// which passes through verbatim (balance, code, storage, nonce). A chain's
// template can therefore prefund operator accounts and prebake the chain's
// own contracts. A template without the placeholder renders a genesis with no
// load account; that only disables bombard on that chain.
//
// The generated allocations inject second: the shared funded addresses (the
// feeder) and the shipped contract allocations. A collision with a template
// allocation is an error, never an overwrite.
//
// The timestamp is not cosmetic. Network upgrade times come from the network,
// not from the genesis chain config, so a genesis stamped 0 sits decades before
// Granite activated and Granite is therefore inactive AT GENESIS. Subnet-EVM
// seeds the ACP-226 minimum block delay only inside its Granite branch, so a
// zero timestamp silently discards initialMinDelayMS and the chain starts at
// the 2000ms default. Stamping creation time keeps
// Granite active from block zero on both Fuji and mainnet.
func RenderGenesis(template []byte, genesisFunds *ethcommon.Address, funded []ethcommon.Address, contracts []ContractAllocation, feeManagerAdmin *ethcommon.Address, createdAt time.Time) ([]byte, error) {
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
	// The typed document drops alloc fields it does not know. Re-parse the
	// alloc raw so an unsupported field is an error, not a silent drop.
	var templateAlloc struct {
		Alloc map[string]json.RawMessage `json:"alloc"`
	}
	if err := json.Unmarshal(template, &templateAlloc); err != nil {
		return nil, fmt.Errorf("parse genesis template: %w", err)
	}
	document.Alloc = make(map[string]genesisAllocation, len(templateAlloc.Alloc)+len(funded)+len(contracts))
	for key, raw := range templateAlloc.Alloc {
		allocation, err := decodeAllocation(key, raw)
		if err != nil {
			return nil, err
		}
		var normalized string
		switch {
		case key == GenesisFundsPlaceholder:
			if genesisFunds == nil {
				return nil, fmt.Errorf("genesis template: %s is present but no funding address was generated", GenesisFundsPlaceholder)
			}
			if allocation.Code != "" || len(allocation.Storage) != 0 || allocation.Nonce != "" {
				return nil, fmt.Errorf("genesis template: %s carries only a balance", GenesisFundsPlaceholder)
			}
			if allocation.Balance == "" {
				return nil, fmt.Errorf("genesis template: %s needs a balance", GenesisFundsPlaceholder)
			}
			normalized = allocKey(*genesisFunds)
		case strings.HasPrefix(key, "$"):
			return nil, fmt.Errorf("genesis template: unknown placeholder %q; the only placeholder is %s", key, GenesisFundsPlaceholder)
		default:
			address, err := parseAllocAddress(key)
			if err != nil {
				return nil, err
			}
			if allocation.Balance == "" && allocation.Code == "" {
				return nil, fmt.Errorf("genesis template allocation %s: a balance or code is required", key)
			}
			if allocation.Balance == "" {
				allocation.Balance = "0x0"
			}
			normalized = allocKey(address)
		}
		if _, exists := document.Alloc[normalized]; exists {
			return nil, fmt.Errorf("genesis template allocation %s: the address is already allocated", key)
		}
		document.Alloc[normalized] = allocation
	}
	for _, address := range funded {
		if _, exists := document.Alloc[allocKey(address)]; exists {
			return nil, fmt.Errorf("genesis: generated address %s collides with a template allocation", address.Hex())
		}
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

// TemplateFundsGenesisAccount reports whether the template carries the
// $genesis-funds allocation. create prints a bombard note when it does not.
func TemplateFundsGenesisAccount(template []byte) bool {
	var document struct {
		Alloc map[string]json.RawMessage `json:"alloc"`
	}
	if err := json.Unmarshal(template, &document); err != nil {
		return false
	}
	_, ok := document.Alloc[GenesisFundsPlaceholder]
	return ok
}

// decodeAllocation parses one template allocation strictly: a field outside
// balance/code/storage/nonce is an error, because the typed struct would
// otherwise drop it from the rendered genesis without a trace.
func decodeAllocation(key string, raw json.RawMessage) (genesisAllocation, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var allocation genesisAllocation
	if err := decoder.Decode(&allocation); err != nil {
		return genesisAllocation{}, fmt.Errorf("genesis template allocation %q: %w", key, err)
	}
	return allocation, nil
}

func parseAllocAddress(key string) (ethcommon.Address, error) {
	trimmed := strings.TrimPrefix(key, "0x")
	raw, err := hex.DecodeString(trimmed)
	if err != nil || len(raw) != ethcommon.AddressLength {
		return ethcommon.Address{}, fmt.Errorf("genesis template allocation %q: the key must be a 20-byte hex address or the %s placeholder", key, GenesisFundsPlaceholder)
	}
	return ethcommon.BytesToAddress(raw), nil
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
