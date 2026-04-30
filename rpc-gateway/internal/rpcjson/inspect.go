package rpcjson

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/ava-labs/avalanche-benchmark/rpc-gateway/internal/policy"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
)

type callArg struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Data  string `json:"data"`
	Input string `json:"input"`
	Value string `json:"value"`
	Gas   string `json:"gas"`
}

func ExtractRawTransaction(params json.RawMessage) (policy.TransactionAttributes, error) {
	var values []string
	if err := json.Unmarshal(params, &values); err != nil {
		return policy.TransactionAttributes{}, fmt.Errorf("eth_sendRawTransaction expects params: [rawTxHex]")
	}
	if len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return policy.TransactionAttributes{}, fmt.Errorf("eth_sendRawTransaction requires a raw transaction")
	}

	payload, err := hexutil.Decode(values[0])
	if err != nil {
		return policy.TransactionAttributes{}, fmt.Errorf("invalid raw transaction hex: %w", err)
	}

	var tx types.Transaction
	if err := tx.UnmarshalBinary(payload); err != nil {
		return policy.TransactionAttributes{}, fmt.Errorf("failed to decode raw transaction: %w", err)
	}

	signer := types.LatestSignerForChainID(tx.ChainId())
	from, err := types.Sender(signer, &tx)
	if err != nil {
		return policy.TransactionAttributes{}, fmt.Errorf("failed to recover transaction sender: %w", err)
	}

	to := ""
	contractCreation := tx.To() == nil
	if tx.To() != nil {
		to = tx.To().Hex()
	}

	value := tx.Value()
	if value == nil {
		value = big.NewInt(0)
	}

	return policy.TransactionAttributes{
		ChainID:          tx.ChainId().String(),
		From:             from.Hex(),
		To:               to,
		Selector:         selectorFromData(tx.Data()),
		GasLimit:         tx.Gas(),
		Value:            value,
		ContractCreation: contractCreation,
	}, nil
}

func ExtractCall(params json.RawMessage) (policy.CallAttributes, error) {
	var rawParams []json.RawMessage
	if err := json.Unmarshal(params, &rawParams); err != nil {
		return policy.CallAttributes{}, fmt.Errorf("call-style methods expect params: [callObject, ...]")
	}
	if len(rawParams) == 0 {
		return policy.CallAttributes{}, fmt.Errorf("call-style methods require a call object")
	}

	var arg callArg
	if err := json.Unmarshal(rawParams[0], &arg); err != nil {
		return policy.CallAttributes{}, fmt.Errorf("invalid call object: %w", err)
	}

	value, err := parseOptionalBigInt(arg.Value)
	if err != nil {
		return policy.CallAttributes{}, fmt.Errorf("invalid value: %w", err)
	}

	gasLimit, err := parseOptionalUint64(arg.Gas)
	if err != nil {
		return policy.CallAttributes{}, fmt.Errorf("invalid gas: %w", err)
	}

	data := arg.Data
	if strings.TrimSpace(data) == "" {
		data = arg.Input
	}

	return policy.CallAttributes{
		From:     arg.From,
		To:       arg.To,
		Selector: selectorFromHexData(data),
		GasLimit: gasLimit,
		Value:    value,
	}, nil
}

func parseOptionalBigInt(raw string) (*big.Int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return big.NewInt(0), nil
	}

	value := new(big.Int)
	var ok bool
	if strings.HasPrefix(strings.ToLower(raw), "0x") {
		value, ok = value.SetString(strings.TrimPrefix(strings.ToLower(raw), "0x"), 16)
	} else {
		value, ok = value.SetString(raw, 10)
	}
	if !ok {
		return nil, fmt.Errorf("invalid integer")
	}
	return value, nil
}

func parseOptionalUint64(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}

	value, err := parseOptionalBigInt(raw)
	if err != nil {
		return 0, err
	}
	if !value.IsUint64() {
		return 0, fmt.Errorf("value does not fit in uint64")
	}
	return value.Uint64(), nil
}

func selectorFromHexData(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || raw == "0x" {
		return ""
	}
	raw = strings.TrimPrefix(raw, "0x")
	if len(raw) < 8 {
		return ""
	}
	if _, err := hex.DecodeString(raw[:8]); err != nil {
		return ""
	}
	return "0x" + raw[:8]
}

func selectorFromData(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	return "0x" + hex.EncodeToString(data[:4])
}
