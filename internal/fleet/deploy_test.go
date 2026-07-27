package fleet

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	output    []byte
	runs      [][]string
	runErrors map[int]error
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) error {
	r.runs = append(r.runs, append([]string{name}, args...))
	return r.runErrors[len(r.runs)-1]
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

func TestDeployModeIsExplicitAndFrozenDeployValidatesArchive(t *testing.T) {
	deployer := NewDeployer(t.TempDir(), os.Stdout)
	if _, _, err := deployer.prepare("", true); err == nil || !strings.Contains(err.Error(), "deploy mode") {
		t.Fatalf("missing mode error = %v", err)
	}
	if err := deployer.validateFrozenDeployArchive(); err == nil || !strings.Contains(err.Error(), pchainArchive) {
		t.Fatalf("missing frozen archive error = %v", err)
	}

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, pchainArchive), "not an archive")
	deployer = NewDeployer(root, os.Stdout)
	if err := deployer.validateFrozenDeployArchive(); err == nil || !strings.Contains(err.Error(), "valid") {
		t.Fatalf("malformed frozen archive error = %v", err)
	}

	root = t.TempDir()
	writeFleetInputs(t, root)
	deployer = NewDeployer(root, os.Stdout)
	_, _, err := deployer.prepare(frozenMode, false)
	if err == nil || strings.Contains(err.Error(), pchainArchive) {
		t.Fatalf("generic frozen-mode preparation still requires archive: %v", err)
	}
}

func TestFrozenDeployValidatesConfigurationBeforeArchive(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, pchainArchive), "not an archive")
	deployer := NewDeployer(root, io.Discard)
	err := deployer.Deploy(context.Background(), frozenMode, nil)
	if err == nil || !strings.Contains(err.Error(), ".env") {
		t.Fatalf("frozen deploy did not report configuration first: %v", err)
	}
	if strings.Contains(err.Error(), "valid ./"+pchainArchive) {
		t.Fatalf("frozen deploy scanned archive before configuration: %v", err)
	}
}

func TestRenderPChainFollowsDefaultsAndL1UsesPChain(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "node-config.json"), `{"index-enabled":false}`)
	writeTestFile(t, filepath.Join(root, "chain-config.json"), `{}`)
	writeTestFile(t, filepath.Join(root, "chain-config-rpc.json"), `{}`)
	writeTestFile(t, filepath.Join(root, "subnet-config.json"), `{"snowParameters":{"k":20,"alphaPreference":11,"alphaConfidence":11,"beta":12},"proposerWindowMilliseconds":50,"proposerMillisecondTimestamps":true}`)
	environment := config.FleetEnvironment{Network: "fuji", SSHUser: "ubuntu"}
	chainID := ids.GenerateTestID()
	subnetID := ids.GenerateTestID()

	pchainDir := filepath.Join(root, "pchain")
	pchain := config.Node{Number: 1, Role: config.RolePChain}
	if err := renderNode(
		pchainDir,
		root,
		environment,
		pchain,
		creation.PublicNode{Identity: "a", Role: config.RolePChain},
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
	pchainConfig := readTestJSON(t, filepath.Join(pchainDir, "node.json"))
	if pchainConfig["p-chain-follow-only"] != true {
		t.Fatalf("pchain does not follow P-chain: %v", pchainConfig)
	}
	if pchainConfig["staking-ephemeral-signer-enabled"] != true {
		t.Fatal("unregistered P-chain node must use an ephemeral BLS signer")
	}
	if _, exists := pchainConfig["bootstrap-ips"]; exists {
		t.Fatal("following P-chain node must use AvalancheGo embedded bootstrappers")
	}
	if _, exists := pchainConfig["track-subnets"]; exists {
		t.Fatal("P-chain node must not track the L1")
	}

	frozenDir := filepath.Join(root, "frozen-pchain")
	if err := renderNode(
		frozenDir,
		root,
		environment,
		pchain,
		creation.PublicNode{Identity: "a", Role: config.RolePChain},
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
		t.Fatalf("frozen P-chain node must have explicit-empty bootstrap peers: %v", frozenConfig)
	}

	unit, err := os.ReadFile(filepath.Join(pchainDir, "node.service"))
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
		"pchain:9651",
		"NodeID-pchain",
		"sibling:9651",
		"NodeID-sibling",
	); err != nil {
		t.Fatal(err)
	}
	validatorConfig := readTestJSON(t, filepath.Join(validatorDir, "node.json"))
	if validatorConfig["bootstrap-ips"] != "pchain:9651" || validatorConfig["bootstrap-ids"] != "NodeID-pchain" {
		t.Fatalf("validator does not use sole P-chain node for bootstrap: %v", validatorConfig)
	}
	if validatorConfig["state-sync-ips"] != "sibling:9651" || validatorConfig["state-sync-ids"] != "NodeID-sibling" {
		t.Fatalf("validator does not use L1 sibling for state sync: %v", validatorConfig)
	}
	if validatorConfig["staking-signer-key-file"] == nil {
		t.Fatal("validator signer path missing")
	}
}

func TestStateSyncPeersExcludePChainAndSelf(t *testing.T) {
	nodes := []config.Node{
		{Number: 1, Host: "validator-a", Role: config.RoleValidator},
		{Number: 2, Host: "validator-b", Role: config.RoleValidator},
		{Number: 3, Host: "rpc", Role: config.RoleRPC},
		{Number: 4, Host: "pchain", Role: config.RolePChain},
	}
	public := map[int]creation.PublicNode{
		1: {NodeID: "NodeID-a"},
		2: {NodeID: "NodeID-b"},
		3: {NodeID: "NodeID-rpc"},
		4: {NodeID: "NodeID-pchain"},
	}
	ips, nodeIDs := stateSyncPeers(nodes[0], nodes, public, portsByNode(nodes))
	if ips != "validator-b:9651,rpc:9651" {
		t.Fatalf("state sync IPs = %q", ips)
	}
	if nodeIDs != "NodeID-b,NodeID-rpc" {
		t.Fatalf("state sync IDs = %q", nodeIDs)
	}
}

func TestFrozenDeployPreservesExistingPChainDatabase(t *testing.T) {
	runner := &recordingRunner{output: []byte("present")}
	var output bytes.Buffer
	deployer := &Deployer{root: t.TempDir(), out: &output, runner: runner}
	deployment := deployment{environment: config.FleetEnvironment{SSHUser: "ubuntu"}}
	pchain := nodeDeployment{node: config.Node{Number: 13, Host: "pchain"}}
	if err := deployer.seedPChain(context.Background(), deployment, pchain); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("existing database triggered mutation: %v", runner.runs)
	}
	if !strings.Contains(output.String(), "preserving") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestFrozenDeployRestoresArchiveIntoEmptyPChainDatabase(t *testing.T) {
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
	pchain := nodeDeployment{node: config.Node{Number: 13, Host: "pchain"}}
	if err := deployer.seedPChain(context.Background(), deployment, pchain); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 3 {
		t.Fatalf("restore commands = %d, want 3: %v", len(runner.runs), runner.runs)
	}
	extract := strings.Join(runner.runs[2], " ")
	if !strings.Contains(extract, "tar -xzf") ||
		!strings.Contains(extract, "mv -T") ||
		!strings.Contains(extract, "/var/lib/avalanche-benchmark/13/db") {
		t.Fatalf("restore command = %q", extract)
	}
	if strings.Contains(extract, "rm -rf /var/lib/avalanche-benchmark/13/db") {
		t.Fatalf("restore deletes authoritative database instead of atomically replacing an empty one: %q", extract)
	}
}

func TestPChainPackageDoesNotTransferL1Plugin(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "bin", "avalanchego"), "binary")
	renderDir := filepath.Join(root, "render")
	if err := os.Mkdir(renderDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(renderDir, "node.json"), "{}")
	runner := &recordingRunner{}
	deployer := &Deployer{root: root, out: io.Discard, runner: runner}
	state := deployment{environment: config.FleetEnvironment{SSHUser: "ubuntu"}}
	node := nodeDeployment{
		node:      config.Node{Number: 6, Host: "pchain", Role: config.RolePChain},
		renderDir: renderDir,
	}
	if err := deployer.installPackage(context.Background(), state, node); err != nil {
		t.Fatal(err)
	}
	allCommands := make([]string, 0, len(runner.runs))
	for _, command := range runner.runs {
		allCommands = append(allCommands, strings.Join(command, " "))
	}
	joined := strings.Join(allCommands, "\n")
	if strings.Contains(joined, pluginID) || strings.Contains(joined, "/plugins") {
		t.Fatalf("P-chain package included L1 plugin: %s", joined)
	}
}

func TestL1PackageTransfersAndInstallsPlugin(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "bin", "avalanchego"), "binary")
	writeTestFile(t, filepath.Join(root, "bin", pluginID), "plugin")
	renderDir := filepath.Join(root, "render")
	if err := os.Mkdir(renderDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"node.json", "chain.json", "subnet.json"} {
		writeTestFile(t, filepath.Join(renderDir, name), "{}")
	}
	runner := &recordingRunner{}
	deployer := &Deployer{root: root, out: io.Discard, runner: runner}
	state := deployment{
		environment: config.FleetEnvironment{SSHUser: "ubuntu"},
		chainID:     ids.GenerateTestID(),
		subnetID:    ids.GenerateTestID(),
	}
	node := nodeDeployment{
		node:      config.Node{Number: 1, Host: "validator", Role: config.RoleValidator},
		renderDir: renderDir,
	}
	if err := deployer.installPackage(context.Background(), state, node); err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, command := range runner.runs {
		commands = append(commands, strings.Join(command, " "))
	}
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, filepath.Join(root, "bin", pluginID)) ||
		!strings.Contains(joined, "/plugins/"+pluginID) {
		t.Fatalf("L1 package did not transfer and install plugin: %s", joined)
	}
}

func TestReconcilePChainTouchesOnlyPChainAndVerifiesService(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "bin", "avalanchego"), "binary")
	renderDir := filepath.Join(root, "render")
	if err := os.Mkdir(renderDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(renderDir, "node.json"), "{}")
	writeTestFile(t, filepath.Join(renderDir, "node.service"), "unit")
	identityDir := filepath.Join(root, "deployment", "identities", "p")
	if err := os.MkdirAll(identityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(identityDir, "staker.crt"), "cert")
	writeTestFile(t, filepath.Join(identityDir, "staker.key"), "key")

	runner := &recordingRunner{}
	deployer := &Deployer{root: root, out: io.Discard, runner: runner}
	pchain := nodeDeployment{
		node:      config.Node{Number: 6, Host: "pchain-host", Role: config.RolePChain},
		identity:  creation.PublicNode{Identity: "p", Role: config.RolePChain},
		renderDir: renderDir,
	}
	state := deployment{
		environment: config.FleetEnvironment{SSHUser: "ubuntu"},
		pchain:      pchain,
		selected: []nodeDeployment{{
			node: config.Node{Number: 1, Host: "validator-host", Role: config.RoleValidator},
		}},
	}
	if err := deployer.reconcilePChain(context.Background(), state, false); err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, command := range runner.runs {
		commands = append(commands, strings.Join(command, " "))
	}
	joined := strings.Join(commands, "\n")
	if strings.Contains(joined, "validator-host") {
		t.Fatalf("P-chain reconciliation touched validator: %s", joined)
	}
	if !strings.Contains(commands[len(commands)-1], "systemctl is-active --quiet") {
		t.Fatalf("P-chain start did not verify service: %s", commands[len(commands)-1])
	}
}

func TestArchiveRestartsPChainNodeWhenTarFails(t *testing.T) {
	root := t.TempDir()
	writeFleetInputs(t, root)
	runner := &recordingRunner{
		runErrors: map[int]error{2: errors.New("tar failed")},
	}
	deployer := &Deployer{root: root, out: io.Discard, runner: runner}
	err := deployer.ArchivePChain(context.Background())
	if err == nil || !strings.Contains(err.Error(), "archive P-chain database") {
		t.Fatalf("archive error = %v", err)
	}
	if len(runner.runs) < 4 || !strings.Contains(strings.Join(runner.runs[3], " "), "systemctl start") {
		t.Fatalf("P-chain node was not restarted after archive failure: %v", runner.runs)
	}
}

func TestArchiveRestartsPChainNodeAfterUncertainStop(t *testing.T) {
	root := t.TempDir()
	writeFleetInputs(t, root)
	runner := &recordingRunner{
		runErrors: map[int]error{1: errors.New("connection lost during stop")},
	}
	deployer := &Deployer{root: root, out: io.Discard, runner: runner}
	err := deployer.ArchivePChain(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stop P-chain node") {
		t.Fatalf("archive error = %v", err)
	}
	if len(runner.runs) < 3 || !strings.Contains(strings.Join(runner.runs[2], " "), "systemctl start") {
		t.Fatalf("P-chain node was not restarted after uncertain stop: %v", runner.runs)
	}
}

func TestValidatePChainArchive(t *testing.T) {
	valid := filepath.Join(t.TempDir(), "valid.tar.gz")
	writeArchive(t, valid, map[string]string{"db/000001.sst": "database"})
	if err := validatePChainArchive(valid); err != nil {
		t.Fatal(err)
	}

	extra := filepath.Join(t.TempDir(), "extra.tar.gz")
	writeArchive(t, extra, map[string]string{"db/000001.sst": "database", "logs/node.log": "log"})
	if err := validatePChainArchive(extra); err == nil || !strings.Contains(err.Error(), "only db/ is allowed") {
		t.Fatalf("unexpected extra-path validation error: %v", err)
	}

	empty := filepath.Join(t.TempDir(), "empty.tar.gz")
	writeArchive(t, empty, nil)
	if err := validatePChainArchive(empty); err == nil || !strings.Contains(err.Error(), "no files") {
		t.Fatalf("unexpected empty validation error: %v", err)
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

func writeFleetInputs(t *testing.T, root string) {
	t.Helper()
	keyPath := filepath.Join(root, "fleet-key")
	writeTestFile(t, keyPath, "key")
	writeTestFile(t, filepath.Join(root, ".env"), strings.Join([]string{
		"NETWORK=fuji",
		"PCHAIN_API=https://api.avax-test.network",
		"FUNDING_PRIVATE_KEY=",
		"SSH_USER=ubuntu",
		"SSH_KEY_PATH=" + keyPath,
	}, "\n"))
	writeTestFile(t, filepath.Join(root, "nodes.ini"), strings.Join([]string{
		"1 host=v1 role=validator",
		"2 host=v2 role=validator",
		"3 host=v3 role=validator",
		"4 host=v4 role=validator",
		"5 host=rpc role=rpc",
		"6 host=pchain role=pchain",
	}, "\n"))
}

func writeArchive(t *testing.T, path string, files map[string]string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "db/", Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		if err := archive.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o600,
			Size: int64(len(contents)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
