package main

import (
	"os"
	"path/filepath"
	"testing"
)

func stdTopo() Topology    { return Topology{NVal: 3, NSpare: 1, NRPC: 2} }
func stdTwoSite() Topology { return Topology{TwoSite: true, NVal: 3, NSpare: 1, NRPC: 2} }

// TestSetCordon: cordon is the pure hardware axis - it flips the flag and
// leaves the input slice untouched.
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

func TestSetOutOfRange(t *testing.T) {
	topo := stdTopo()
	intents := seedIntents(topo)
	if _, err := setCordon(intents, 0, true); err == nil {
		t.Error("machine 0 must be rejected")
	}
	if _, err := setCordon(intents, topo.Size()+1, true); err == nil {
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
}

// A state file from the retired weight-carrying format loads fine: the weight
// field is ignored (weights are on-chain state, owned by cmd/l1).
func TestLoadIntentsIgnoresRetiredWeightField(t *testing.T) {
	topo := stdTopo()
	path := filepath.Join(t.TempDir(), "intents.json")
	old := `[{"cordoned":true,"weight":100000},{"cordoned":false,"weight":100000},{"cordoned":false,"weight":100000},{"cordoned":false,"weight":1},{"cordoned":false,"weight":0},{"cordoned":false,"weight":0}]`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadIntents(path, topo)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Cordoned || got[1].Cordoned {
		t.Errorf("cordon flags lost: %+v", got)
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
