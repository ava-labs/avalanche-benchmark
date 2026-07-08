package main

import (
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/valmgr"
)

func TestParseWeightArgsSingleTier(t *testing.T) {
	topo := stdTwoSite()
	w, ms, err := parseWeightArgs([]string{"validator", "1", "2", "3", "4"}, topo)
	if err != nil {
		t.Fatal(err)
	}
	if w != valmgr.ValidatorWeight {
		t.Errorf("weight = %d, want %d", w, valmgr.ValidatorWeight)
	}
	if len(ms) != 4 || ms[0] != 1 || ms[3] != 4 {
		t.Errorf("machines = %v, want [1 2 3 4]", ms)
	}
}

// A second tier word anywhere in the args is an error: one tier per invocation.
func TestParseWeightArgsRejectsSecondTier(t *testing.T) {
	topo := stdTwoSite()
	if _, _, err := parseWeightArgs([]string{"validator", "7", "8", "9", "dead", "1", "2", "3"}, topo); err == nil {
		t.Error("multi-tier form must be rejected")
	}
}

func TestParseWeightArgsErrors(t *testing.T) {
	topo := stdTwoSite()
	for _, args := range [][]string{
		{},                        // no args
		{"validator"},             // tier without machines
		{"1", "2"},                // machine before tier
		{"validator", "99"},       // out of range
		{"validator", "1", "1"},   // duplicate
		{"validator", "1", "--x"}, // flag-shaped
	} {
		if _, _, err := parseWeightArgs(args, topo); err == nil {
			t.Errorf("parseWeightArgs(%v) must fail", args)
		}
	}
}

// Exactly `weight validator 1 2 3 4 5`: machine 5 is rpc_a1, so the command
// must fail naming it, and the intents built so far must never be saved. The
// command applies setWeight machine by machine and fatals before any save, so
// the atomicity contract here is: the error fires and prev is untouched.
func TestWeightValidatorHitsRPCSlotAtomically(t *testing.T) {
	topo := stdTopo() // one site: a1 a2 a3 a4 rpc_a1 rpc_a2 -> machine 5 is rpc_a1
	w, ms, err := parseWeightArgs([]string{"validator", "1", "2", "3", "4", "5"}, topo)
	if err != nil {
		t.Fatal(err)
	}
	prev := seedIntents(topo)
	intents := prev
	var applyErr error
	for _, m := range ms {
		next, err := setWeight(intents, m, w, topo)
		if err != nil {
			applyErr = err
			break
		}
		intents = next
	}
	if applyErr == nil {
		t.Fatal("weight validator 1 2 3 4 5 must fail on machine 5 (rpc_a1)")
	}
	if !strings.Contains(applyErr.Error(), "rpc_a1 is an RPC node") {
		t.Errorf("error must name the RPC slot, got: %v", applyErr)
	}
	for i, in := range prev {
		if in != seedIntents(topo)[i] {
			t.Errorf("slot %d of the original intents was mutated: %+v", i, in)
		}
	}
}
