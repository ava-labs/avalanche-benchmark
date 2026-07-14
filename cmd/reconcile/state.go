package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// loadIntents reads the intentions JSON. If the file is absent it returns the
// default seed (first-ever run behaves like a fresh deploy). A single-site
// file read in two-site mode is migrated by appending the site-B seed. A file
// in the pre-weights key-swap format (entries carrying "key") is rejected:
// that deployment's subnet cannot be managed by this version at all. A file
// carrying the retired "weight" field loads fine: the field is simply
// ignored (weights are on-chain state now, owned by cmd/l1).
func loadIntents(path string, t Topology) ([]MachineIntent, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return seedIntents(t), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var raw []struct {
		Cordoned bool `json:"cordoned"`
		Key      int  `json:"key"` // pre-weights format marker
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, r := range raw {
		if r.Key != 0 {
			return nil, fmt.Errorf(
				"%s is in the old key-swap format; its subnet predates managed weights and cannot be managed by this version - redeploy fresh (run/01_deploy.sh) or remove the state file",
				path)
		}
	}
	intents := make([]MachineIntent, len(raw))
	for i, r := range raw {
		intents[i] = MachineIntent{Cordoned: r.Cordoned}
	}
	if t.TwoSite && len(intents) == t.SitePool() {
		intents = append(intents, seedIntents(t)[t.SitePool():]...)
	}
	if len(intents) != t.Size() {
		return nil, fmt.Errorf("%s has %d machines, expected %d", path, len(intents), t.Size())
	}
	return intents, nil
}

// saveIntents writes the intentions JSON (pretty-printed, the sole persisted state).
func saveIntents(path string, intents []MachineIntent) error {
	data, err := json.MarshalIndent(intents, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// setCordon returns a copy of prev with machine m's (1-based) cordon flag set.
// Cordon is the pure hardware-reachability axis: it never touches on-chain
// weight (that is cmd/l1's separate, deliberate axis); a box going up or down
// never moves stake on its own.
func setCordon(prev []MachineIntent, m int, cordon bool) ([]MachineIntent, error) {
	if m < 1 || m > len(prev) {
		return nil, fmt.Errorf("machine %d out of range 1..%d", m, len(prev))
	}
	next := append([]MachineIntent{}, prev...)
	next[m-1].Cordoned = cordon
	return next, nil
}
