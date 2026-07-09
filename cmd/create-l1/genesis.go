package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"

	ethcommon "github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
)

// The committed genesis.json is a TEMPLATE: it prefunds the well-known ewoq
// test address (below) as a documented placeholder. templateGenesis swaps that
// placeholder for the deploy's own wallet address at chain-creation time, so
// the publicly-known ewoq key never controls funds on a live chain. Templating
// changes the genesis bytes and therefore the chain ID, which is why it runs
// before IssueCreateChainTx and never against an existing chain (chains
// created before this template keep their committed ewoq genesis; bombard
// them with -ewoq).
var ewoqAddr = ethcommon.HexToAddress("0x8db97C7cEcE249c2b98bDC0226Cc4C2A57BF52FC")

// mappingSlotScan bounds the Solidity storage-slot indices we probe when
// relocating address-keyed mapping entries (the ERC20 balances mapping sits at
// slot 0; 16 leaves generous headroom for future template contracts).
const mappingSlotScan = 16

// mappingSlot computes the storage slot of mapping[addr] for a mapping at the
// given slot index: keccak256(pad32(addr) || pad32(slotIndex)) per the
// Solidity storage layout.
func mappingSlot(addr ethcommon.Address, slotIndex uint64) ethcommon.Hash {
	var buf [64]byte
	copy(buf[12:32], addr.Bytes())
	binary.BigEndian.PutUint64(buf[56:64], slotIndex)
	return ethcommon.BytesToHash(crypto.Keccak256(buf[:]))
}

// templateGenesis rewrites the genesis template so every prefund keyed to the
// ewoq placeholder (the alloc EOA balance and any address-keyed mapping slots
// in contract storage, e.g. the ERC20 balances entry) belongs to addr instead.
// Non-address-keyed storage (totalSupply etc.) is untouched.
func templateGenesis(genesisBytes []byte, addr ethcommon.Address) ([]byte, error) {
	var g map[string]any
	if err := json.Unmarshal(genesisBytes, &g); err != nil {
		return nil, fmt.Errorf("parse genesis: %w", err)
	}
	alloc, ok := g["alloc"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("genesis has no alloc object")
	}

	// Move the prefunded EOA from the placeholder to the wallet address.
	moved := false
	for k, v := range alloc {
		if ethcommon.HexToAddress(k) == ewoqAddr {
			delete(alloc, k)
			alloc[strings.TrimPrefix(addr.Hex(), "0x")] = v
			moved = true
			break
		}
	}
	if !moved {
		return nil, fmt.Errorf("genesis template: placeholder %s not found in alloc", ewoqAddr.Hex())
	}

	// Re-key every placeholder-derived mapping slot in every contract's storage.
	for _, v := range alloc {
		acct, _ := v.(map[string]any)
		storage, _ := acct["storage"].(map[string]any)
		for key, val := range storage {
			for i := uint64(0); i < mappingSlotScan; i++ {
				if ethcommon.HexToHash(key) == mappingSlot(ewoqAddr, i) {
					delete(storage, key)
					storage[mappingSlot(addr, i).Hex()] = val
					break
				}
			}
		}
	}
	return json.MarshalIndent(g, "", "    ")
}
