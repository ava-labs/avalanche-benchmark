package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// loadIntents reads the intentions JSON. If the file is absent it returns the
// default seed (first-ever run behaves like a fresh deploy of the mapping).
func loadIntents(path string) ([]MachineIntent, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return seedIntents(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var intents []MachineIntent
	if err := json.Unmarshal(data, &intents); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(intents) != poolSize {
		return nil, fmt.Errorf("%s has %d machines, expected %d", path, len(intents), poolSize)
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
// key mapping from the previous intents, and returns the new intents.
func retarget(prev []MachineIntent, m int, cordon bool) ([]MachineIntent, error) {
	if m < 1 || m > len(prev) {
		return nil, fmt.Errorf("machine %d out of range 1..%d", m, len(prev))
	}
	cordoned := make([]bool, len(prev))
	prevKey := make([]int, len(prev))
	for i, in := range prev {
		cordoned[i] = in.Cordoned
		prevKey[i] = in.Key
	}
	cordoned[m-1] = cordon

	newKeys := ComputeMapping(cordoned, prevKey)
	next := make([]MachineIntent, len(prev))
	for i := range next {
		next[i] = MachineIntent{Cordoned: cordoned[i], Key: newKeys[i]}
	}
	return next, nil
}
