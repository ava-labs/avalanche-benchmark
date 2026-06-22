package main

import (
	"reflect"
	"testing"
)

// TestPickCloneSource pins the rule that keeps a hard failover from deadlocking: pick the
// most-advanced serving validator and re-seed any that are too far behind (or not serving).
func TestPickCloneSource(t *testing.T) {
	cases := []struct {
		name        string
		blocks      []uint64
		serving     []bool
		tol         uint64
		wantSrc     int
		wantLag     []int
		wantOK      bool
	}{
		{"all consistent", []uint64{1000, 990, 995}, []bool{true, true, true}, 30, 0, nil, true},
		{"two far behind (the b1/b2/b3 deadlock)", []uint64{659000, 426000, 412000}, []bool{true, true, true}, 30, 0, []int{1, 2}, true},
		{"highest not first", []uint64{400000, 659000, 412000}, []bool{true, true, true}, 30, 1, []int{0, 2}, true},
		{"not-serving counts as laggard", []uint64{1000, 0, 995}, []bool{true, false, true}, 30, 0, []int{1}, true},
		{"none serving -> no source", []uint64{500, 400}, []bool{false, false}, 30, -1, nil, false},
		{"boundary: ==tol stays, >tol clones", []uint64{1000, 970, 969}, []bool{true, true, true}, 30, 0, []int{2}, true},
		{"source ignores a higher but non-serving node", []uint64{1000, 2000, 990}, []bool{true, false, true}, 30, 0, []int{1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, lag, ok := pickCloneSource(tc.blocks, tc.serving, tc.tol)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if src != tc.wantSrc {
				t.Fatalf("srcIdx = %d, want %d", src, tc.wantSrc)
			}
			if !reflect.DeepEqual(lag, tc.wantLag) {
				t.Fatalf("laggards = %v, want %v", lag, tc.wantLag)
			}
		})
	}
}
