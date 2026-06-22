package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// loadIntents reads the intentions JSON. If the file is absent it returns the
// default seed (first-ever run behaves like a fresh deploy of the mapping). A
// single-site file (sitePoolSize machines) read in two-site mode is migrated by
// appending the site-B seed (backup site joins as zero-weight trackers, A untouched).
func loadIntents(path string, topo Topology) ([]MachineIntent, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return seedIntents(topo), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var intents []MachineIntent
	if err := json.Unmarshal(data, &intents); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if topo.TwoSite && len(intents) == sitePoolSize {
		intents = append(intents, seedIntents(topo)[sitePoolSize:]...)
	}
	if len(intents) != topo.Size() {
		return nil, fmt.Errorf("%s has %d machines, expected %d", path, len(intents), topo.Size())
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

// retarget mutates the cordon flag of machine m (1-based), recomputes the sticky
// key mapping from the previous intents, and returns the new intents. Orphaned
// validator keys are only reassigned within the toggled machine's own site —
// a single-machine fault never moves consensus across sites.
func retarget(prev []MachineIntent, m int, cordon bool, topo Topology) ([]MachineIntent, error) {
	if m < 1 || m > len(prev) {
		return nil, fmt.Errorf("machine %d out of range 1..%d", m, len(prev))
	}
	cordoned, prevKey := splitIntents(prev)
	cordoned[m-1] = cordon

	return mergeIntents(cordoned, ComputeMapping(topo, cordoned, prevKey, topo.Site(m-1))), nil
}

// retargetSite fails the whole deployment over to the target site: every
// machine in the other site is cordoned, every machine in the target site is
// uncordoned, and the orphaned validator keys land on the target site's free
// machines. This is the only path that moves validator identities across sites.
func retargetSite(prev []MachineIntent, topo Topology, target int) ([]MachineIntent, error) {
	if !topo.TwoSite {
		return nil, fmt.Errorf("site failover requires BACKUP_SITE_NODE_IPS to be configured")
	}
	cordoned, prevKey := splitIntents(prev)
	for i := range cordoned {
		cordoned[i] = topo.Site(i) != target
	}
	return mergeIntents(cordoned, ComputeMapping(topo, cordoned, prevKey, target)), nil
}

func splitIntents(intents []MachineIntent) (cordoned []bool, keys []int) {
	cordoned = make([]bool, len(intents))
	keys = make([]int, len(intents))
	for i, in := range intents {
		cordoned[i] = in.Cordoned
		keys[i] = in.Key
	}
	return cordoned, keys
}

func mergeIntents(cordoned []bool, keys []int) []MachineIntent {
	next := make([]MachineIntent, len(cordoned))
	for i := range next {
		next[i] = MachineIntent{Cordoned: cordoned[i], Key: keys[i]}
	}
	return next
}
