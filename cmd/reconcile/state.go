package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

// loadIntents reads the intentions JSON. If the file is absent it returns the
// default seed (first-ever run behaves like a fresh deploy). A single-site
// file read in two-site mode is migrated by appending the site-B seed. A file
// in the pre-weights key-swap format (entries carrying "key") is rejected:
// that deployment's subnet has no ValidatorManager and cannot be managed by
// this version at all.
func loadIntents(path string, t Topology) ([]MachineIntent, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return seedIntents(t), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var raw []struct {
		Cordoned bool   `json:"cordoned"`
		Weight   uint64 `json:"weight"`
		Key      int    `json:"key"` // pre-weights format marker
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, r := range raw {
		if r.Key != 0 {
			return nil, fmt.Errorf(
				"%s is in the old key-swap format; its subnet predates C-chain managed weights and cannot be managed by this version — redeploy fresh (03_deploy_chain.sh) or remove the state file",
				path)
		}
	}
	intents := make([]MachineIntent, len(raw))
	for i, r := range raw {
		intents[i] = MachineIntent{Cordoned: r.Cordoned, Weight: r.Weight}
	}
	if t.TwoSite && len(intents) == t.SitePool() {
		intents = append(intents, seedIntents(t)[t.SitePool():]...)
	}
	if len(intents) != t.Size() {
		return nil, fmt.Errorf("%s has %d machines, expected %d", path, len(intents), t.Size())
	}
	for i, in := range intents {
		if t.IsStakingSlot(i) && in.Weight == 0 {
			return nil, fmt.Errorf("%s: %s is a registered validator and must have weight >= 1 (weight 0 means removal, which we never do)", path, t.MachineName(i))
		}
		if !t.IsStakingSlot(i) && in.Weight != 0 {
			return nil, fmt.Errorf("%s: %s is an RPC slot and is not registered; weight must be 0", path, t.MachineName(i))
		}
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

// retarget toggles machine m's cordon flag (1-based) and applies the weight
// policy for a single-machine fault, never crossing sites:
//   - cordoning a slot that holds active weight hands that weight to an
//     uncordoned standby staking slot in the SAME site (spares first, then
//     any), and parks the cordoned slot at StandbyWeight. With no free slot
//     the weight simply drops to standby (quorum may degrade; reported).
//   - uncordoning brings the machine back as a standby: weight placement is
//     sticky, so the earlier promotion is not flapped back automatically
//     (use restore/site-failover for deliberate weight moves).
func retarget(prev []MachineIntent, m int, cordon bool, t Topology) ([]MachineIntent, error) {
	if m < 1 || m > len(prev) {
		return nil, fmt.Errorf("machine %d out of range 1..%d", m, len(prev))
	}
	next := append([]MachineIntent{}, prev...)
	i := m - 1
	next[i].Cordoned = cordon

	total := totalWeight(prev)
	if cordon && t.IsStakingSlot(i) && isActiveWeight(next[i].Weight, total) {
		if j := promotionTarget(next, t, t.Site(i)); j >= 0 {
			next[j].Weight = next[i].Weight
			fmt.Printf("  weight: %s -> %s (same-site standby promoted)\n", t.MachineName(i), t.MachineName(j))
		} else {
			fmt.Printf("  weight: no free standby in site %s — %s's weight drops to standby (quorum may degrade)\n",
				siteName(t.Site(i)), t.MachineName(i))
		}
		next[i].Weight = valmgr.StandbyWeight
	}
	return next, nil
}

// promotionTarget picks the same-site uncordoned staking slot to hand active
// weight to: spare slots first, then any standby, lowest slot number first.
func promotionTarget(intents []MachineIntent, t Topology, site int) int {
	total := totalWeight(intents)
	pick := -1
	for i, in := range intents {
		if in.Cordoned || t.Site(i) != site || !t.IsStakingSlot(i) || isActiveWeight(in.Weight, total) {
			continue
		}
		if t.IsSpareSlot(i) {
			return i
		}
		if pick < 0 {
			pick = i
		}
	}
	return pick
}

// retargetSite is the hard DC failover: the target site's validator slots get
// ActiveWeight, its spares StandbyWeight, and the whole other site is
// cordoned (presumed dead) at StandbyWeight. The weight seesaw itself is
// executed later by the weight reconciler (raises before lowers).
func retargetSite(prev []MachineIntent, t Topology, target int) ([]MachineIntent, error) {
	if !t.TwoSite {
		return nil, fmt.Errorf("site failover requires the BACKUP_* site to be configured")
	}
	next := make([]MachineIntent, len(prev))
	for i := range prev {
		next[i] = MachineIntent{
			Cordoned: t.Site(i) != target,
			Weight:   siteWeights(t, i, target),
		}
	}
	return next, nil
}

// retargetRestore is the graceful return: BOTH sites stay up (the returning
// site rejoins as standby trackers first; the caller waits for it to be
// consensus-ready before the weight seesaw shifts the set).
func retargetRestore(prev []MachineIntent, t Topology, target int) ([]MachineIntent, error) {
	if !t.TwoSite {
		return nil, fmt.Errorf("restore requires the BACKUP_* site to be configured")
	}
	next := make([]MachineIntent, len(prev))
	for i := range prev {
		next[i] = MachineIntent{Cordoned: false, Weight: siteWeights(t, i, target)}
	}
	return next, nil
}

// siteWeights is the steady-state weight layout with the given site active.
func siteWeights(t Topology, i, activeSite int) uint64 {
	switch {
	case !t.IsStakingSlot(i):
		return 0
	case t.Site(i) == activeSite && t.IsValidatorSlot(i):
		return valmgr.ActiveWeight
	default:
		return valmgr.StandbyWeight
	}
}
