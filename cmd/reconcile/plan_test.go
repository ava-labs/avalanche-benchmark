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

func TestStakeCell(t *testing.T) {
	v, s, d := valmgr.ValidatorWeight, valmgr.SpareWeight, valmgr.DeadWeight
	cases := []struct {
		name            string
		desired, actual uint64
		haveActual      bool
		want            string
	}{
		{"rpc slot", 0, 0, false, "rpc"},
		{"converged validator", v, v, true, "validator"},
		{"converged spare", s, s, true, "spare"},
		{"demotion in flight shows actual first", s, v, true, "validator -> spare pending"},
		{"promotion in flight", v, s, true, "spare -> validator pending"},
		{"kill in flight", d, v, true, "validator -> dead pending"},
		{"mid-ratchet raw weight", s, 40_600, true, "w=40600 -> spare pending"},
		{"pchain unreadable falls back to desired", s, 0, false, "spare"},
	}
	for _, c := range cases {
		if got := stakeCell(c.desired, c.actual, c.haveActual); got != c.want {
			t.Errorf("%s: stakeCell(%d, %d, %v) = %q, want %q", c.name, c.desired, c.actual, c.haveActual, got, c.want)
		}
	}
}
