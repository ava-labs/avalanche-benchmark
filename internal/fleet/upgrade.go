package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// upgradeFile is the subset of a subnet-evm upgrade.json the kit validates.
// Unknown fields pass through untouched: the file, not this struct, is what
// gets installed.
type upgradeFile struct {
	StateUpgrades []struct {
		BlockTimestamp int64 `json:"blockTimestamp"`
		Accounts       map[string]struct {
			Code    string            `json:"code"`
			Storage map[string]string `json:"storage"`
		} `json:"accounts"`
	} `json:"stateUpgrades"`
	PrecompileUpgrades []json.RawMessage `json:"precompileUpgrades"`
}

// validateUpgradeContents refuses the mistakes that pass a first restart and
// hurt later. The explicit-zero rule exists because subnet-evm reads a zero
// back from the database as absent, so a node that restarts after activation
// fails the config's deep-equal check and stays down (observed in the field,
// 2026-08-03). The future-timestamp rule exists because a node refuses an
// upgrade that is already in the past when it loads it, and every node must
// restart with the file BEFORE activation.
func validateUpgradeContents(contents []byte, nodeCount int, now time.Time) error {
	var upgrade upgradeFile
	if err := json.Unmarshal(contents, &upgrade); err != nil {
		return fmt.Errorf("upgrade file is not valid JSON: %w", err)
	}
	if len(upgrade.StateUpgrades) == 0 && len(upgrade.PrecompileUpgrades) == 0 {
		return fmt.Errorf("upgrade file has neither stateUpgrades nor precompileUpgrades")
	}
	for index, stateUpgrade := range upgrade.StateUpgrades {
		if stateUpgrade.BlockTimestamp <= now.Unix() {
			return fmt.Errorf("stateUpgrades[%d] activates at %d, which is not in the future; every node must restart with the file before activation", index, stateUpgrade.BlockTimestamp)
		}
		// One node restart takes tens of seconds plus its readiness wait.
		if stateUpgrade.BlockTimestamp < now.Add(time.Duration(nodeCount)*30*time.Second).Unix() {
			fmt.Fprintf(os.Stderr, "warning: stateUpgrades[%d] activates in %ds, which may not cover a rolling restart of %d node(s)\n",
				index, stateUpgrade.BlockTimestamp-now.Unix(), nodeCount)
		}
		if len(stateUpgrade.Accounts) == 0 {
			return fmt.Errorf("stateUpgrades[%d] has no accounts", index)
		}
		for address, account := range stateUpgrade.Accounts {
			if account.Code == "" || account.Code == "0x" {
				if len(account.Storage) == 0 {
					return fmt.Errorf("stateUpgrades[%d] account %s sets neither code nor storage", index, address)
				}
			}
			if account.Code == "0x" {
				return fmt.Errorf("stateUpgrades[%d] account %s sets explicitly empty code; omit the field instead (an explicit zero value bricks the node on the restart after activation)", index, address)
			}
			for slot, value := range account.Storage {
				trimmed := strings.TrimPrefix(strings.ToLower(value), "0x")
				if strings.Trim(trimmed, "0") == "" {
					return fmt.Errorf("stateUpgrades[%d] account %s sets slot %s to zero; omit the entry instead (an explicit zero value bricks the node on the restart after activation)", index, address, slot)
				}
			}
		}
	}
	return nil
}

// rawUpgradeFile carries upgrade entries as raw JSON, so a merge preserves
// already-activated entries byte for byte. subnet-evm treats the file as
// append-only history: an entry that has activated must stay in the file
// unchanged forever, or every node refuses to start.
type rawUpgradeFile struct {
	StateUpgrades      []json.RawMessage `json:"stateUpgrades,omitempty"`
	PrecompileUpgrades []json.RawMessage `json:"precompileUpgrades,omitempty"`
}

// upgradesPath is the control-side canonical upgrade history for the main
// L1. Apps emit FRAGMENTS (one new upgrade each); this file accumulates
// them, fleet upgrade distributes it whole, and fleet deploy installs it on
// every fresh node so a machine deployed after an activation still carries
// the full history.
func upgradesPath(root string) string {
	return filepath.Join(root, "deployment", "upgrades.json")
}

// stateUpgradeTimestamps extracts blockTimestamp from each raw entry.
func stateUpgradeTimestamps(entries []json.RawMessage) ([]int64, error) {
	timestamps := make([]int64, 0, len(entries))
	for index, entry := range entries {
		var head struct {
			BlockTimestamp int64 `json:"blockTimestamp"`
		}
		if err := json.Unmarshal(entry, &head); err != nil {
			return nil, fmt.Errorf("stateUpgrades[%d] is not an object: %w", index, err)
		}
		timestamps = append(timestamps, head.BlockTimestamp)
	}
	return timestamps, nil
}

// mergeUpgrades appends a validated fragment to the existing history.
// Existing entries pass through untouched (they may be activated already, so
// their timestamps are allowed to be in the past); every fragment entry must
// activate strictly after every existing entry, because subnet-evm requires
// the file ordered by timestamp and refuses changes behind the head.
func mergeUpgrades(existing, fragment []byte, nodeCount int, now time.Time) ([]byte, error) {
	if err := validateUpgradeContents(fragment, nodeCount, now); err != nil {
		return nil, err
	}
	var fragmentFile rawUpgradeFile
	if err := json.Unmarshal(fragment, &fragmentFile); err != nil {
		return nil, fmt.Errorf("upgrade fragment is not valid JSON: %w", err)
	}
	merged := fragmentFile
	if len(existing) > 0 {
		var existingFile rawUpgradeFile
		if err := json.Unmarshal(existing, &existingFile); err != nil {
			return nil, fmt.Errorf("deployment/upgrades.json is not valid JSON: %w", err)
		}
		existingTimestamps, err := stateUpgradeTimestamps(existingFile.StateUpgrades)
		if err != nil {
			return nil, fmt.Errorf("deployment/upgrades.json: %w", err)
		}
		fragmentTimestamps, err := stateUpgradeTimestamps(fragmentFile.StateUpgrades)
		if err != nil {
			return nil, fmt.Errorf("upgrade fragment: %w", err)
		}
		for _, existingTimestamp := range existingTimestamps {
			for _, fragmentTimestamp := range fragmentTimestamps {
				if fragmentTimestamp <= existingTimestamp {
					return nil, fmt.Errorf(
						"fragment activates at %d, but the history already has an entry at %d; a new upgrade must activate strictly after every recorded one",
						fragmentTimestamp, existingTimestamp)
				}
			}
		}
		merged = rawUpgradeFile{
			StateUpgrades:      append(existingFile.StateUpgrades, fragmentFile.StateUpgrades...),
			PrecompileUpgrades: append(existingFile.PrecompileUpgrades, fragmentFile.PrecompileUpgrades...),
		}
	}
	contents, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(contents, '\n'), nil
}

// UpgradeChain appends an upgrade fragment to the canonical history in
// deployment/upgrades.json, installs the FULL history on every main-L1
// node, and restarts them one at a time. The history, not the fragment, is
// what lands on the nodes: subnet-evm's upgrade file is append-only, and a
// file missing an already-activated entry stops every node it reaches. The
// history reaches EVERY node before the first restart, so a failure in the
// push phase changes no running process; the restarts are sequential with a
// readiness wait, the same discipline as a rolling deploy.
func (d *Deployer) UpgradeChain(ctx context.Context, path string) error {
	fragment, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read upgrade fragment: %w", err)
	}
	var existing []byte
	canonical := upgradesPath(d.root)
	if contents, err := os.ReadFile(canonical); err == nil {
		existing = contents
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", canonical, err)
	}

	state, inv, err := d.lifecycleTargets(nil)
	if err != nil {
		return err
	}
	if !inv.created {
		return fmt.Errorf("fleet upgrade requires a complete deployment/network.env; run l1 create first")
	}
	// The upgrade file is per chain. This command targets the main L1; the
	// optional oracle chain keeps its own file and is out of scope here.
	targets := make([]nodeDeployment, 0, len(state.selected))
	for _, target := range state.selected {
		if oracleRole(target.node.Role) {
			continue
		}
		targets = append(targets, target)
	}
	merged, err := mergeUpgrades(existing, fragment, len(targets), time.Now())
	if err != nil {
		return err
	}

	// Control-side state is recorded atomically before any remote work, the
	// same convention as every other mutating command: a rerun after a
	// mid-push failure converges from the history.
	temporary := canonical + ".tmp"
	if err := os.WriteFile(temporary, merged, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, canonical); err != nil {
		return err
	}
	fmt.Fprintf(d.out, "recorded the upgrade in %s\n", canonical)

	// Push phase: the full history reaches every node before any restart.
	l := layoutFor(inv.environment)
	push := state
	push.selected = targets
	if err := d.phaseParallel(ctx, push, "upgrade push", func(ctx context.Context, dep deployment, node nodeDeployment) error {
		stage := stagingDir(dep.environment.SSHUser, node, "upgrade")
		if err := d.runSSH(ctx, dep, node, "rm -rf "+stage+" && mkdir -m 700 "+stage); err != nil {
			return err
		}
		if err := d.rsyncFile(ctx, dep, node, canonical, stage); err != nil {
			return err
		}
		chainDir := fmt.Sprintf("%s/%d/chains/%s", l.cfg, node.node.Number, inv.chainID)
		return d.runSSH(ctx, dep, node, strings.Join([]string{
			l.sudo(fmt.Sprintf("install -d -m 0755 %s", chainDir)),
			l.sudo(fmt.Sprintf("install -m 0644 %s/upgrades.json %s/upgrade.json", stage, chainDir)),
			"rm -rf " + stage,
		}, " && "))
	}); err != nil {
		return fmt.Errorf("upgrade push failed and no node was restarted: %w", err)
	}
	fmt.Fprintf(d.out, "upgrade history installed on %d node(s); rolling restart\n", len(targets))

	// Restart phase: one node fully back before the next goes down.
	for _, target := range targets {
		fmt.Fprintf(d.out, "== node %d (%s)\n", target.node.Number, target.node.Host)
		one := state
		one.selected = []nodeDeployment{target}
		for _, phase := range []lifecyclePhase{
			{"stop", d.stop},
			{"start", d.enableAndStart},
			{"readiness", d.waitL1Ready},
		} {
			if err := d.phase(ctx, one, phase.name, phase.action); err != nil {
				return err
			}
		}
	}
	fmt.Fprintf(d.out, "restarted %d node(s) with the upgrade installed\n", len(targets))
	fmt.Fprintln(d.out, "verify after the activation timestamp, for example with a cast call against the upgraded contract")
	return nil
}
