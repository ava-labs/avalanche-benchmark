package fleet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	ethcommon "github.com/ava-labs/libevm/common"
)

func upgradeJSON(t *testing.T, timestamp int64, accounts map[string]any) []byte {
	t.Helper()
	contents, err := json.Marshal(map[string]any{
		"stateUpgrades": []any{map[string]any{
			"blockTimestamp": timestamp,
			"accounts":       accounts,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

// The explicit-zero rule is the whole point of validating at all: a zero
// value passes the first restart and bricks the node on the next one, after
// activation, when the deep-equal check reads the zero back as absent.
func TestValidateUpgradeContentsRefusesTheRestartBrick(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	future := now.Add(time.Hour).Unix()

	for name, testCase := range map[string]struct {
		contents []byte
		wantErr  string
	}{
		"valid code and storage": {
			contents: upgradeJSON(t, future, map[string]any{
				"0x00000000000000000000000000000000FeedFacE": map[string]any{
					"code":    "0x6001",
					"storage": map[string]string{"0x01": "0x02"},
				},
			}),
		},
		"zero storage value": {
			contents: upgradeJSON(t, future, map[string]any{
				"0x00000000000000000000000000000000FeedFacE": map[string]any{
					"code":    "0x6001",
					"storage": map[string]string{"0x01": "0x0000000000000000000000000000000000000000000000000000000000000000"},
				},
			}),
			wantErr: "zero",
		},
		"short zero storage value": {
			contents: upgradeJSON(t, future, map[string]any{
				"0xAAA0000000000000000000000000000000000001": map[string]any{
					"storage": map[string]string{"0x01": "0x0"},
				},
			}),
			wantErr: "zero",
		},
		"explicitly empty code": {
			contents: upgradeJSON(t, future, map[string]any{
				"0xAAA0000000000000000000000000000000000001": map[string]any{
					"code":    "0x",
					"storage": map[string]string{"0x01": "0x02"},
				},
			}),
			wantErr: "empty code",
		},
		"past activation": {
			contents: upgradeJSON(t, now.Unix()-10, map[string]any{
				"0xAAA0000000000000000000000000000000000001": map[string]any{"code": "0x6001"},
			}),
			wantErr: "not in the future",
		},
		"account with nothing": {
			contents: upgradeJSON(t, future, map[string]any{
				"0xAAA0000000000000000000000000000000000001": map[string]any{},
			}),
			wantErr: "neither code nor storage",
		},
		"no upgrades at all": {
			contents: []byte(`{}`),
			wantErr:  "neither stateUpgrades nor precompileUpgrades",
		},
		"not json": {
			contents: []byte(`nope`),
			wantErr:  "not valid JSON",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateUpgradeContents(testCase.contents, 14, now)
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("valid upgrade refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, testCase.wantErr)
			}
		})
	}
}

// The rendered direct-feed upgrade must always pass its own gate: every
// seeded value is non-zero by construction, and this test keeps it that way.
func TestDirectFeedAllocationsRenderAValidUpgrade(t *testing.T) {
	feeder := ethcommon.HexToAddress("0x9d0DF24eBD17211c5C26B79A79Aee6d56B94E6EE")
	accounts := make(map[string]any)
	for _, allocation := range creation.DirectFeedAllocations(feeder) {
		if allocation.RuntimeCode == "" || allocation.RuntimeCode == "0x" {
			t.Fatalf("contract %s has empty runtime code", allocation.Address.Hex())
		}
		storage := make(map[string]string, len(allocation.Storage))
		for slot, value := range allocation.Storage {
			if value == (ethcommon.Hash{}) {
				t.Fatalf("contract %s seeds slot %s with zero", allocation.Address.Hex(), slot.Hex())
			}
			storage[slot.Hex()] = value.Hex()
		}
		accounts[allocation.Address.Hex()] = map[string]any{
			"code":    allocation.RuntimeCode,
			"storage": storage,
		}
	}
	now := time.Unix(1_700_000_000, 0)
	contents := upgradeJSON(t, now.Add(time.Hour).Unix(), accounts)
	if err := validateUpgradeContents(contents, 14, now); err != nil {
		t.Fatalf("the rendered direct-feed upgrade failed its own gate: %v", err)
	}
}
