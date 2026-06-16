package main

import (
	"reflect"
	"testing"
)

func TestComputeMapping(t *testing.T) {
	tests := []struct {
		name      string
		topo      Topology
		preferred int
		cordoned  []bool
		prevKey   []int
		want      []int
	}{
		{
			name:     "steady state, no cordon, no change",
			cordoned: []bool{false, false, false, false},
			prevKey:  []int{6, 7, 8, 9},
			want:     []int{6, 7, 8, 9},
		},
		{
			name:     "cordon m2: spare m4 takes orphaned v2",
			cordoned: []bool{false, true, false, false},
			prevKey:  []int{6, 7, 8, 9},
			want:     []int{6, 9, 8, 7},
		},
		{
			name:     "second cordon m3: no spare left, v3 uncovered",
			cordoned: []bool{false, true, true, false},
			prevKey:  []int{6, 9, 8, 7}, // continues previous case
			want:     []int{6, 9, 9, 7},
		},
		{
			name:     "third cordon m1: v1 uncovered, only v2 live",
			cordoned: []bool{true, true, true, false},
			prevKey:  []int{6, 9, 9, 7},
			want:     []int{9, 9, 9, 7},
		},
		{
			name:     "uncordon m3: free machine takes lowest orphaned validator key",
			cordoned: []bool{true, true, false, false},
			prevKey:  []int{9, 9, 9, 7},
			want:     []int{9, 9, 6, 7}, // 6 and 8 orphaned; m3 takes lowest (6)
		},
		{
			name:     "uncordoned validator key is sticky, not reshuffled",
			cordoned: []bool{false, false, false, false},
			prevKey:  []int{8, 6, 7, 9}, // rotated from a prior failover
			want:     []int{8, 6, 7, 9},
		},
		{
			name:     "two spares, two orphans assigned lowest-machine-first",
			cordoned: []bool{true, true, false, false},
			prevKey:  []int{6, 7, 9, 9},
			want:     []int{9, 9, 6, 7}, // m3,m4 free; orphans 6,7 -> m3=6, m4=7
		},
		{
			name:     "pinned rpc m5 stays put in steady state",
			cordoned: []bool{false, false, false, false, false},
			prevKey:  []int{6, 7, 8, 9, 10},
			want:     []int{6, 7, 8, 9, 10},
		},
		{
			name:     "cordon m2: spare m4 promotes, pinned rpc m5 untouched",
			cordoned: []bool{false, true, false, false, false},
			prevKey:  []int{6, 7, 8, 9, 10},
			want:     []int{6, 9, 8, 7, 10},
		},
		{
			name:     "rpc m5 NEVER absorbs an orphan even when v3 goes uncovered",
			cordoned: []bool{false, true, true, false, false},
			prevKey:  []int{6, 7, 8, 9, 10},
			want:     []int{6, 9, 9, 7, 10}, // only m4 free; v3(8) stays uncovered; m5 keeps 10
		},
		{
			name:     "pin is sticky across cordon/uncordon of m5 itself",
			cordoned: []bool{false, false, false, false, true},
			prevKey:  []int{6, 7, 8, 9, 10},
			want:     []int{6, 7, 8, 9, 10}, // cordoned rpc still keeps key 10 (just goes down)
		},
		{
			name:      "two-site steady state: backup trackers untouched",
			topo:      Topology{TwoSite: true},
			preferred: siteA,
			cordoned:  []bool{false, false, false, false, false, false, false, false, false, false},
			prevKey:   []int{6, 7, 8, 9, 10, 14, 15, 16, 17, 18},
			want:      []int{6, 7, 8, 9, 10, 14, 15, 16, 17, 18},
		},
		{
			name:      "two-site cordon m2: same-site spare m4 promotes, B never touched",
			topo:      Topology{TwoSite: true},
			preferred: siteA,
			cordoned:  []bool{false, true, false, false, false, false, false, false, false, false},
			prevKey:   []int{6, 7, 8, 9, 10, 14, 15, 16, 17, 18},
			want:      []int{6, 12, 8, 7, 10, 14, 15, 16, 17, 18}, // m2 parks on home 12, m4 takes v2
		},
		{
			name:      "two-site: orphan with no same-site spare stays uncovered, never crosses to B",
			topo:      Topology{TwoSite: true},
			preferred: siteA,
			cordoned:  []bool{false, true, true, false, false, false, false, false, false, false},
			prevKey:   []int{6, 12, 8, 7, 10, 14, 15, 16, 17, 18},
			want:      []int{6, 12, 13, 7, 10, 14, 15, 16, 17, 18}, // v3(8) uncovered; b1-b4 stay sync
		},
		{
			name:      "site-failover to B: all of A cordoned, v1-v3 land on b1-b3",
			topo:      Topology{TwoSite: true},
			preferred: siteB,
			cordoned:  []bool{true, true, true, true, true, false, false, false, false, false},
			prevKey:   []int{6, 7, 8, 9, 10, 14, 15, 16, 17, 18},
			want:      []int{11, 12, 13, 9, 10, 6, 7, 8, 17, 18}, // b4 is the new spare, b5 rpc pinned
		},
		{
			name:      "post-failover fault on b2: B spare b4 promotes",
			topo:      Topology{TwoSite: true},
			preferred: siteB,
			cordoned:  []bool{true, true, true, true, true, false, true, false, false, false},
			prevKey:   []int{11, 12, 13, 9, 10, 6, 7, 8, 17, 18},
			want:      []int{11, 12, 13, 9, 10, 6, 15, 8, 7, 18}, // b2 parks on home 15, b4 takes v2
		},
		{
			name:      "failback to A: B cordons to homes, m1-m3 retake v1-v3",
			topo:      Topology{TwoSite: true},
			preferred: siteA,
			cordoned:  []bool{false, false, false, false, false, true, true, true, true, true},
			prevKey:   []int{11, 12, 13, 9, 10, 6, 7, 8, 17, 18},
			want:      []int{6, 7, 8, 9, 10, 14, 15, 16, 17, 18}, // exact seed restored
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preferred := tt.preferred // zero value is siteA, correct for the legacy cases
			got := ComputeMapping(tt.topo, tt.cordoned, tt.prevKey, preferred)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ComputeMapping() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlan(t *testing.T) {
	tests := []struct {
		name    string
		intents []MachineIntent
		obs     []Observed
		want    []Action
	}{
		{
			name: "fresh: nothing on disk, swap seed key then start all",
			intents: []MachineIntent{
				{false, 6}, {false, 7}, {false, 8}, {false, 9},
			},
			obs: []Observed{
				{Alive: false, ActualKey: 0}, {Alive: false, ActualKey: 0},
				{Alive: false, ActualKey: 0}, {Alive: false, ActualKey: 0},
			},
			want: []Action{
				{Machine: 1, Stop: false, SwapKey: 6, Start: true},
				{Machine: 2, Stop: false, SwapKey: 7, Start: true},
				{Machine: 3, Stop: false, SwapKey: 8, Start: true},
				{Machine: 4, Stop: false, SwapKey: 9, Start: true},
			},
		},
		{
			name: "steady state already correct: no-op",
			intents: []MachineIntent{
				{false, 6}, {false, 7}, {false, 8}, {false, 9},
			},
			obs: []Observed{
				{true, 6}, {true, 7}, {true, 8}, {true, 9},
			},
			want: []Action{
				{Machine: 1}, {Machine: 2}, {Machine: 3}, {Machine: 4},
			},
		},
		{
			name: "cordon m2, spare m4 takes v2: stop+rekey m2, stop-swap-start m4",
			intents: []MachineIntent{
				{false, 6}, {true, 9}, {false, 8}, {false, 7},
			},
			obs: []Observed{
				{true, 6}, {true, 7}, {true, 8}, {true, 9},
			},
			want: []Action{
				{Machine: 1},
				{Machine: 2, Stop: true, SwapKey: 9, Start: false}, // cordoned -> rekey nv, stays down
				{Machine: 3},
				{Machine: 4, Stop: true, SwapKey: 7, Start: true}, // spare becomes v2
			},
		},
		{
			name: "uncordoned validator crashed: restart same key, no swap",
			intents: []MachineIntent{
				{false, 6}, {false, 7}, {false, 8}, {false, 9},
			},
			obs: []Observed{
				{true, 6}, {false, 7}, {true, 8}, {true, 9},
			},
			want: []Action{
				{Machine: 1},
				{Machine: 2, Stop: false, SwapKey: 0, Start: true}, // dead, key already right
				{Machine: 3},
				{Machine: 4},
			},
		},
		{
			name: "cordoned machine still running stale validator key: stop and rekey to nv",
			intents: []MachineIntent{
				{false, 6}, {true, 9}, {false, 8}, {false, 7},
			},
			obs: []Observed{
				{true, 7}, {true, 7}, {true, 8}, {true, 9}, // m1 wrongly on 7, m2 stale 7, m4 not yet swapped
			},
			want: []Action{
				{Machine: 1, Stop: true, SwapKey: 6, Start: true},  // wrong key -> stop-swap-start
				{Machine: 2, Stop: true, SwapKey: 9, Start: false}, // cordoned -> rekey nv, stays down
				{Machine: 3},
				{Machine: 4, Stop: true, SwapKey: 7, Start: true}, // spare becomes v2
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Plan(tt.intents, tt.obs)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Plan() =\n  %v\nwant\n  %v", got, tt.want)
			}
		})
	}
}

func TestLiveValidators(t *testing.T) {
	tests := []struct {
		name    string
		intents []MachineIntent
		want    int
	}{
		{"all three live", []MachineIntent{{false, 6}, {false, 7}, {false, 8}, {false, 9}}, 3},
		{"one cordoned but key moved to spare", []MachineIntent{{false, 6}, {true, 9}, {false, 8}, {false, 7}}, 3},
		{"two live, one uncovered", []MachineIntent{{false, 6}, {true, 9}, {true, 9}, {false, 7}}, 2},
		{"one live, halt", []MachineIntent{{true, 9}, {true, 9}, {true, 9}, {false, 7}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LiveValidators(tt.intents); got != tt.want {
				t.Errorf("LiveValidators() = %d, want %d", got, tt.want)
			}
		})
	}
}
