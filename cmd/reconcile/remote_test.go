package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fujiTestConfig builds a two-site 3v/1s/2r config. Permanent key scheme:
// KeyOf(i) = 1+i, so site A slots wear keys 1..6 (RPCs 5,6) and site B slots
// wear 7..12 (RPCs 11,12).
func fujiTestConfig(t *testing.T) *config {
	t.Helper()
	topo := Topology{TwoSite: true, NVal: 3, NSpare: 1, NRPC: 2}
	pool := []string{
		"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6",
		"10.1.0.1", "10.1.0.2", "10.1.0.3", "10.1.0.4", "10.1.0.5", "10.1.0.6",
	}
	nodeIDs := map[int]string{}
	for _, k := range topo.AllKeys() {
		nodeIDs[k] = "NodeID-key" + strconv.Itoa(k)
	}
	return &config{
		topo:        topo,
		nodeIPs:     pool,
		instances:   buildInstances(pool),
		remoteDir:   "~/avalanche-benchmark",
		chainID:     "CHAIN",
		subnetID:    "SUBNET",
		upstreamIPs: "18.192.93.241:9651",
		upstreamIDs: "NodeID-2m38qc95mhHXtrhjyGbe7r2NhniqHHJRB",
		nodeIDByKey: nodeIDs,
	}
}

func TestLoadNodeIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-ids.env")
	if err := os.WriteFile(path, []byte("L1_1_NODE_ID=NodeID-abc\nL1_12_NODE_ID=NodeID-xyz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadNodeIDs(path)
	if got[1] != "NodeID-abc" || got[12] != "NodeID-xyz" || len(got) != 2 {
		t.Errorf("loadNodeIDs = %v", got)
	}
}

func TestPchainBeacons(t *testing.T) {
	c := fujiTestConfig(t)

	// RPC slots (4,5 site A; 10,11 site B) follow the pinned public Fuji peer.
	for _, i := range []int{4, 5, 10, 11} {
		ips, ids := c.pchainBeacons(i)
		if ips != c.upstreamIPs || ids != c.upstreamIDs {
			t.Errorf("slot %d: RPC beacons = %q/%q, want public upstream", i, ips, ids)
		}
	}

	// A site-A validator follows site A's RPC slots ONLY (their permanent keys
	// 9,10: RPC identities come after the 8 staking keys), never the public
	// peer, never site B's RPCs.
	ips, ids := c.pchainBeacons(0)
	if ips != "10.0.0.5:9653,10.0.0.6:9653" {
		t.Errorf("validator beacon ips = %q", ips)
	}
	if ids != c.nodeIDByKey[9]+","+c.nodeIDByKey[10] {
		t.Errorf("validator beacon ids = %q", ids)
	}

	// A site-B node follows site B's RPC slots (permanent keys 11,12).
	ips, ids = c.pchainBeacons(6)
	if ips != "10.1.0.5:9653,10.1.0.6:9653" {
		t.Errorf("site-B beacon ips = %q", ips)
	}
	if ids != c.nodeIDByKey[11]+","+c.nodeIDByKey[12] {
		t.Errorf("site-B beacon ids = %q", ids)
	}
}

func TestSiblingSeeds(t *testing.T) {
	c := fujiTestConfig(t)

	// a1 (key 1) seeds every OTHER slot under its permanent identity.
	ips, ids := c.siblingSeeds(0)
	ipList := strings.Split(ips, ",")
	idList := strings.Split(ids, ",")
	if len(ipList) != 11 || len(idList) != 11 {
		t.Fatalf("want 11 sibling seeds, got %d/%d", len(ipList), len(idList))
	}
	if ipList[0] != "10.0.0.2:9653" || idList[0] != c.nodeIDByKey[2] {
		t.Errorf("first seed = %s/%s, want m2 with key 2", ipList[0], idList[0])
	}
	// Spare a4 wears its permanent key 4.
	if idList[2] != c.nodeIDByKey[4] {
		t.Errorf("a4 seed id = %s, want key 4's NodeID", idList[2])
	}
	for _, id := range idList {
		if id == c.nodeIDByKey[1] {
			t.Errorf("slot 0's own identity found in its sibling seeds")
		}
	}
}

func TestStartScriptFujiFlags(t *testing.T) {
	c := fujiTestConfig(t)

	script := c.startScript(0) // site-A validator
	for _, want := range []string{
		"--network-id=fuji",
		"--partial-sync-primary-network=true",
		"--p-chain-follow-only=true",
		"--network-allow-private-ips=true",
		"--bootstrap-ips=10.0.0.5:9653,10.0.0.6:9653",
		"--state-sync-ips=",
		"--track-subnets=\"SUBNET\"",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("validator start script missing %q", want)
		}
	}
	if strings.Contains(script, c.upstreamIPs) {
		t.Errorf("validator start script must not reach the public Fuji peer")
	}

	script = c.startScript(4) // site-A RPC
	if !strings.Contains(script, "--bootstrap-ips="+c.upstreamIPs) ||
		!strings.Contains(script, "--bootstrap-ids="+c.upstreamIDs) {
		t.Errorf("RPC start script must bootstrap from the public Fuji peer")
	}
}

// The stdout-capture cap and memory guard land verbatim in every rendered
// script (incident guards from 2026-07-10/11: disk-full from avalanchego.out
// tx spam, box-wedging OOM from a lagging plugin). The %-escapes in the Go
// template are the usual suspects, so pin the exact rendered shell.
func TestStartScriptGuards(t *testing.T) {
	c := fujiTestConfig(t)
	script := c.startScript(0)
	for _, want := range []string{
		`pkill -f "outwatch=data/validator;" || true`,
		`stat -c%s "$outwatch/logs/avalanchego.out"`,
		`-gt 2147483648`,
		`truncate -s 0 "$outwatch/logs/avalanchego.out"`,
		`export GOMEMLIMIT=$(awk '/MemTotal/{printf "%dB", $2*1024*3/4}' /proc/meminfo)`,
		`echo 500 > /proc/self/oom_score_adj || true`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("start script missing %q", want)
		}
	}
	t.Logf("rendered start script for a1:\n%s", script)
}
