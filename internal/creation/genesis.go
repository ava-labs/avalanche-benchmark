package creation

import (
	"encoding/json"
	"fmt"
	"strings"

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

func RenderGenesis(template []byte, genesisAddress ethcommon.Address) ([]byte, error) {
	var document genesisDocument
	if err := json.Unmarshal(template, &document); err != nil {
		return nil, fmt.Errorf("parse genesis template: %w", err)
	}
	if len(document.Config) == 0 {
		return nil, fmt.Errorf("genesis template: required config is not provided")
	}
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
