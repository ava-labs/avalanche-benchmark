package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

// Standard 3 validators + 1 spare + 2 RPCs per site. Key scheme (see plan.go):
// registered validators 6..5+totalValidators (NVal per site, both sites live);
// then one home identity per slot, HomeKey(i) = homeBase+i. A single site seeds
// [6,7,8,12,13,14] (m4 spare home 12, m5/m6 rpc homes 13,14 — pinned); two
// sites seed A=[6,7,8,15,16,17] and B=[9,10,11,21,22,23] (homes 12..23).
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

// TestSeed2x2 pins the 2x2 default mapping: 2 registered validators in each
// data center plus 2 pinned RPC trackers per site, no spares. This is the
// topology the draft deploys.
func TestSeed2x2(t *testing.T) {
	topo := Topology{TwoSite: true, NVal: 2, NRPC: 2}
	want := []int{6, 7, 12, 13, 8, 9, 16, 17} // m1,m2=v1,v2; b1,b2=v3,v4; rest rpc homes
	got := seedIntentsKeys(topo)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("2x2 seed = %v, want %v", got, want)
	}
	if live := LiveValidators(topo, seedIntents(topo)); live != 4 {
		t.Errorf("2x2 seed live validators = %d, want 4", live)
	}
	// The DC split must be visible: m* slots are site A, b* slots site B.
	names := []string{"m1", "m2", "m3", "m4", "b1", "b2", "b3", "b4"}
	for i, n := range names {
		if topo.MachineName(i) != n {
			t.Errorf("MachineName(%d) = %s, want %s", i, topo.MachineName(i), n)
		}
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

// TestRetargetNeverCrossesSites pins the site-locality invariant: a
// single-machine fault with no same-site spare leaves the key uncovered rather
// than promoting a machine in the other data center.
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
	for i := range next {
		if next[i].Key == 6 {
			t.Errorf("site-A validator key 6 landed on %s; must stay uncovered", topo.MachineName(i))
		}
	}
	if live := LiveValidators(topo, next); live != 5 {
		t.Errorf("live = %d, want 5 of 6 (v1 uncovered by design)", live)
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
