package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

func stdTopo() Topology    { return Topology{NVal: 3, NSpare: 1, NRPC: 2} }
func stdTwoSite() Topology { return Topology{TwoSite: true, NVal: 3, NSpare: 1, NRPC: 2} }

// TestRetargetPromotesSpare: cordoning an active validator hands its weight to
// the same-site spare; uncordoning brings it back as a standby (sticky).
func TestRetargetPromotesSpare(t *testing.T) {
	topo := stdTopo()
	intents := seedIntents(topo)

	next, err := retarget(intents, 1, true, topo) // down m1
	if err != nil {
		t.Fatal(err)
	}
	if !next[0].Cordoned || next[0].Weight != valmgr.StandbyWeight {
		t.Errorf("m1 after down = %+v, want cordoned standby", next[0])
	}
	if next[3].Weight != valmgr.ActiveWeight {
		t.Errorf("spare m4 weight = %d, want promoted to %d", next[3].Weight, valmgr.ActiveWeight)
	}
	if got := LiveValidators(topo, next); got != 3 {
		t.Errorf("LiveValidators after down = %d, want 3 (spare covered)", got)
	}

	back, err := retarget(next, 1, false, topo) // up m1
	if err != nil {
		t.Fatal(err)
	}
	if back[0].Cordoned || back[0].Weight != valmgr.StandbyWeight {
		t.Errorf("m1 after up = %+v, want uncordoned standby (sticky promotion)", back[0])
	}
	if back[3].Weight != valmgr.ActiveWeight {
		t.Errorf("spare m4 must keep the promoted weight, got %d", back[3].Weight)
	}
}

// TestRetargetNoFreeStandby: with the spare already promoted and another
// validator cordoned, the weight drops to standby (quorum degrades, no crash).
func TestRetargetNoFreeStandby(t *testing.T) {
	topo := stdTopo()
	intents := seedIntents(topo)
	next, _ := retarget(intents, 1, true, topo) // spare promoted
	next, _ = retarget(next, 2, true, topo)     // no free standby left in site A
	if next[1].Weight != valmgr.StandbyWeight || !next[1].Cordoned {
		t.Errorf("m2 = %+v, want cordoned standby", next[1])
	}
	if got := LiveValidators(topo, next); got != 2 {
		t.Errorf("LiveValidators = %d, want 2 (degraded)", got)
	}
}

// TestRetargetNeverCrossesSites: a site-A fault never promotes a site-B slot.
func TestRetargetNeverCrossesSites(t *testing.T) {
	topo := stdTwoSite()
	intents := seedIntents(topo)
	next, _ := retarget(intents, 1, true, topo)
	next, _ = retarget(next, 2, true, topo)
	for i := topo.SitePool(); i < topo.Size(); i++ {
		if next[i].Weight != seedWeight(topo, i) {
			t.Errorf("site-B slot %d weight changed to %d on a site-A fault", i, next[i].Weight)
		}
	}
}

func TestSiteFailoverWeights(t *testing.T) {
	topo := stdTwoSite()
	intents := seedIntents(topo)
	next, err := retargetSite(intents, topo, siteB)
	if err != nil {
		t.Fatal(err)
	}
	for i, in := range next {
		wantCordon := topo.Site(i) == siteA
		if in.Cordoned != wantCordon {
			t.Errorf("slot %d cordoned = %v, want %v", i, in.Cordoned, wantCordon)
		}
		want := uint64(valmgr.StandbyWeight)
		switch {
		case !topo.IsStakingSlot(i):
			want = 0
		case topo.Site(i) == siteB && topo.IsValidatorSlot(i):
			want = valmgr.ActiveWeight
		}
		if in.Weight != want {
			t.Errorf("slot %d weight = %d, want %d", i, in.Weight, want)
		}
	}
}

func TestRestoreKeepsBothSitesUp(t *testing.T) {
	topo := stdTwoSite()
	intents, err := retargetSite(seedIntents(topo), topo, siteB)
	if err != nil {
		t.Fatal(err)
	}
	next, err := retargetRestore(intents, topo, siteA)
	if err != nil {
		t.Fatal(err)
	}
	for i, in := range next {
		if in.Cordoned {
			t.Errorf("slot %d cordoned after restore, want everything up", i)
		}
		if topo.Site(i) == siteA && topo.IsValidatorSlot(i) && in.Weight != valmgr.ActiveWeight {
			t.Errorf("site-A validator %d weight = %d, want active", i, in.Weight)
		}
		if topo.Site(i) == siteB && in.Weight > valmgr.StandbyWeight {
			t.Errorf("site-B slot %d weight = %d, want standby", i, in.Weight)
		}
	}
}

func TestSiteFailoverRequiresTwoSite(t *testing.T) {
	topo := stdTopo()
	if _, err := retargetSite(seedIntents(topo), topo, siteB); err == nil {
		t.Error("retargetSite must fail in single-site mode")
	}
	if _, err := retargetRestore(seedIntents(topo), topo, siteB); err == nil {
		t.Error("retargetRestore must fail in single-site mode")
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
	want, _ := retargetSite(seedIntents(topo), topo, siteB)
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
