package main

import (
	"testing"
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

// TestPlanSkipsUnreachable: an ssh-unreachable host gets NO actions (nothing
// can execute there), while every other machine is still planned normally, so
// one dead box never blocks the fleet's stop/swap/start passes.
func TestPlanSkipsUnreachable(t *testing.T) {
	topo := twoSiteTopo()
	intents := seedIntents(topo)
	obs := make([]Observed, len(intents)) // all dead: everyone would be started
	obs[0].Unreachable = true
	acts := Plan(topo, intents, obs)
	if a := acts[0]; a.Stop || a.Start || a.SwapKey != 0 {
		t.Errorf("unreachable slot got action %+v, want none", a)
	}
	for i := 1; i < len(acts); i++ {
		if !acts[i].Start || acts[i].SwapKey != topo.KeyOf(i) {
			t.Errorf("slot %d: dead host elsewhere changed its plan: %+v", i, acts[i])
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
	total := uint64(3*validatorWeight + 5*spareWeight)
	if !isActiveWeight(validatorWeight, total) {
		t.Error("validatorWeight should be active")
	}
	if isActiveWeight(spareWeight, total) {
		t.Error("spareWeight should not be active")
	}
	if isActiveWeight(deadWeight, total) {
		t.Error("deadWeight should not be active")
	}
	if isActiveWeight(0, total) {
		t.Error("0 should not be active")
	}
}

func TestWeightRole(t *testing.T) {
	cases := map[uint64]string{
		0:               "rpc",
		validatorWeight: "validator",
		spareWeight:     "spare",
		deadWeight:      "dead",
	}
	for w, want := range cases {
		if got := weightRole(w); got != want {
			t.Errorf("weightRole(%d) = %q, want %q", w, got, want)
		}
	}
}

func TestStakeCell(t *testing.T) {
	cases := []struct {
		name       string
		actual     uint64
		haveActual bool
		staking    bool
		want       string
	}{
		{"rpc slot", 0, false, false, "rpc"},
		{"validator tier", validatorWeight, true, true, "validator"},
		{"spare tier", spareWeight, true, true, "spare"},
		{"dead tier", deadWeight, true, true, "dead"},
		{"mid-seesaw raw weight", 40_600, true, true, "w=40600"},
		{"pchain unreadable", 0, false, true, "?"},
	}
	for _, c := range cases {
		if got := stakeCell(c.actual, c.haveActual, c.staking); got != c.want {
			t.Errorf("%s: stakeCell(%d, %v, %v) = %q, want %q", c.name, c.actual, c.haveActual, c.staking, got, c.want)
		}
	}
}
