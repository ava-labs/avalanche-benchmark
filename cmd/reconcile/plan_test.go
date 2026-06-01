package main

import (
	"reflect"
	"testing"
)

func TestComputeMapping(t *testing.T) {
	tests := []struct {
		name     string
		cordoned []bool
		prevKey  []int
		want     []int
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeMapping(tt.cordoned, tt.prevKey)
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
