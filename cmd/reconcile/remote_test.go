package main

import (
	"strings"
	"testing"
)

// testInventory mirrors the shipped two-dc fleet: 4 validators + 2 rpc per dc,
// one node per box.
const testInventory = `
a1     host=10.0.0.1 role=validator dc=A
a2     host=10.0.0.2 role=validator dc=A
a3     host=10.0.0.3 role=validator dc=A
a4     host=10.0.0.4 role=validator dc=A
rpc_a1 host=10.0.0.5 role=rpc       dc=A
rpc_a2 host=10.0.0.6 role=rpc       dc=A
b1     host=10.1.0.1 role=validator dc=B
b2     host=10.1.0.2 role=validator dc=B
b3     host=10.1.0.3 role=validator dc=B
b4     host=10.1.0.4 role=validator dc=B
rpc_b1 host=10.1.0.5 role=rpc       dc=B
rpc_b2 host=10.1.0.6 role=rpc       dc=B
`

func testConfig(t *testing.T) *config {
	t.Helper()
	nodes := mustParse(t, testInventory)
	nodeIDs := map[string]string{}
	for _, n := range nodes {
		nodeIDs[n.Name] = "NodeID-" + n.Name
	}
	return &config{
		nodes:        nodes,
		instances:    buildInstances(nodes),
		remoteDir:    "~/avalanche-benchmark",
		chainID:      "CHAIN",
		subnetID:     "SUBNET",
		upstreamIPs:  "18.192.93.241:9651",
		upstreamIDs:  "NodeID-2m38qc95mhHXtrhjyGbe7r2NhniqHHJRB",
		nodeIDByName: nodeIDs,
	}
}

func TestPchainBeacons(t *testing.T) {
	c := testConfig(t)

	// rpc nodes (4,5 and 10,11) follow the pinned public anchor peer.
	for _, i := range []int{4, 5, 10, 11} {
		ips, ids := c.pchainBeacons(i)
		if ips != c.upstreamIPs || ids != c.upstreamIDs {
			t.Errorf("node %d: rpc beacons = %q/%q, want public upstream", i, ips, ids)
		}
	}

	// A validator follows ALL of the fleet's rpc nodes (dc is display-only),
	// never the public peer.
	ips, ids := c.pchainBeacons(0)
	if ips != "10.0.0.5:9651,10.0.0.6:9651,10.1.0.5:9651,10.1.0.6:9651" {
		t.Errorf("validator beacon ips = %q", ips)
	}
	if ids != "NodeID-rpc_a1,NodeID-rpc_a2,NodeID-rpc_b1,NodeID-rpc_b2" {
		t.Errorf("validator beacon ids = %q", ids)
	}
	if strings.Contains(ips, c.upstreamIPs) {
		t.Errorf("validator must not reach the public peer")
	}
}

func TestSiblingSeeds(t *testing.T) {
	c := testConfig(t)

	// a1 seeds every OTHER node under its permanent identity.
	ips, ids := c.siblingSeeds(0)
	ipList := strings.Split(ips, ",")
	idList := strings.Split(ids, ",")
	if len(ipList) != 11 || len(idList) != 11 {
		t.Fatalf("want 11 sibling seeds, got %d/%d", len(ipList), len(idList))
	}
	if ipList[0] != "10.0.0.2:9651" || idList[0] != "NodeID-a2" {
		t.Errorf("first seed = %s/%s, want a2", ipList[0], idList[0])
	}
	for _, id := range idList {
		if id == "NodeID-a1" {
			t.Errorf("a1's own identity found in its sibling seeds")
		}
	}
}

func TestStartScriptFlags(t *testing.T) {
	c := testConfig(t)

	script := c.startScript(0) // a1, a validator
	for _, want := range []string{
		"--network-id=fuji",
		"--partial-sync-primary-network=true",
		"--p-chain-follow-only=true",
		"--network-allow-private-ips=true",
		"--http-port=9650",
		"--staking-port=9651",
		"--data-dir=data/a1",
		"--db-dir=data/a1/db",
		"--staking-tls-cert-file=data/a1/staking/active/staker.crt",
		"--bootstrap-ips=10.0.0.5:9651,10.0.0.6:9651,10.1.0.5:9651,10.1.0.6:9651",
		"--state-sync-ips=",
		"--track-subnets=\"SUBNET\"",
		`cp chain-config.json "data/a1/configs/chains/CHAIN/config.json"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("validator start script missing %q", want)
		}
	}
	if strings.Contains(script, c.upstreamIPs) {
		t.Errorf("validator start script must not reach the public anchor peer")
	}

	script = c.startScript(4) // rpc_a1
	if !strings.Contains(script, "--bootstrap-ips="+c.upstreamIPs) ||
		!strings.Contains(script, "--bootstrap-ids="+c.upstreamIDs) {
		t.Errorf("rpc start script must bootstrap from the public anchor peer")
	}
	if !strings.Contains(script, "--data-dir=data/rpc_a1") {
		t.Errorf("rpc start script must use its own data root")
	}
}

// The stdout-capture cap and memory guard land verbatim in every rendered
// script (incident guards from 2026-07-10/11: disk-full from avalanchego.out
// tx spam, box-wedging OOM from a lagging plugin). The %-escapes in the Go
// template are the usual suspects, so pin the exact rendered shell.
func TestStartScriptGuards(t *testing.T) {
	c := testConfig(t)
	script := c.startScript(0)
	for _, want := range []string{
		`pkill -f "outwatch=data/a1;" || true`,
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
