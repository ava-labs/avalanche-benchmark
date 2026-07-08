package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

func stdTopo() Topology    { return Topology{NVal: 3, NSpare: 1, NRPC: 2} }
func stdTwoSite() Topology { return Topology{TwoSite: true, NVal: 3, NSpare: 1, NRPC: 2} }

// TestSetCordon: cordon is the pure hardware axis - it flips the flag and
// leaves weight (and the input slice) untouched.
func TestSetCordon(t *testing.T) {
	topo := stdTopo()
	intents := seedIntents(topo)

	next, err := setCordon(intents, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if !next[0].Cordoned {
		t.Error("a1 should be cordoned")
	}
	if next[0].Weight != intents[0].Weight {
		t.Errorf("cordon changed weight: %d -> %d", intents[0].Weight, next[0].Weight)
	}
	if intents[0].Cordoned {
		t.Error("setCordon mutated its input")
	}

	back, err := setCordon(next, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if back[0].Cordoned {
		t.Error("a1 should be uncordoned")
	}
}

// TestSetWeight: setWeight is pure stake intent - it sets the weight tier and
// leaves cordon (and the input slice) untouched.
func TestSetWeight(t *testing.T) {
	topo := stdTopo()
	intents := seedIntents(topo)

	next, err := setWeight(intents, 4, valmgr.SpareWeight, topo) // demote a4 (seeds at validator tier)
	if err != nil {
		t.Fatal(err)
	}
	if next[3].Weight != valmgr.SpareWeight {
		t.Errorf("a4 weight = %d, want %d", next[3].Weight, valmgr.SpareWeight)
	}
	if next[3].Cordoned != intents[3].Cordoned {
		t.Error("setWeight changed cordon")
	}
	if intents[3].Weight == valmgr.SpareWeight {
		t.Error("setWeight mutated its input")
	}

	// dead tier pulls stake without touching the process
	dead, err := setWeight(next, 4, valmgr.DeadWeight, topo)
	if err != nil {
		t.Fatal(err)
	}
	if dead[3].Weight != valmgr.DeadWeight {
		t.Errorf("a4 weight = %d, want dead %d", dead[3].Weight, valmgr.DeadWeight)
	}
}

func TestSetWeightRejectsRPCSlot(t *testing.T) {
	topo := stdTopo()
	intents := seedIntents(topo)
	rpc := -1
	for i := 0; i < topo.Size(); i++ {
		if topo.IsRPCSlot(i) {
			rpc = i
			break
		}
	}
	if _, err := setWeight(intents, rpc+1, valmgr.SpareWeight, topo); err == nil {
		t.Error("setWeight on an RPC slot must fail (unregistered)")
	}
}

func TestSetOutOfRange(t *testing.T) {
	topo := stdTopo()
	intents := seedIntents(topo)
	if _, err := setCordon(intents, 0, true); err == nil {
		t.Error("machine 0 must be rejected")
	}
	if _, err := setWeight(intents, topo.Size()+1, valmgr.DeadWeight, topo); err == nil {
		t.Error("out-of-range machine must be rejected")
	}
}

func TestLoadIntentsMissingReturnsSeed(t *testing.T) {
	topo := stdTwoSite()
	got, err := loadIntents(filepath.Join(t.TempDir(), "absent.json"), topo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != topo.Size() {
		t.Fatalf("seed size = %d, want %d", len(got), topo.Size())
	}
}

func TestLoadIntentsRoundTrip(t *testing.T) {
	topo := stdTwoSite()
	path := filepath.Join(t.TempDir(), "intents.json")
	want := seedIntents(topo)
	want, _ = setCordon(want, 1, true)
	want, _ = setWeight(want, 4, valmgr.DeadWeight, topo)
	if err := saveIntents(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadIntents(path, topo)
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("slot %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLoadIntentsMigratesSingleSiteFile(t *testing.T) {
	single := stdTopo()
	two := stdTwoSite()
	path := filepath.Join(t.TempDir(), "intents.json")
	if err := saveIntents(path, seedIntents(single)); err != nil {
		t.Fatal(err)
	}
	got, err := loadIntents(path, two)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != two.Size() {
		t.Fatalf("migrated size = %d, want %d", len(got), two.Size())
	}
	for i := two.SitePool(); i < two.Size(); i++ {
		if got[i].Weight != seedWeight(two, i) {
			t.Errorf("appended site-B slot %d weight = %d, want seed", i, got[i].Weight)
		}
	}
}

func TestLoadIntentsRejectsOldKeySwapFormat(t *testing.T) {
	topo := stdTwoSite()
	path := filepath.Join(t.TempDir(), "intents.json")
	old := `[{"cordoned":false,"key":6},{"cordoned":false,"key":7}]`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIntents(path, topo); err == nil {
		t.Error("old key-swap format must be rejected")
	}
}

func TestLoadIntentsRejectsZeroWeightStakingSlot(t *testing.T) {
	topo := stdTopo()
	path := filepath.Join(t.TempDir(), "intents.json")
	intents := seedIntents(topo)
	intents[0].Weight = 0 // weight 0 means removal; never allowed
	if err := saveIntents(path, intents); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIntents(path, topo); err == nil {
		t.Error("zero-weight staking slot must be rejected")
	}
}
