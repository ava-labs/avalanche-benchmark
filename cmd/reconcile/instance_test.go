package main

import "testing"

func TestMakeInstanceFirstOccurrenceIsLegacyLayout(t *testing.T) {
	// idx 0 MUST reproduce the single-process layout byte-for-byte - this is what
	// keeps a 6-distinct-IP deploy identical to the validated prod path.
	in := makeInstance("10.0.0.1", 0)
	if in.httpPort != 9652 || in.stakingPort != 9653 {
		t.Errorf("idx0 ports = %d/%d, want 9652/9653", in.httpPort, in.stakingPort)
	}
	if in.dataDir != "data/validator" {
		t.Errorf("idx0 dataDir = %q, want data/validator", in.dataDir)
	}
	if in.activeDir != "staking/active" {
		t.Errorf("idx0 activeDir = %q, want staking/active", in.activeDir)
	}
	if in.chainCfg != "chain-config.json" {
		t.Errorf("idx0 chainCfg = %q, want chain-config.json", in.chainCfg)
	}
	if in.startScript != "start-l1-validator.sh" {
		t.Errorf("idx0 startScript = %q, want start-l1-validator.sh", in.startScript)
	}
}

func TestMakeInstanceColocatedOffsets(t *testing.T) {
	in := makeInstance("10.0.0.1", 1)
	if in.httpPort != 9662 || in.stakingPort != 9663 {
		t.Errorf("idx1 ports = %d/%d, want 9662/9663", in.httpPort, in.stakingPort)
	}
	if in.dataDir != "data/validator-1" || in.activeDir != "staking/active-1" {
		t.Errorf("idx1 dirs = %q,%q want data/validator-1, staking/active-1", in.dataDir, in.activeDir)
	}
	if in.chainCfg != "chain-config-1.json" || in.startScript != "start-l1-validator-1.sh" {
		t.Errorf("idx1 files = %q,%q", in.chainCfg, in.startScript)
	}
	in2 := makeInstance("10.0.0.1", 2)
	if in2.httpPort != 9672 || in2.stakingPort != 9673 {
		t.Errorf("idx2 ports = %d/%d, want 9672/9673", in2.httpPort, in2.stakingPort)
	}
}

func TestMakeInstanceProcPatAvoidsSelfMatch(t *testing.T) {
	// The bracketed first digit makes the regex match the avalanchego argv
	// ("--http-port=9662") but NOT the literal pgrep argv ("--http-port=[9]662").
	if got := makeInstance("10.0.0.1", 1).procPat; got != "--http-port=[9]662" {
		t.Errorf("procPat = %q, want --http-port=[9]662", got)
	}
}

func TestBuildInstancesAllDistinctIsLegacy(t *testing.T) {
	// 6 distinct IPs: every instance is idx0, non-shared, at the legacy ports/dirs.
	pool := []string{"a", "b", "c", "d", "e", "f"}
	insts := buildInstances(pool)
	for i, in := range insts {
		if in.idx != 0 || in.shared {
			t.Errorf("slot %d: idx=%d shared=%v, want 0/false (distinct IPs)", i, in.idx, in.shared)
		}
		if in.httpPort != 9652 || in.dataDir != "data/validator" {
			t.Errorf("slot %d: distinct IP must use legacy layout, got :%d %s", i, in.httpPort, in.dataDir)
		}
	}
}

func TestBuildInstancesColocationByOrderOfAppearance(t *testing.T) {
	// 3 boxes hosting 6 slots: NODE_IPS=a,b,c,a,b,c. Slots 0-2 are the first
	// occurrence of each box (idx0), slots 3-5 the second (idx1), all shared.
	pool := []string{"a", "b", "c", "a", "b", "c"}
	insts := buildInstances(pool)

	want := []struct {
		host string
		idx  int
		http int
		data string
	}{
		{"a", 0, 9652, "data/validator"},
		{"b", 0, 9652, "data/validator"},
		{"c", 0, 9652, "data/validator"},
		{"a", 1, 9662, "data/validator-1"},
		{"b", 1, 9662, "data/validator-1"},
		{"c", 1, 9662, "data/validator-1"},
	}
	for i, w := range want {
		in := insts[i]
		if in.host != w.host || in.idx != w.idx || in.httpPort != w.http || in.dataDir != w.data {
			t.Errorf("slot %d = {host:%s idx:%d http:%d data:%s}, want {host:%s idx:%d http:%d data:%s}",
				i, in.host, in.idx, in.httpPort, in.dataDir, w.host, w.idx, w.http, w.data)
		}
		if !in.shared {
			t.Errorf("slot %d: shared=false, want true (box %s is co-located)", i, in.host)
		}
	}
}

func TestBuildInstancesMixedSharing(t *testing.T) {
	// Box "a" hosts two slots (shared); "b" and "c" host one each (not shared).
	pool := []string{"a", "b", "c", "a"}
	insts := buildInstances(pool)
	if !insts[0].shared || !insts[3].shared {
		t.Errorf("box a slots should be shared: %v %v", insts[0].shared, insts[3].shared)
	}
	if insts[1].shared || insts[2].shared {
		t.Errorf("boxes b,c should not be shared: %v %v", insts[1].shared, insts[2].shared)
	}
	if insts[3].idx != 1 || insts[3].httpPort != 9662 {
		t.Errorf("second 'a' = idx %d :%d, want idx1 :9662", insts[3].idx, insts[3].httpPort)
	}
}
