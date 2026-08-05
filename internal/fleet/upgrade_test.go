package fleet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanchego/ids"
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

// The history is append-only because subnet-evm refuses a file that lost or
// changed an activated entry. The merge must preserve existing entries byte
// for byte, order every new entry strictly after them, and never let a
// fragment rewrite history.
func TestMergeUpgradesIsAppendOnly(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	first := upgradeJSON(t, now.Add(time.Hour).Unix(), map[string]any{
		"0xAAA0000000000000000000000000000000000001": map[string]any{"code": "0x6001"},
	})

	// First fragment with no history: the history IS the fragment.
	merged, err := mergeUpgrades(nil, first, 14, now)
	if err != nil {
		t.Fatal(err)
	}
	var mergedFile rawUpgradeFile
	if err := json.Unmarshal(merged, &mergedFile); err != nil {
		t.Fatal(err)
	}
	if len(mergedFile.StateUpgrades) != 1 {
		t.Fatalf("state upgrades = %d, want 1", len(mergedFile.StateUpgrades))
	}

	// A later second fragment appends, and the first entry survives verbatim.
	second := upgradeJSON(t, now.Add(2*time.Hour).Unix(), map[string]any{
		"0xBBB0000000000000000000000000000000000002": map[string]any{"code": "0x6002"},
	})
	twice, err := mergeUpgrades(merged, second, 14, now)
	if err != nil {
		t.Fatal(err)
	}
	var twiceFile rawUpgradeFile
	if err := json.Unmarshal(twice, &twiceFile); err != nil {
		t.Fatal(err)
	}
	if len(twiceFile.StateUpgrades) != 2 {
		t.Fatalf("state upgrades = %d, want 2", len(twiceFile.StateUpgrades))
	}
	var original, kept map[string]any
	if err := json.Unmarshal(mergedFile.StateUpgrades[0], &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(twiceFile.StateUpgrades[0], &kept); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(kept) != fmt.Sprint(original) {
		t.Fatalf("merge changed an existing entry:\nbefore %v\nafter  %v", original, kept)
	}

	// A fragment that does not activate strictly after the history is refused.
	stale := upgradeJSON(t, now.Add(time.Hour).Unix(), map[string]any{
		"0xCCC0000000000000000000000000000000000003": map[string]any{"code": "0x6003"},
	})
	if _, err := mergeUpgrades(twice, stale, 14, now); err == nil || !strings.Contains(err.Error(), "strictly after") {
		t.Fatalf("stale fragment error = %v, want a strictly-after refusal", err)
	}
}

// A history with an already-activated (past) entry must still accept a new
// future fragment: the past entries belong to the chain now and only the
// fragment is held to the future-timestamp rule.
func TestMergeUpgradesKeepsActivatedHistory(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	activated := upgradeJSON(t, now.Add(-time.Hour).Unix(), map[string]any{
		"0xAAA0000000000000000000000000000000000001": map[string]any{"code": "0x6001"},
	})
	fragment := upgradeJSON(t, now.Add(time.Hour).Unix(), map[string]any{
		"0xBBB0000000000000000000000000000000000002": map[string]any{"code": "0x6002"},
	})
	merged, err := mergeUpgrades(activated, fragment, 14, now)
	if err != nil {
		t.Fatalf("a past entry in the HISTORY must not block a future fragment: %v", err)
	}
	var mergedFile rawUpgradeFile
	if err := json.Unmarshal(merged, &mergedFile); err != nil {
		t.Fatal(err)
	}
	if len(mergedFile.StateUpgrades) != 2 {
		t.Fatalf("state upgrades = %d, want 2", len(mergedFile.StateUpgrades))
	}
}

// Every deploy must carry the recorded upgrade history to main-L1 nodes: a
// fresh machine deployed after an activation cannot join the chain without
// the activated entries. The pchain and oracle renders must not carry it
// (wrong chain).
func TestRenderNodeCarriesUpgradeHistory(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "node-config.json"), "{}")
	writeTestFile(t, filepath.Join(root, "chain-config.json"), "{}")
	writeTestFile(t, filepath.Join(root, "chain-config-rpc.json"), "{}")
	writeTestFile(t, filepath.Join(root, "subnet-config.json"), "{}")
	writeTestFile(t, filepath.Join(root, "subnet-config-oracle.json"), "{}")
	if err := os.MkdirAll(filepath.Join(root, "deployment"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, upgradesPath(root), `{"stateUpgrades":[{"blockTimestamp":1,"accounts":{}}]}`)

	environment := config.FleetEnvironment{SSHUser: "op"}
	for name, testCase := range map[string]struct {
		node config.Node
		want bool
	}{
		"validator carries the history": {config.Node{Number: 1, Host: "v1", Role: config.RoleValidator}, true},
		"rpc carries the history":       {config.Node{Number: 9, Host: "r1", Role: config.RoleRPC}, true},
		"oracle node does not":          {config.Node{Number: 16, Host: "o1", Role: config.RoleOracleValidator}, false},
		"pchain does not":               {config.Node{Number: 13, Host: "p1", Role: config.RolePChain}, false},
	} {
		t.Run(name, func(t *testing.T) {
			renderDir := filepath.Join(t.TempDir(), "render")
			if err := renderNode(
				renderDir, root, environment, testCase.node,
				creation.PublicNode{Identity: "a", Role: testCase.node.Role},
				ids.GenerateTestID(), ids.GenerateTestID(), [2]int{9650, 9651},
				frozenMode, "", "", "", "",
			); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(filepath.Join(renderDir, "upgrade.json"))
			if testCase.want && err != nil {
				t.Fatalf("render did not carry the upgrade history: %v", err)
			}
			if !testCase.want && err == nil {
				t.Fatal("render carried the upgrade history to the wrong chain")
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
