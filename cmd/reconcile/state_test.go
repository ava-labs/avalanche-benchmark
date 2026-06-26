package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

// Standard 3 validators + 1 spare + 2 RPCs per site. Key scheme (see plan.go):
// validators 6,7,8; then one home identity per slot, HomeKey(i) = 9+i. So a single
// site seeds [6,7,8,12,13,14] (m4 spare home 12, m5/m6 rpc homes 13,14 — pinned),
// and two sites add b1..b6 homes 15..20 (b5/b6 rpc homes 19,20 pinned).
func stdTopo() Topology    { return Topology{NVal: 3, NSpare: 1, NRPC: 2} }
func stdTwoSite() Topology { return Topology{TwoSite: true, NVal: 3, NSpare: 1, NRPC: 2} }

// TestRetargetSequence walks the worked example from the design doc through the
// real code path (retarget + JSON persistence), confirming the spare rotates and
// quorum drops/recovers exactly as documented.
func TestRetargetSequence(t *testing.T) {
	topo := stdTopo()
	path := filepath.Join(t.TempDir(), "intentions.json")

	// Seed (what `fresh` writes).
	if err := saveIntents(path, seedIntents(topo)); err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		op       string
		machine  int
		cordon   bool
		wantKeys []int
		wantLive int
	}{
		// m5 (key 13) and m6 (key 14) are pinned RPCs — never promoted, they stay every step.
		{"down", 2, true, []int{6, 10, 8, 7, 13, 14}, 3},  // spare m4 takes v2; m2 parks on home 10
		{"down", 3, true, []int{6, 10, 11, 7, 13, 14}, 2}, // no spare, v3 uncovered
		{"down", 1, true, []int{9, 10, 11, 7, 13, 14}, 1}, // v1 uncovered -> halt
		{"up", 3, false, []int{9, 10, 6, 7, 13, 14}, 2},   // m3 covers lowest orphan (v1), quorum back
	}

	for _, s := range steps {
		prev, err := loadIntents(path, topo)
		if err != nil {
			t.Fatalf("%s %d: load: %v", s.op, s.machine, err)
		}
		next, err := retarget(prev, s.machine, s.cordon, topo)
		if err != nil {
			t.Fatalf("%s %d: retarget: %v", s.op, s.machine, err)
		}
		if err := saveIntents(path, next); err != nil {
			t.Fatalf("%s %d: save: %v", s.op, s.machine, err)
		}

		gotKeys := make([]int, len(next))
		for i, in := range next {
			gotKeys[i] = in.Key
		}
		if !reflect.DeepEqual(gotKeys, s.wantKeys) {
			t.Errorf("after %s %d: keys = %v, want %v", s.op, s.machine, gotKeys, s.wantKeys)
		}
		if live := LiveValidators(topo, next); live != s.wantLive {
			t.Errorf("after %s %d: live = %d, want %d", s.op, s.machine, live, s.wantLive)
		}
	}
}

// TestSiteFailoverSequence walks a full two-site cycle: fail over to the backup
// site, take a single-machine fault there, then fail back to the primary.
func TestSiteFailoverSequence(t *testing.T) {
	topo := stdTwoSite()
	sp := topo.sitePool()
	path := filepath.Join(t.TempDir(), "intentions.json")

	if err := saveIntents(path, seedIntents(topo)); err != nil {
		t.Fatal(err)
	}

	keys := func(intents []MachineIntent) []int {
		ks := make([]int, len(intents))
		for i, in := range intents {
			ks[i] = in.Key
		}
		return ks
	}

	// Step 1 — full site-A outage: fail over to B. All of A cordons onto its home
	// identities, v1-v3 land on b1-b3, b4 stays the new spare, b5/b6 stay pinned rpc.
	prev, err := loadIntents(path, topo)
	if err != nil {
		t.Fatal(err)
	}
	next, err := retargetSite(prev, topo, siteB)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{9, 10, 11, 12, 13, 14, 6, 7, 8, 18, 19, 20}
	if got := keys(next); !reflect.DeepEqual(got, want) {
		t.Errorf("after site-failover b: keys = %v, want %v", got, want)
	}
	for i := 0; i < sp; i++ {
		if !next[i].Cordoned {
			t.Errorf("after site-failover b: %s not cordoned", topo.MachineName(i))
		}
	}
	if live := LiveValidators(topo, next); live != 3 {
		t.Errorf("after site-failover b: live = %d, want 3", live)
	}
	if err := saveIntents(path, next); err != nil {
		t.Fatal(err)
	}

	// Step 2 — single-machine fault in the now-active site B: b1 dies, the B spare
	// (b4) takes its key. Site A stays untouched. (b1 = machine sp+1.)
	prev, err = loadIntents(path, topo)
	if err != nil {
		t.Fatal(err)
	}
	next, err = retarget(prev, sp+1, true, topo) // machine sp+1 = b1
	if err != nil {
		t.Fatal(err)
	}
	want = []int{9, 10, 11, 12, 13, 14, 15, 7, 8, 6, 19, 20}
	if got := keys(next); !reflect.DeepEqual(got, want) {
		t.Errorf("after down b1: keys = %v, want %v", got, want)
	}
	if live := LiveValidators(topo, next); live != 3 {
		t.Errorf("after down b1: live = %d, want 3", live)
	}
	if err := saveIntents(path, next); err != nil {
		t.Fatal(err)
	}

	// Step 3 — fail back to A: B cordons, A uncordons, v1-v3 land on m1-m3,
	// m4 returns to spare. Steady state restored exactly.
	prev, err = loadIntents(path, topo)
	if err != nil {
		t.Fatal(err)
	}
	next, err = retargetSite(prev, topo, siteA)
	if err != nil {
		t.Fatal(err)
	}
	want = seedIntentsKeys(topo)
	if got := keys(next); !reflect.DeepEqual(got, want) {
		t.Errorf("after site-failover a: keys = %v, want %v", got, want)
	}
	if live := LiveValidators(topo, next); live != 3 {
		t.Errorf("after site-failover a: live = %d, want 3", live)
	}
}

// seedIntentsKeys is the key vector of the default seed — a test convenience.
func seedIntentsKeys(topo Topology) []int {
	seed := seedIntents(topo)
	ks := make([]int, len(seed))
	for i, in := range seed {
		ks[i] = in.Key
	}
	return ks
}

// TestRetargetNeverCrossesSites pins the single-site-consensus invariant: a
// single-machine fault with no same-site spare leaves the key uncovered rather
// than promoting a backup-site tracker.
func TestRetargetNeverCrossesSites(t *testing.T) {
	topo := stdTwoSite()
	intents := seedIntents(topo)

	// Kill m1 (v1 -> spare m4), then m4: v1 must go uncovered, not to site B.
	next, err := retarget(intents, 1, true, topo)
	if err != nil {
		t.Fatal(err)
	}
	if next[3].Key != 6 {
		t.Fatalf("after down m1: m4 key = %d, want 6", next[3].Key)
	}
	next, err = retarget(next, 4, true, topo)
	if err != nil {
		t.Fatal(err)
	}
	for i := topo.sitePool(); i < topo.Size(); i++ {
		if topo.isValidatorKey(next[i].Key) {
			t.Errorf("validator key %d crossed to %s without site-failover", next[i].Key, topo.MachineName(i))
		}
	}
	if live := LiveValidators(topo, next); live != 2 {
		t.Errorf("live = %d, want 2 (v1 uncovered by design)", live)
	}
}

func TestSiteFailoverRequiresTwoSite(t *testing.T) {
	topo := stdTopo()
	if _, err := retargetSite(seedIntents(topo), topo, siteB); err == nil {
		t.Error("retargetSite in single-site mode should error")
	}
	if _, ok := topo.SiteFromName("b"); ok {
		t.Error("SiteFromName(b) should fail in single-site mode")
	}
}

// TestRestorePlan checks the rolling-restore snapshot sequence: bring the
// target site up as trackers, then move each validator key over one at a time,
// never with a key on two live machines, ending exactly at the steady seed.
func TestRestorePlan(t *testing.T) {
	topo := stdTwoSite()
	sp := topo.sitePool()
	// Failed-over state (after `site-failover b`): site A cordoned on home keys,
	// site B active with v1-v3 on b1-b3.
	prev := []MachineIntent{
		{true, 9}, {true, 10}, {true, 11}, {true, 12}, {true, 13}, {true, 14},
		{false, 6}, {false, 7}, {false, 8}, {false, 18}, {false, 19}, {false, 20},
	}
	trackerStep, keySteps := RestorePlan(topo, prev, siteA)

	wantTracker := []MachineIntent{
		{false, 9}, {false, 10}, {false, 11}, {false, 12}, {false, 13}, {false, 14},
		{false, 6}, {false, 7}, {false, 8}, {false, 18}, {false, 19}, {false, 20},
	}
	if !reflect.DeepEqual(trackerStep, wantTracker) {
		t.Fatalf("trackerStep = %v, want %v", trackerStep, wantTracker)
	}
	if len(keySteps) != 3 {
		t.Fatalf("got %d key steps, want 3", len(keySteps))
	}

	// Every snapshot must keep all three validator keys assigned to exactly one
	// live machine each (no key on two live machines, none dropped).
	for n, step := range keySteps {
		seen := map[int]int{}
		for _, in := range step {
			if topo.isValidatorKey(in.Key) && !in.Cordoned {
				seen[in.Key]++
			}
		}
		for k, c := range seen {
			if c != 1 {
				t.Errorf("step %d: validator key %d on %d live machines, want 1", n, k, c)
			}
		}
		if len(seen) != 3 {
			t.Errorf("step %d: %d validator keys live, want 3", n, len(seen))
		}
	}

	// Step 0 moves only v1 (key 6) to m1; v2/v3 stay on site B. b1 (index sp)
	// releases v1 back to its home identity (15).
	if keySteps[0][0].Key != 6 || keySteps[0][sp].Key != topo.HomeKey(sp) {
		t.Errorf("step0: want m1=6, b1=%d; got m1=%d b1=%d", topo.HomeKey(sp), keySteps[0][0].Key, keySteps[0][sp].Key)
	}
	if keySteps[0][sp+1].Key != 7 || keySteps[0][sp+2].Key != 8 {
		t.Errorf("step0: v2/v3 should still be on site B; got b2=%d b3=%d", keySteps[0][sp+1].Key, keySteps[0][sp+2].Key)
	}

	// Final state equals the steady seed with site A active.
	if !reflect.DeepEqual(keySteps[2], seedIntents(topo)) {
		t.Errorf("final step = %v, want seed %v", keySteps[2], seedIntents(topo))
	}
}

func TestLoadIntentsMissingReturnsSeed(t *testing.T) {
	topo := stdTopo()
	got, err := loadIntents(filepath.Join(t.TempDir(), "does-not-exist.json"), topo)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, seedIntents(topo)) {
		t.Errorf("missing file = %v, want seed %v", got, seedIntents(topo))
	}
}

// TestLoadIntentsMigratesSingleSiteFile confirms a single-site intentions file
// (one site's machines, written before the backup site existed) loads in two-site
// mode with the site-B seed appended and site A preserved as-is (mid-failover included).
func TestLoadIntentsMigratesSingleSiteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intentions.json")
	old := []MachineIntent{
		{Cordoned: false, Key: 6},
		{Cordoned: true, Key: 10}, // mid-failover: m2 down, v2 on the spare
		{Cordoned: false, Key: 8},
		{Cordoned: false, Key: 7},
		{Cordoned: false, Key: 13},
		{Cordoned: false, Key: 14}, // m6 second archive RPC
	}
	if err := saveIntents(path, old); err != nil {
		t.Fatal(err)
	}
	topo := stdTwoSite()
	got, err := loadIntents(path, topo)
	if err != nil {
		t.Fatal(err)
	}
	want := append(old, seedIntents(topo)[topo.sitePool():]...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("migrated = %v, want %v", got, want)
	}
}
