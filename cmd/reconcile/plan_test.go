package main

import (
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

func twoSiteTopo() Topology {
	return Topology{TwoSite: true, NVal: 3, NSpare: 1, NRPC: 2}
}

func TestSeedIntents(t *testing.T) {
	topo := twoSiteTopo()
	intents := seedIntents(topo)
	if len(intents) != 12 {
		t.Fatalf("seed size = %d, want 12", len(intents))
	}
	for i, in := range intents {
		if in.Cordoned {
			t.Errorf("slot %d seeded cordoned", i)
		}
		want := valmgr.StandbyWeight
		switch {
		case topo.IsRPCSlot(i):
			want = 0
		case i < 3: // site A validators
			want = valmgr.ActiveWeight
		}
		if in.Weight != want {
			t.Errorf("slot %d weight = %d, want %d", i, in.Weight, want)
		}
	}
	if got := LiveValidators(topo, intents); got != 3 {
		t.Errorf("LiveValidators(seed) = %d, want 3", got)
	}
}

func TestPlanInstallsPermanentKeys(t *testing.T) {
	topo := twoSiteTopo()
	intents := seedIntents(topo)
	obs := make([]Observed, len(intents)) // all dead, no keys installed
	acts := Plan(topo, intents, obs)
	for i, a := range acts {
		if a.SwapKey != topo.KeyOf(i) {
			t.Errorf("slot %d swap = %d, want permanent key %d", i, a.SwapKey, topo.KeyOf(i))
		}
		if !a.Start || a.Stop {
			t.Errorf("slot %d: fresh dead machine should start without stop, got %+v", i, a)
		}
	}
}

func TestPlanStableStateIsNoop(t *testing.T) {
	topo := twoSiteTopo()
	intents := seedIntents(topo)
	obs := make([]Observed, len(intents))
	for i := range obs {
		obs[i] = Observed{Alive: true, ActualKey: topo.KeyOf(i)}
	}
	for _, a := range Plan(topo, intents, obs) {
		if a.Stop || a.Start || a.SwapKey != 0 {
			t.Errorf("stable state produced action %+v", a)
		}
	}
}

func TestPlanCordonStopsOnlyThatMachine(t *testing.T) {
	topo := twoSiteTopo()
	intents := seedIntents(topo)
	intents[1].Cordoned = true
	obs := make([]Observed, len(intents))
	for i := range obs {
		obs[i] = Observed{Alive: true, ActualKey: topo.KeyOf(i)}
	}
	acts := Plan(topo, intents, obs)
	for i, a := range acts {
		wantStop := i == 1
		if a.Stop != wantStop || a.Start || a.SwapKey != 0 {
			t.Errorf("slot %d action = %+v (wantStop=%v)", i, a, wantStop)
		}
	}
}

func TestPlanHealsWrongKey(t *testing.T) {
	topo := twoSiteTopo()
	intents := seedIntents(topo)
	obs := make([]Observed, len(intents))
	for i := range obs {
		obs[i] = Observed{Alive: true, ActualKey: topo.KeyOf(i)}
	}
	obs[2].ActualKey = 99 // hand-mangled active dir
	acts := Plan(topo, intents, obs)
	a := acts[2]
	if !a.Stop || a.SwapKey != topo.KeyOf(2) || !a.Start {
		t.Errorf("wrong-key slot action = %+v, want stop+install+start", a)
	}
}

func TestIsActiveWeight(t *testing.T) {
	total := uint64(3*valmgr.ActiveWeight + 5*valmgr.StandbyWeight)
	if !isActiveWeight(valmgr.ActiveWeight, total) {
		t.Error("ActiveWeight should be active")
	}
	if isActiveWeight(valmgr.StandbyWeight, total) {
		t.Error("StandbyWeight should not be active")
	}
	if isActiveWeight(0, total) {
		t.Error("0 should not be active")
	}
}
