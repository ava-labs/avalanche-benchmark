package main

import (
	"testing"
)

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
