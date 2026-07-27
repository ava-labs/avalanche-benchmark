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
	Balance string `json:"balance"`
}

// RenderGenesis injects the funding allocation and stamps the genesis with the
// creation time.
//
// The timestamp is not cosmetic. Network upgrade times come from the network,
// not from the genesis chain config, so a genesis stamped 0 sits decades before
// Granite activated and Granite is therefore inactive AT GENESIS. Subnet-EVM
// seeds the ACP-226 minimum block delay only inside its Granite branch, so a
// zero timestamp silently discards initialMinDelayMS and the chain starts at
// the 2000ms default, converging down over hours. Stamping creation time keeps
// Granite active from block zero on both Fuji and mainnet.
func RenderGenesis(template []byte, genesisAddress ethcommon.Address, createdAt time.Time) ([]byte, error) {
	var document genesisDocument
	if err := json.Unmarshal(template, &document); err != nil {
		return nil, fmt.Errorf("parse genesis template: %w", err)
	}
	if len(document.Config) == 0 {
		return nil, fmt.Errorf("genesis template: required config is not provided")
	}
	document.Timestamp = fmt.Sprintf("0x%x", createdAt.Unix())
	if len(document.Alloc) != 0 {
		return nil, fmt.Errorf("genesis template: alloc must be empty before funding-key injection")
	}
	address := strings.TrimPrefix(genesisAddress.Hex(), "0x")
	document.Alloc = map[string]genesisAllocation{
		address: {Balance: genesisBalance},
	}
	genesis, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render genesis: %w", err)
	}
	return append(genesis, '\n'), nil
}
