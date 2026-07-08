package topo

import (
	"reflect"
	"testing"
)

func TestStakingSlotsTwoSite(t *testing.T) {
	tp := Topology{TwoSite: true, NVal: 3, NSpare: 1, NRPC: 2}
	// Per site [v v v spare rpc rpc]: staking slots skip the RPCs.
	want := []int{0, 1, 2, 3, 6, 7, 8, 9}
	if got := tp.StakingSlots(); !reflect.DeepEqual(got, want) {
		t.Fatalf("StakingSlots = %v, want %v", got, want)
	}
	for k, s := range want {
		if tp.StakingIndex(s) != k {
			t.Errorf("StakingIndex(%d) = %d, want %d", s, tp.StakingIndex(s), k)
		}
	}
	if tp.StakingIndex(4) != -1 || tp.StakingIndex(11) != -1 {
		t.Error("RPC slots must have StakingIndex -1")
	}
	if got := tp.AllKeys(); got[0] != 1 || got[11] != 12 || len(got) != 12 {
		t.Errorf("AllKeys = %v", got)
	}
}

func TestMachineName(t *testing.T) {
	tp := Topology{TwoSite: true, NVal: 3, NSpare: 1, NRPC: 2}
	want := []string{
		"a1", "a2", "a3", "a4", "rpc_a1", "rpc_a2",
		"b1", "b2", "b3", "b4", "rpc_b1", "rpc_b2",
	}
	for i, w := range want {
		if got := tp.MachineName(i); got != w {
			t.Errorf("MachineName(%d) = %q, want %q", i, got, w)
		}
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{
		"VALIDATOR_IPS":        "1.1.1.1,1.1.1.2,1.1.1.3",
		"SPARE_IPS":            "1.1.1.4",
		"RPC_IPS":              "1.1.1.5,1.1.1.6",
		"BACKUP_VALIDATOR_IPS": "2.1.1.1,2.1.1.2,2.1.1.3",
		"BACKUP_SPARE_IPS":     "2.1.1.4",
		"BACKUP_RPC_IPS":       "2.1.1.5,2.1.1.6",
	}
	tp, pool, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !tp.TwoSite || tp.NVal != 3 || tp.NSpare != 1 || tp.NRPC != 2 || len(pool) != 12 {
		t.Fatalf("FromEnv = %+v pool=%d", tp, len(pool))
	}
	if pool[0] != "1.1.1.1" || pool[6] != "2.1.1.1" || pool[11] != "2.1.1.6" {
		t.Errorf("pool order wrong: %v", pool)
	}

	delete(env, "BACKUP_VALIDATOR_IPS")
	tp, pool, err = FromEnv(func(k string) string { return env[k] })
	if err != nil || tp.TwoSite || len(pool) != 6 {
		t.Fatalf("single-site FromEnv = %+v pool=%d err=%v", tp, len(pool), err)
	}

	env["BACKUP_VALIDATOR_IPS"] = "2.1.1.1" // shape mismatch
	if _, _, err := FromEnv(func(k string) string { return env[k] }); err == nil {
		t.Error("mismatched backup shape must error")
	}
}
