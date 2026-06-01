package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

// TestRetargetSequence walks the worked example from the design doc through the
// real code path (retarget + JSON persistence), confirming the spare rotates and
// quorum drops/recovers exactly as documented.
func TestRetargetSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intentions.json")

	// Seed (what `fresh` writes).
	if err := saveIntents(path, seedIntents()); err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		op       string
		machine  int
		cordon   bool
		wantKeys []int
		wantLive int
	}{
		{"down", 2, true, []int{6, 9, 8, 7}, 3}, // spare m4 takes v2
		{"down", 3, true, []int{6, 9, 9, 7}, 2}, // no spare, v3 uncovered
		{"down", 1, true, []int{9, 9, 9, 7}, 1}, // v1 uncovered -> halt
		{"up", 3, false, []int{9, 9, 6, 7}, 2},  // m3 covers lowest orphan (v1), quorum back
	}

	for _, s := range steps {
		prev, err := loadIntents(path)
		if err != nil {
			t.Fatalf("%s %d: load: %v", s.op, s.machine, err)
		}
		next, err := retarget(prev, s.machine, s.cordon)
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
		if live := LiveValidators(next); live != s.wantLive {
			t.Errorf("after %s %d: live = %d, want %d", s.op, s.machine, live, s.wantLive)
		}
	}
}

func TestLoadIntentsMissingReturnsSeed(t *testing.T) {
	got, err := loadIntents(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, seedIntents()) {
		t.Errorf("missing file = %v, want seed %v", got, seedIntents())
	}
}
