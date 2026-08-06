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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanchego/ids"
)

type recordingRunner struct {
	mutex     sync.Mutex
	output    []byte
	runs      [][]string
	runErrors map[int]error
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
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
	err := deployer.Deploy(context.Background(), frozenMode, nil, false)
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
	environment := config.FleetEnvironment{Network: "fuji", SSHUser: "ubuntu", SystemInstall: true}
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
	// This list is the L1 address book (node.go hands it to ManuallyTrack), not
	// just a beacon override: without it a node knows only the P-chain node.
	if validatorConfig["state-sync-ips"] != "sibling:9651" || validatorConfig["state-sync-ids"] != "NodeID-sibling" {
		t.Fatalf("validator does not use L1 sibling for state sync: %v", validatorConfig)
	}
	if validatorConfig["staking-signer-key-file"] == nil {
		t.Fatal("validator signer path missing")
	}
}

func TestFrozenDeployPreservesExistingPChainDatabase(t *testing.T) {
	runner := &recordingRunner{output: []byte("present")}
	var output bytes.Buffer
	deployer := &Deployer{root: t.TempDir(), out: &output, runner: runner}
	deployment := deployment{environment: config.FleetEnvironment{SSHUser: "ubuntu", SystemInstall: true}}
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
			SSHUser:       "ubuntu",
			SSHKeyPath:    "/key",
			SystemInstall: true,
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
	state := deployment{environment: config.FleetEnvironment{SSHUser: "ubuntu", SystemInstall: true}}
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
		environment: config.FleetEnvironment{SSHUser: "ubuntu", SystemInstall: true},
		chainIDs:    map[string]ids.ID{"main": ids.GenerateTestID()},
		subnetIDs:   map[string]ids.ID{"main": ids.GenerateTestID()},
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
		environment: config.FleetEnvironment{SSHUser: "ubuntu", SystemInstall: true},
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

// allUp is the address book of a fully running fleet, which is what the pairing
// test is about; the liveness filter has its own test.
func allUp(inv inventory) map[int]bool {
	up := make(map[int]bool, len(inv.nodes))
	for _, node := range inv.l1Nodes() {
		up[node.Number] = true
	}
	return up
}

func TestStateSyncPeersExcludePChainAndSelf(t *testing.T) {
	nodes := []config.Node{
		{Number: 1, Host: "validator-a", Role: config.RoleValidator},
		{Number: 2, Host: "validator-b", Role: config.RoleValidator},
		{Number: 3, Host: "rpc", Role: config.RoleRPC},
		{Number: 4, Host: "pchain", Role: config.RolePChain},
	}
	assigned := map[int]creation.PublicNode{
		1: {NodeID: "NodeID-a"},
		2: {NodeID: "NodeID-b"},
		3: {NodeID: "NodeID-rpc"},
		4: {NodeID: "NodeID-pchain"},
	}
	up := map[int]bool{1: true, 2: true, 3: true, 4: true}
	ips, nodeIDs := stateSyncPeers(nodes[0], nodes, assigned, portsByNode(nodes), up)
	if ips != "validator-b:9651,rpc:9651" {
		t.Fatalf("state sync IPs = %q", ips)
	}
	if nodeIDs != "NodeID-b,NodeID-rpc" {
		t.Fatalf("state sync IDs = %q", nodeIDs)
	}
}

// A key swap must land in EVERY node's address book, resolved through
// placement. Rendering peers from public.json (the original keygen mapping)
// leaves each entry naming the identity that machine used to run: the dial
// fails TLS verification, the peer is unreachable, and a wiped node cannot
// reach the connected-stake gate or find a state-sync summary.
func TestRenderConfigsPairsPeersFromPlacementNotKeygen(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"node-config.json", "chain-config.json", "chain-config-rpc.json", "subnet-config.json"} {
		writeTestFile(t, filepath.Join(root, name), "{}")
	}

	inv := placementTestInventory(t)
	inv.environment = config.FleetEnvironment{Network: "fuji", SSHUser: "ubuntu", SystemInstall: true}
	inv.ports = portsByNode(inv.nodes)
	inv.pchain = inv.nodes[5]
	swapped, _, err := planPlace(inv, "a", 3)
	if err != nil {
		t.Fatal(err)
	}
	inv.placement = swapped

	deployer := &Deployer{root: root, out: io.Discard}
	target, err := inv.target(inv.nodes[1])
	if err != nil {
		t.Fatal(err)
	}
	rendered, cleanup, err := deployer.renderConfigs(inv, []nodeDeployment{target}, allUp(inv))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	config := readTestJSON(t, filepath.Join(rendered[0].renderDir, "node.json"))
	addresses := strings.Split(config["state-sync-ips"].(string), ",")
	nodeIDs := strings.Split(config["state-sync-ids"].(string), ",")
	if len(addresses) != len(nodeIDs) {
		t.Fatalf("address book lists disagree: %v vs %v", addresses, nodeIDs)
	}
	book := make(map[string]string, len(addresses))
	for i := range addresses {
		book[strings.Split(addresses[i], ":")[0]] = nodeIDs[i]
	}
	// a and c swapped machines, so v1 must now name c and v3 must name a.
	if book["v1"] != "NodeID-C" || book["v3"] != "NodeID-A" {
		t.Fatalf("address book follows the keygen mapping, not placement: %v", book)
	}
	if config["bootstrap-ids"] != "NodeID-F" {
		t.Fatalf("bootstrap = %v, want the pinned pchain identity NodeID-F", config["bootstrap-ids"])
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
		"SYSTEM_INSTALL=true",
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

func TestRenderOracleAndArchiveRoles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "node-config.json"), `{}`)
	writeTestFile(t, filepath.Join(root, "chain-config.json"), `{"main":true}`)
	writeTestFile(t, filepath.Join(root, "chain-config-rpc.json"), `{"rpc":true}`)
	writeTestFile(t, filepath.Join(root, "chain-config-archive.json"), `{"archive":true}`)
	writeTestFile(t, filepath.Join(root, "subnet-config.json"), `{"l1":"main"}`)
	writeTestFile(t, filepath.Join(root, "subnet-config-oracle.json"), `{"l1":"oracle"}`)
	environment := config.FleetEnvironment{Network: "fuji", SSHUser: "ubuntu", SystemInstall: true}
	oracleChainID := ids.GenerateTestID()
	oracleSubnetID := ids.GenerateTestID()

	cases := map[string]struct {
		node        config.Node
		signer      bool
		chainConfig string
		subnetCfg   string
	}{
		"oracle validator": {
			node:        config.Node{Number: 8, Role: config.RoleOracleValidator},
			signer:      true,
			chainConfig: `{"main":true}`,
			subnetCfg:   `{"l1":"oracle"}`,
		},
		"oracle rpc": {
			node:        config.Node{Number: 9, Role: config.RoleOracleRPC},
			chainConfig: `{"rpc":true}`,
			subnetCfg:   `{"l1":"oracle"}`,
		},
		"archive": {
			node:        config.Node{Number: 6, Role: config.RoleArchive},
			chainConfig: `{"archive":true}`,
			subnetCfg:   `{"l1":"main"}`,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			renderDir := filepath.Join(root, strconv.Itoa(testCase.node.Number))
			if err := renderNode(
				renderDir,
				root,
				environment,
				testCase.node,
				creation.PublicNode{Identity: "x", Role: testCase.node.Role},
				oracleChainID,
				oracleSubnetID,
				[2]int{9650, 9651},
				followMode,
				"pchain:9651",
				"NodeID-pchain",
				"",
				"",
			); err != nil {
				t.Fatal(err)
			}
			rendered := readTestJSON(t, filepath.Join(renderDir, "node.json"))
			if rendered["track-subnets"] != oracleSubnetID.String() {
				t.Fatalf("node must track the subnet it was rendered for: %v", rendered["track-subnets"])
			}
			if testCase.signer != (rendered["staking-signer-key-file"] != nil) {
				t.Fatalf("signer key presence = %v, want %v", rendered["staking-signer-key-file"] != nil, testCase.signer)
			}
			chain, err := os.ReadFile(filepath.Join(renderDir, "chain.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(chain) != testCase.chainConfig {
				t.Fatalf("chain.json = %s, want %s", chain, testCase.chainConfig)
			}
			subnet, err := os.ReadFile(filepath.Join(renderDir, "subnet.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(subnet) != testCase.subnetCfg {
				t.Fatalf("subnet.json = %s, want %s", subnet, testCase.subnetCfg)
			}
		})
	}
}

// REMOTE_DIR switches renderNode to a user-level install: the node is a plain
// process started by run.sh, so no systemd unit and no sudo anywhere.
func TestRenderNodeUserLayoutWritesRunScriptInsteadOfUnit(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "node-config.json"), `{}`)
	writeTestFile(t, filepath.Join(root, "chain-config.json"), `{}`)
	writeTestFile(t, filepath.Join(root, "subnet-config.json"), `{}`)
	environment := config.FleetEnvironment{Network: "fuji", SSHUser: "op", RemoteDir: "/home/op/bench"}
	renderDir := filepath.Join(root, "render")
	if err := renderNode(
		renderDir,
		root,
		environment,
		config.Node{Number: 3, Role: config.RoleValidator},
		creation.PublicNode{Identity: "c", Role: config.RoleValidator},
		ids.GenerateTestID(),
		ids.GenerateTestID(),
		[2]int{9650, 9651},
		followMode,
		"pchain:9651",
		"NodeID-pchain",
		"",
		"",
	); err != nil {
		t.Fatal(err)
	}
	run, err := os.ReadFile(filepath.Join(renderDir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(run, []byte("/home/op/bench/pkg")) {
		t.Fatalf("run.sh does not start the user-level binary: %s", run)
	}
	if bytes.Contains(run, []byte("sudo")) {
		t.Fatalf("user-level run.sh must not use sudo: %s", run)
	}
	if _, err := os.Stat(filepath.Join(renderDir, "node.service")); !os.IsNotExist(err) {
		t.Fatalf("user-level render must not produce a systemd unit: stat = %v", err)
	}
}

// The layout is the single seam between the two install modes: system commands
// keep their sudo systemctl shape, user commands never reach for either.
func TestLayoutSeparatesUserAndSystemInstalls(t *testing.T) {
	node := nodeDeployment{node: config.Node{Number: 4}}

	user := layoutFor(config.FleetEnvironment{RemoteDir: "/home/op/bench", RemoteDataDir: "/nvme/data"})
	if user.pkg != "/home/op/bench/pkg" || user.cfg != "/home/op/bench/config" || user.data != "/nvme/data" {
		t.Fatalf("user layout paths = %+v", user)
	}

	// The user install is the DEFAULT: an empty REMOTE_DIR roots it in the
	// ssh user's home, and nothing selects the system install implicitly.
	fallback := layoutFor(config.FleetEnvironment{SSHUser: "op"})
	if !fallback.user || fallback.pkg != "/home/op/avalanche-benchmark/pkg" ||
		fallback.cfg != "/home/op/avalanche-benchmark/config" ||
		fallback.data != "/home/op/avalanche-benchmark/data" {
		t.Fatalf("default layout = %+v, want a user install under /home/op/avalanche-benchmark", fallback)
	}
	for name, command := range map[string]string{
		"start":        user.startCommand(node),
		"stop":         user.stopCommand(node),
		"install unit": user.installUnitCommand("/tmp/stage", node),
	} {
		if strings.Contains(command, "sudo") || strings.Contains(command, "systemctl") {
			t.Fatalf("user-level %s command reaches for root: %q", name, command)
		}
	}

	system := layoutFor(config.FleetEnvironment{SystemInstall: true})
	if system.pkg != remotePackageDir || system.cfg != remoteConfigDir || system.data != remoteDataDir {
		t.Fatalf("system layout paths = %+v", system)
	}
	for name, command := range map[string]string{
		"start":        system.startCommand(node),
		"stop":         system.stopCommand(node),
		"install unit": system.installUnitCommand("/tmp/stage", node),
	} {
		if !strings.Contains(command, "sudo systemctl") {
			t.Fatalf("system %s command lost its sudo systemctl shape: %q", name, command)
		}
	}
}

// Two kits on one machine share node numbers, so the pattern-based process
// fallback must be anchored to THIS install's directories. An unanchored
// config/<n>/node.json pattern once let a second kit's deploy stop the first
// kit's running P-chain node (reported 2026-08-05).
func TestProcessPatternsAreScopedToTheInstall(t *testing.T) {
	kitA := layoutFor(config.FleetEnvironment{RemoteDir: "/home/op/kit-a"})
	kitB := layoutFor(config.FleetEnvironment{RemoteDir: "/home/op/kit-b"})

	nodeArgv := "/home/op/kit-a/pkg/13/bin/avalanchego --config-file=/home/op/kit-a/config/13/node.json"
	if !regexp.MustCompile(kitA.processPattern(13)).MatchString(nodeArgv) {
		t.Fatalf("pattern %q does not match its own node", kitA.processPattern(13))
	}
	if regexp.MustCompile(kitB.processPattern(13)).MatchString(nodeArgv) {
		t.Fatalf("pattern %q matches a DIFFERENT install's node", kitB.processPattern(13))
	}

	pluginArgv := "/home/op/kit-a/pkg/13/plugins/" + pluginID
	if !regexp.MustCompile(kitA.pluginPattern(13)).MatchString(pluginArgv) {
		t.Fatalf("pattern %q does not match its own plugin", kitA.pluginPattern(13))
	}
	if regexp.MustCompile(kitB.pluginPattern(13)).MatchString(pluginArgv) {
		t.Fatalf("pattern %q matches a DIFFERENT install's plugin", kitB.pluginPattern(13))
	}

	// The stop and kill commands carry the pattern in their own argv; the
	// bracket must keep them from matching themselves.
	target := nodeDeployment{node: config.Node{Number: 13}}
	for name, command := range map[string]string{
		"stop": kitA.stopCommand(target),
		"kill": kitA.killCommand(target),
	} {
		if regexp.MustCompile(kitA.processPattern(13)).MatchString(command) {
			t.Fatalf("process pattern matches the %s command that carries it: %q", name, command)
		}
		if regexp.MustCompile(kitA.pluginPattern(13)).MatchString(command) {
			t.Fatalf("plugin pattern matches the %s command that carries it: %q", name, command)
		}
	}

	// A regex metacharacter in the install path must match only itself.
	dated := layoutFor(config.FleetEnvironment{RemoteDir: "/home/op/kit-2026.08"})
	if regexp.MustCompile(dated.processPattern(13)).MatchString("/home/op/kit-2026x08/config/13/node.json") {
		t.Fatal("unescaped dot in the install path matches unrelated paths")
	}

	// The system layout anchors to its own constants the same way.
	system := layoutFor(config.FleetEnvironment{SystemInstall: true})
	if !strings.Contains(system.processPattern(13), remoteConfigDir) {
		t.Fatalf("system process pattern %q is not anchored to %s", system.processPattern(13), remoteConfigDir)
	}
}

func TestStateSyncPeersStayWithinTheirL1(t *testing.T) {
	nodes := []config.Node{
		{Number: 1, Host: "v1", Role: config.RoleValidator},
		{Number: 2, Host: "r1", Role: config.RoleRPC},
		{Number: 3, Host: "p1", Role: config.RolePChain},
		{Number: 4, Host: "o1", Role: config.RoleOracleValidator},
		{Number: 5, Host: "o2", Role: config.RoleOracleRPC},
	}
	public := map[int]creation.PublicNode{
		1: {NodeID: "NodeID-v1"}, 2: {NodeID: "NodeID-r1"},
		4: {NodeID: "NodeID-o1"}, 5: {NodeID: "NodeID-o2"},
	}
	ports := portsByNode(nodes)

	up := map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true}
	ips, ids := stateSyncPeers(nodes[0], nodes, public, ports, up)
	if ips != "r1:9651" || ids != "NodeID-r1" {
		t.Fatalf("main validator peers = %q %q, want only the main rpc", ips, ids)
	}
	ips, ids = stateSyncPeers(nodes[3], nodes, public, ports, up)
	if ips != "o2:9651" || ids != "NodeID-o2" {
		t.Fatalf("oracle validator peers = %q %q, want only the oracle rpc", ips, ids)
	}
}

// The chain= partition generalizes the oracle special case: peers pair only
// within their own chain, whatever the chain is called.
func TestStateSyncPeersPartitionByDeclaredChain(t *testing.T) {
	nodes := []config.Node{
		{Number: 1, Host: "v1", Role: config.RoleValidator, Chain: "main"},
		{Number: 2, Host: "r1", Role: config.RoleRPC, Chain: "main"},
		{Number: 3, Host: "p1", Role: config.RolePChain},
		{Number: 4, Host: "t1", Role: config.RoleValidator, Chain: "trading"},
		{Number: 5, Host: "t2", Role: config.RoleRPC, Chain: "trading"},
	}
	assigned := map[int]creation.PublicNode{
		1: {NodeID: "NodeID-v1"}, 2: {NodeID: "NodeID-r1"},
		4: {NodeID: "NodeID-t1"}, 5: {NodeID: "NodeID-t2"},
	}
	ports := portsByNode(nodes)
	up := map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true}

	ips, ids := stateSyncPeers(nodes[0], nodes, assigned, ports, up)
	if ips != "r1:9651" || ids != "NodeID-r1" {
		t.Fatalf("main validator peers = %q %q, want only the main rpc", ips, ids)
	}
	ips, ids = stateSyncPeers(nodes[3], nodes, assigned, ports, up)
	if ips != "t2:9651" || ids != "NodeID-t2" {
		t.Fatalf("trading validator peers = %q %q, want only the trading rpc", ips, ids)
	}
}

// A chain can carry its own subnet configuration under chains/<name>/; every
// chain without one shares the root default.
func TestRenderNodeResolvesPerChainSubnetConfig(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "node-config.json"), `{}`)
	writeTestFile(t, filepath.Join(root, "chain-config.json"), `{}`)
	writeTestFile(t, filepath.Join(root, "subnet-config.json"), `{"l1":"default"}`)
	if err := os.MkdirAll(filepath.Join(root, "chains", "trading"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "chains", "trading", "subnet-config.json"), `{"l1":"trading"}`)
	environment := config.FleetEnvironment{Network: "fuji", SSHUser: "op"}

	for name, testCase := range map[string]struct {
		node config.Node
		want string
	}{
		"main falls back to the root": {config.Node{Number: 1, Role: config.RoleValidator, Chain: "main"}, `{"l1":"default"}`},
		"trading uses its override":   {config.Node{Number: 7, Role: config.RoleValidator, Chain: "trading"}, `{"l1":"trading"}`},
	} {
		t.Run(name, func(t *testing.T) {
			renderDir := filepath.Join(t.TempDir(), "render")
			if err := renderNode(
				renderDir, root, environment, testCase.node,
				creation.PublicNode{Identity: "a", Role: testCase.node.Role},
				ids.GenerateTestID(), ids.GenerateTestID(), [2]int{9650, 9651},
				followMode, "pchain:9651", "NodeID-pchain", "", "",
			); err != nil {
				t.Fatal(err)
			}
			subnet, err := os.ReadFile(filepath.Join(renderDir, "subnet.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(subnet) != testCase.want {
				t.Fatalf("subnet.json = %s, want %s", subnet, testCase.want)
			}
		})
	}
}

// A chain can carry its own EVM chain configuration variants under
// chains/<name>/; a chain without one shares the root variant.
func TestRenderNodeResolvesPerChainChainConfig(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "node-config.json"), `{}`)
	writeTestFile(t, filepath.Join(root, "chain-config.json"), `{"variant":"root"}`)
	writeTestFile(t, filepath.Join(root, "chain-config-rpc.json"), `{"variant":"root-rpc"}`)
	writeTestFile(t, filepath.Join(root, "subnet-config.json"), `{}`)
	if err := os.MkdirAll(filepath.Join(root, "chains", "trading"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "chains", "trading", "chain-config-rpc.json"), `{"variant":"trading-rpc"}`)
	environment := config.FleetEnvironment{Network: "fuji", SSHUser: "op"}

	for name, testCase := range map[string]struct {
		node config.Node
		want string
	}{
		"trading rpc uses its override":        {config.Node{Number: 7, Role: config.RoleRPC, Chain: "trading"}, `{"variant":"trading-rpc"}`},
		"main rpc falls back to the root":      {config.Node{Number: 5, Role: config.RoleRPC, Chain: "main"}, `{"variant":"root-rpc"}`},
		"trading validator falls back to root": {config.Node{Number: 8, Role: config.RoleValidator, Chain: "trading"}, `{"variant":"root"}`},
	} {
		t.Run(name, func(t *testing.T) {
			renderDir := filepath.Join(t.TempDir(), "render")
			if err := renderNode(
				renderDir, root, environment, testCase.node,
				creation.PublicNode{Identity: "a", Role: testCase.node.Role},
				ids.GenerateTestID(), ids.GenerateTestID(), [2]int{9650, 9651},
				followMode, "pchain:9651", "NodeID-pchain", "", "",
			); err != nil {
				t.Fatal(err)
			}
			chain, err := os.ReadFile(filepath.Join(renderDir, "chain.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(chain) != testCase.want {
				t.Fatalf("chain.json = %s, want %s", chain, testCase.want)
			}
		})
	}
}

// The address book must name only machines meant to be up. It doubles as the
// state-sync beacon set with alpha = count/2 + 1 over the LIST, so listing a
// machine that is down raises the bar without adding anyone who can clear it:
// twelve entries with six down leaves five reachable against an alpha of six and
// nobody can state-sync.
func TestStateSyncPeersListOnlyMachinesMeantToBeUp(t *testing.T) {
	nodes := []config.Node{
		{Number: 1, Host: "v1", Role: config.RoleValidator},
		{Number: 2, Host: "v2", Role: config.RoleValidator},
		{Number: 3, Host: "v3", Role: config.RoleValidator},
		{Number: 4, Host: "pchain", Role: config.RolePChain},
	}
	assigned := map[int]creation.PublicNode{
		1: {NodeID: "NodeID-a"}, 2: {NodeID: "NodeID-b"},
		3: {NodeID: "NodeID-c"}, 4: {NodeID: "NodeID-pchain"},
	}
	ports := portsByNode(nodes)

	ips, nodeIDs := stateSyncPeers(nodes[0], nodes, assigned, ports, map[int]bool{1: true, 2: true, 3: false})
	if ips != "v2:9651" || nodeIDs != "NodeID-b" {
		t.Fatalf("down peer is still listed: ips=%q ids=%q", ips, nodeIDs)
	}

	// Every peer down leaves an empty override, which falls back to the
	// stake-weighted validator set rather than pinning an unusable list.
	ips, nodeIDs = stateSyncPeers(nodes[0], nodes, assigned, ports, map[int]bool{1: true})
	if ips != "" || nodeIDs != "" {
		t.Fatalf("expected an empty address book, got ips=%q ids=%q", ips, nodeIDs)
	}
}
