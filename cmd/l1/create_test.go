package main

import "testing"

func TestPlanValidatorsDefaultHalves(t *testing.T) {
	// No topology env, no --validators: 8 validators, a1-a4 active, b1-b4 standby.
	t.Setenv("VALIDATOR_IPS", "")
	t.Setenv("SPARE_IPS", "")
	t.Setenv("RPC_IPS", "")
	plans := planValidators(createOpts{activeWeight: defaultActiveWeight, standbyWeight: defaultStandbyWeight})
	if len(plans) != 8 {
		t.Fatalf("default plan has %d validators, want 8", len(plans))
	}
	wantNames := []string{"a1", "a2", "a3", "a4", "b1", "b2", "b3", "b4"}
	for i, p := range plans {
		if p.key != i+1 {
			t.Errorf("plan[%d].key = %d, want %d", i, p.key, i+1)
		}
		if p.name != wantNames[i] {
			t.Errorf("plan[%d].name = %q, want %q", i, p.name, wantNames[i])
		}
		want := defaultActiveWeight
		if i >= 4 {
			want = defaultStandbyWeight
		}
		if p.weight != want {
			t.Errorf("plan[%d].weight = %d, want %d", i, p.weight, want)
		}
	}
}

func TestPlanValidatorsExplicitOdd(t *testing.T) {
	plans := planValidators(createOpts{validators: 3, activeWeight: 100, standbyWeight: 10})
	wantNames := []string{"a1", "a2", "b1"}
	wantWeights := []uint64{100, 100, 10}
	for i, p := range plans {
		if p.name != wantNames[i] || p.weight != wantWeights[i] || p.key != i+1 {
			t.Errorf("plan[%d] = %+v, want name=%s weight=%d key=%d", i, p, wantNames[i], wantWeights[i], i+1)
		}
	}
}

func TestPlanValidatorsFromTopology(t *testing.T) {
	t.Setenv("VALIDATOR_IPS", "1.1.1.1,1.1.1.2,1.1.1.3")
	t.Setenv("SPARE_IPS", "1.1.1.4")
	t.Setenv("RPC_IPS", "1.1.1.5,1.1.1.6")
	t.Setenv("BACKUP_VALIDATOR_IPS", "2.1.1.1,2.1.1.2,2.1.1.3")
	t.Setenv("BACKUP_SPARE_IPS", "2.1.1.4")
	t.Setenv("BACKUP_RPC_IPS", "2.1.1.5,2.1.1.6")
	plans := planValidators(createOpts{activeWeight: defaultActiveWeight, standbyWeight: defaultStandbyWeight})
	if len(plans) != 8 {
		t.Fatalf("topology plan has %d validators, want 8 (staking slots)", len(plans))
	}
	wantNames := []string{"a1", "a2", "a3", "a4", "b1", "b2", "b3", "b4"}
	for i, p := range plans {
		if p.name != wantNames[i] || p.key != i+1 {
			t.Errorf("plan[%d] = name %q key %d, want %q key %d", i, p.name, p.key, wantNames[i], i+1)
		}
	}
	if plans[3].weight != defaultActiveWeight || plans[4].weight != defaultStandbyWeight {
		t.Errorf("site split wrong: a4=%d b1=%d", plans[3].weight, plans[4].weight)
	}
}
