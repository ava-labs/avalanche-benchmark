package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanchego/ids"
)

type recordingRunner struct {
	output []byte
	runs   [][]string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) error {
	r.runs = append(r.runs, append([]string{name}, args...))
	return nil
}

func (r *recordingRunner) Output(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return r.output, nil
}

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

func TestDeployModeIsExplicitAndFrozenRequiresArchive(t *testing.T) {
	deployer := NewDeployer(t.TempDir(), os.Stdout)
	if _, _, err := deployer.prepare(""); err == nil || !strings.Contains(err.Error(), "deploy mode") {
		t.Fatalf("missing mode error = %v", err)
	}
	if _, _, err := deployer.prepare(frozenMode); err == nil || !strings.Contains(err.Error(), pchainArchive) {
		t.Fatalf("missing frozen archive error = %v", err)
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
		followMode,
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

	frozenDir := filepath.Join(root, "frozen-beacon")
	if err := renderNode(
		frozenDir,
		root,
		environment,
		beacon,
		creation.PublicNode{Identity: "a", Role: config.RoleBeacon},
		chainID,
		subnetID,
		[2]int{9650, 9651},
		frozenMode,
		"",
		"",
		"",
		"",
	); err != nil {
		t.Fatal(err)
	}
	frozenConfig := readTestJSON(t, filepath.Join(frozenDir, "node.json"))
	if frozenConfig["bootstrap-ips"] != "" || frozenConfig["bootstrap-ids"] != "" {
		t.Fatalf("frozen beacon must have explicit-empty bootstrap peers: %v", frozenConfig)
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
		followMode,
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

func TestFrozenDeployPreservesExistingBeaconDatabase(t *testing.T) {
	runner := &recordingRunner{output: []byte("present")}
	var output bytes.Buffer
	deployer := &Deployer{root: t.TempDir(), out: &output, runner: runner}
	deployment := deployment{environment: config.FleetEnvironment{SSHUser: "ubuntu"}}
	beacon := nodeDeployment{node: config.Node{Number: 13, Host: "beacon"}}
	if err := deployer.seedBeacon(context.Background(), deployment, beacon); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("existing database triggered mutation: %v", runner.runs)
	}
	if !strings.Contains(output.String(), "preserving") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestFrozenDeployRestoresArchiveIntoEmptyBeaconDatabase(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, pchainArchive), "archive")
	runner := &recordingRunner{output: []byte("missing")}
	deployer := &Deployer{root: root, out: io.Discard, runner: runner}
	deployment := deployment{
		environment: config.FleetEnvironment{
			SSHUser:    "ubuntu",
			SSHKeyPath: "/key",
		},
	}
	beacon := nodeDeployment{node: config.Node{Number: 13, Host: "beacon"}}
	if err := deployer.seedBeacon(context.Background(), deployment, beacon); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 3 {
		t.Fatalf("restore commands = %d, want 3: %v", len(runner.runs), runner.runs)
	}
	extract := strings.Join(runner.runs[2], " ")
	if !strings.Contains(extract, "tar -xzf") ||
		!strings.Contains(extract, "/var/lib/avalanche-benchmark/13/db") {
		t.Fatalf("restore command = %q", extract)
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
