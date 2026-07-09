package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	ethcommon "github.com/ava-labs/libevm/common"
)

// TestMappingSlotReproducesCommittedGenesis proves the slot formula against
// the ewoq-derived keys committed in genesis.json: keccak256(pad32(addr) ||
// pad32(slotIndex)) must reproduce the ERC20 balances entry exactly.
func TestMappingSlotReproducesCommittedGenesis(t *testing.T) {
	for _, tc := range []struct {
		addr ethcommon.Address
		slot uint64
		want string
	}{
		// genesis.json ERC20 storage: balances[ewoq] at mapping slot 0.
		{ewoqAddr, 0, "0x8752f2ce489c60adfebb82af1ee397b7cda0e7af19fe4d57af54dd0cbf417866"},
		// Independent vector: keccak256(pad32(0x00..01) || pad32(0)), the
		// canonical mapping-slot example value.
		{ethcommon.HexToAddress("0x0000000000000000000000000000000000000001"), 0,
			"0xada5013122d395ba3c54772283fb069b10426056ef8ca54750cb9bb552a59e7d"},
	} {
		if got := mappingSlot(tc.addr, tc.slot).Hex(); got != tc.want {
			t.Errorf("mappingSlot(%s, %d) = %s, want %s", tc.addr.Hex(), tc.slot, got, tc.want)
		}
	}
}

func TestTemplateGenesis(t *testing.T) {
	raw, err := os.ReadFile("../../genesis.json")
	if err != nil {
		t.Fatal(err)
	}
	newAddr := ethcommon.HexToAddress("0x1111111111111111111111111111111111111111")
	out, err := templateGenesis(raw, newAddr)
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Alloc map[string]struct {
			Balance string            `json:"balance"`
			Storage map[string]string `json:"storage"`
		} `json:"alloc"`
	}
	if err := json.Unmarshal(out, &g); err != nil {
		t.Fatal(err)
	}

	maxBal := "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"
	for k := range g.Alloc {
		if ethcommon.HexToAddress(k) == ewoqAddr {
			t.Errorf("ewoq placeholder still in alloc: %s", k)
		}
	}
	eoa, ok := g.Alloc["1111111111111111111111111111111111111111"]
	if !ok || eoa.Balance != maxBal {
		t.Fatalf("wallet EOA prefund missing or wrong: %+v (present=%v)", eoa, ok)
	}

	erc20, ok := g.Alloc["B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5"]
	if !ok {
		t.Fatal("ERC20 contract missing from templated alloc")
	}
	if got := erc20.Storage[mappingSlot(newAddr, 0).Hex()]; got != maxBal {
		t.Errorf("balances[wallet] slot = %q, want max balance", got)
	}
	if _, still := erc20.Storage[mappingSlot(ewoqAddr, 0).Hex()]; still {
		t.Error("ewoq-derived balances slot still present")
	}
	// totalSupply (raw slot 2, not address-keyed) must be untouched.
	if got := erc20.Storage["0x0000000000000000000000000000000000000000000000000000000000000002"]; got != maxBal {
		t.Errorf("totalSupply slot changed: %q", got)
	}

	// Large feeConfig numbers must survive the round-trip byte-exact. A
	// float64-based decode mangles targetGas 2^64-1 to 18446744073709552000,
	// which the VM wraps to 384 and the base fee creeps +1 wei per block.
	for _, n := range []string{"18446744073709551615", "9223372036854775807"} {
		if !bytes.Contains(out, []byte(n)) {
			t.Errorf("feeConfig value %s did not survive templating byte-exact", n)
		}
	}
}
