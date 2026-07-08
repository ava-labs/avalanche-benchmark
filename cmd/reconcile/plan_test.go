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
	total := totalWeight(intents)
	active := 0
	for i, in := range intents {
		if in.Cordoned {
			t.Errorf("slot %d seeded cordoned", i)
		}
		want := valmgr.SpareWeight
		switch {
		case topo.IsRPCSlot(i):
			want = 0
		case i < 4: // all site A staking slots (validators + spare)
			want = valmgr.ValidatorWeight
		}
		if in.Weight != want {
			t.Errorf("slot %d weight = %d, want %d", i, in.Weight, want)
		}
		if isActiveWeight(in.Weight, total) {
			active++
		}
	}
	if active != 4 {
		t.Errorf("active validators in seed = %d, want 4", active)
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
	total := uint64(3*valmgr.ValidatorWeight + 5*valmgr.SpareWeight)
	if !isActiveWeight(valmgr.ValidatorWeight, total) {
		t.Error("ValidatorWeight should be active")
	}
	if isActiveWeight(valmgr.SpareWeight, total) {
		t.Error("SpareWeight should not be active")
	}
	if isActiveWeight(valmgr.DeadWeight, total) {
		t.Error("DeadWeight should not be active")
	}
	if isActiveWeight(0, total) {
		t.Error("0 should not be active")
	}
}

func TestWeightRole(t *testing.T) {
	cases := map[uint64]string{
		0:                      "rpc",
		valmgr.ValidatorWeight: "validator",
		valmgr.SpareWeight:     "spare",
		valmgr.DeadWeight:      "dead",
	}
	for w, want := range cases {
		if got := weightRole(w); got != want {
			t.Errorf("weightRole(%d) = %q, want %q", w, got, want)
		}
	}
}
