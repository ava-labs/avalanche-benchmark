package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanchego/ids"
)

func TestPortsArePositionalPerHost(t *testing.T) {
	nodes := []config.Node{
		{Number: 1, Host: "one"},
		{Number: 2, Host: "two"},
		{Number: 3, Host: "one"},
		{Number: 4, Host: "one"},
	}
	got := portsByNode(nodes)
	want := map[int][2]int{
		1: {9650, 9651},
		2: {9650, 9651},
		3: {9652, 9653},
		4: {9654, 9655},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %v, want %v", got, want)
	}
}

func TestRenderBeaconFollowsDefaultsAndL1UsesBeacon(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "node-config.json"), `{"index-enabled":false}`)
	writeTestFile(t, filepath.Join(root, "chain-config.json"), `{}`)
	writeTestFile(t, filepath.Join(root, "chain-config-rpc.json"), `{}`)
	writeTestFile(t, filepath.Join(root, "subnet-config.json"), `{"snowParameters":{"k":30,"alphaPreference":16,"alphaConfidence":17,"beta":12},"proposerWindowMilliseconds":100,"proposerMillisecondTimestamps":true}`)
	environment := config.FleetEnvironment{Network: "fuji", SSHUser: "ubuntu"}
	chainID := ids.GenerateTestID()
	subnetID := ids.GenerateTestID()

	beaconDir := filepath.Join(root, "beacon")
	beacon := config.Node{Number: 1, Role: config.RoleBeacon}
	if err := renderNode(
		beaconDir,
		root,
		environment,
		beacon,
		creation.PublicNode{Identity: "a", Role: config.RoleBeacon},
		chainID,
		subnetID,
		[2]int{9650, 9651},
		"",
		"",
		"",
		"",
	); err != nil {
		t.Fatal(err)
	}
	beaconConfig := readTestJSON(t, filepath.Join(beaconDir, "node.json"))
	if beaconConfig["p-chain-follow-only"] != true {
		t.Fatalf("beacon does not follow P-chain: %v", beaconConfig)
	}
	if beaconConfig["staking-ephemeral-signer-enabled"] != true {
		t.Fatal("unregistered P-chain beacon must use an ephemeral BLS signer")
	}
	if _, exists := beaconConfig["bootstrap-ips"]; exists {
		t.Fatal("following beacon must use AvalancheGo embedded bootstrappers")
	}
	if _, exists := beaconConfig["track-subnets"]; exists {
		t.Fatal("P-chain beacon must not track the L1")
	}
	unit, err := os.ReadFile(filepath.Join(beaconDir, "node.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(unit, []byte("/opt/avalanche-benchmark/1/bin/avalanchego")) {
		t.Fatalf("systemd unit does not use the node's own binary: %s", unit)
	}

	validatorDir := filepath.Join(root, "validator")
	validator := config.Node{Number: 2, Role: config.RoleValidator}
	if err := renderNode(
		validatorDir,
		root,
		environment,
		validator,
		creation.PublicNode{Identity: "b", Role: config.RoleValidator},
		chainID,
		subnetID,
		[2]int{9650, 9651},
		"beacon:9651",
		"NodeID-beacon",
		"sibling:9651",
		"NodeID-sibling",
	); err != nil {
		t.Fatal(err)
	}
	validatorConfig := readTestJSON(t, filepath.Join(validatorDir, "node.json"))
	if validatorConfig["bootstrap-ips"] != "beacon:9651" || validatorConfig["bootstrap-ids"] != "NodeID-beacon" {
		t.Fatalf("validator does not use sole P-chain beacon for bootstrap: %v", validatorConfig)
	}
	if validatorConfig["state-sync-ips"] != "sibling:9651" || validatorConfig["state-sync-ids"] != "NodeID-sibling" {
		t.Fatalf("validator does not use L1 sibling for state sync: %v", validatorConfig)
	}
	if validatorConfig["staking-signer-key-file"] == nil {
		t.Fatal("validator signer path missing")
	}
}

func TestStateSyncPeersExcludeBeaconAndSelf(t *testing.T) {
	nodes := []config.Node{
		{Number: 1, Host: "validator-a", Role: config.RoleValidator},
		{Number: 2, Host: "validator-b", Role: config.RoleValidator},
		{Number: 3, Host: "rpc", Role: config.RoleRPC},
		{Number: 4, Host: "beacon", Role: config.RoleBeacon},
	}
	public := map[int]creation.PublicNode{
		1: {NodeID: "NodeID-a"},
		2: {NodeID: "NodeID-b"},
		3: {NodeID: "NodeID-rpc"},
		4: {NodeID: "NodeID-beacon"},
	}
	ips, nodeIDs := stateSyncPeers(nodes[0], nodes, public, portsByNode(nodes))
	if ips != "validator-b:9651,rpc:9651" {
		t.Fatalf("state sync IPs = %q", ips)
	}
	if nodeIDs != "NodeID-b,NodeID-rpc" {
		t.Fatalf("state sync IDs = %q", nodeIDs)
	}
}

func TestPhaseStopsAtFirstFailure(t *testing.T) {
	deployer := &Deployer{out: os.Stdout}
	nodes := []nodeDeployment{
		{node: config.Node{Number: 1}},
		{node: config.Node{Number: 2}},
		{node: config.Node{Number: 3}},
	}
	var visited []int
	err := deployer.phase(context.Background(), deployment{selected: nodes}, "test", func(_ context.Context, _ deployment, node nodeDeployment) error {
		visited = append(visited, node.node.Number)
		if node.node.Number == 2 {
			return errors.New("failed")
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected phase failure")
	}
	if !reflect.DeepEqual(visited, []int{1, 2}) {
		t.Fatalf("phase continued after failure: %v", visited)
	}
}

func readTestJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value := make(map[string]any)
	if err := json.Unmarshal(contents, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
