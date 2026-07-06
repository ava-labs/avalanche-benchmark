package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fujiTestConfig builds a two-site 3v/1s/2r config with a seeded intentions
// file. Permanent key scheme: KeyOf(i) = 6+i, so m1..m6 wear 6..11 (RPCs
// 10,11) and b1..b6 wear 12..17 (RPCs 16,17).
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
	stateFile := filepath.Join(t.TempDir(), "intentions.json")
	if err := saveIntents(stateFile, seedIntents(topo)); err != nil {
		t.Fatalf("saveIntents: %v", err)
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
		stateFile:   stateFile,
	}
}

func TestLoadNodeIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-ids.env")
	if err := os.WriteFile(path, []byte("L1_6_NODE_ID=NodeID-abc\nL1_17_NODE_ID=NodeID-xyz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadNodeIDs(path)
	if got[6] != "NodeID-abc" || got[17] != "NodeID-xyz" || len(got) != 2 {
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
	// 10,11), never the public peer, never site B's RPCs.
	ips, ids := c.pchainBeacons(0)
	if ips != "10.0.0.5:9653,10.0.0.6:9653" {
		t.Errorf("validator beacon ips = %q", ips)
	}
	if ids != c.nodeIDByKey[10]+","+c.nodeIDByKey[11] {
		t.Errorf("validator beacon ids = %q", ids)
	}

	// A site-B node follows site B's RPC slots (permanent keys 16,17).
	ips, ids = c.pchainBeacons(6)
	if ips != "10.1.0.5:9653,10.1.0.6:9653" {
		t.Errorf("site-B beacon ips = %q", ips)
	}
	if ids != c.nodeIDByKey[16]+","+c.nodeIDByKey[17] {
		t.Errorf("site-B beacon ids = %q", ids)
	}
}

func TestSiblingSeeds(t *testing.T) {
	c := fujiTestConfig(t)

	// m1 (key 6) seeds every OTHER slot under its permanent identity.
	ips, ids := c.siblingSeeds(0)
	ipList := strings.Split(ips, ",")
	idList := strings.Split(ids, ",")
	if len(ipList) != 11 || len(idList) != 11 {
		t.Fatalf("want 11 sibling seeds, got %d/%d", len(ipList), len(idList))
	}
	if ipList[0] != "10.0.0.2:9653" || idList[0] != c.nodeIDByKey[7] {
		t.Errorf("first seed = %s/%s, want m2 with key 7", ipList[0], idList[0])
	}
	// Spare m4 wears its permanent key 9.
	if idList[2] != c.nodeIDByKey[9] {
		t.Errorf("m4 seed id = %s, want key 9's NodeID", idList[2])
	}
	for _, id := range idList {
		if id == c.nodeIDByKey[6] {
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
