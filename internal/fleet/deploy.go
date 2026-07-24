package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/joho/godotenv"
)

const (
	pluginID         = "srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"
	servicePrefix    = "avalanche-benchmark-node-"
	remotePackageDir = "/opt/avalanche-benchmark"
	remoteConfigDir  = "/etc/avalanche-benchmark"
	remoteDataDir    = "/var/lib/avalanche-benchmark"
	pchainArchive    = "pchain.tar.gz"
	frozenMode       = "frozen"
	followMode       = "follow"
)

var sshUserPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

type commandRunner interface {
	Run(context.Context, string, ...string) error
	Output(context.Context, string, ...string) ([]byte, error)
}

type osCommandRunner struct {
	stdout io.Writer
	stderr io.Writer
}

func (r osCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = r.stdout
	command.Stderr = r.stderr
	return command.Run()
}

func (r osCommandRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Stderr = r.stderr
	return command.Output()
}

type Deployer struct {
	root      string
	out       io.Writer
	runner    commandRunner
	http      *http.Client
	waitLimit time.Duration
}

func NewDeployer(root string, out io.Writer) *Deployer {
	return &Deployer{
		root:      root,
		out:       out,
		runner:    osCommandRunner{stdout: out, stderr: os.Stderr},
		http:      &http.Client{Timeout: 5 * time.Second},
		waitLimit: 10 * time.Minute,
	}
}

type deployment struct {
	environment     config.FleetEnvironment
	beacon          nodeDeployment
	selected        []nodeDeployment
	chainID         ids.ID
	subnetID        ids.ID
	managerSubnetID ids.ID
	expectedMain    map[ids.NodeID]struct{}
	expectedManager map[ids.NodeID]struct{}
	beaconMode      string
}

type nodeDeployment struct {
	node      config.Node
	identity  creation.PublicNode
	httpPort  int
	renderDir string
}

func (d *Deployer) Deploy(ctx context.Context, beaconMode string) error {
	prepared, cleanup, err := d.prepare(beaconMode)
	if err != nil {
		return err
	}
	defer cleanup()

	// The P-chain beacon is brought fully current before any L1 node is
	// touched. This is a phase barrier, not a best-effort bootstrap hint.
	beaconOnly := prepared
	beaconOnly.selected = []nodeDeployment{prepared.beacon}
	for _, phase := range []struct {
		name   string
		action func(context.Context, deployment, nodeDeployment) error
	}{
		{"beacon stop", d.stop},
		{"beacon package", d.installPackage},
		{"beacon systemd", d.installUnit},
		{"beacon identity", d.installIdentity},
	} {
		if err := d.phase(ctx, beaconOnly, phase.name, phase.action); err != nil {
			return err
		}
	}
	if prepared.beaconMode == frozenMode {
		if err := d.phase(ctx, beaconOnly, "beacon P-chain seed", d.seedBeacon); err != nil {
			return err
		}
	}
	if err := d.phase(ctx, beaconOnly, "beacon start", d.start); err != nil {
		return err
	}
	if err := d.waitBeaconReady(ctx, prepared); err != nil {
		return fmt.Errorf("beacon readiness phase node %d (%s): %w", prepared.beacon.node.Number, prepared.beacon.node.Host, err)
	}

	for _, phase := range []struct {
		name   string
		action func(context.Context, deployment, nodeDeployment) error
	}{
		{"stop", d.stop},
		{"package", d.installPackage},
		{"systemd", d.installUnit},
		{"identity", d.installIdentity},
		{"start", d.start},
		{"readiness", d.waitL1Ready},
	} {
		if err := d.phase(ctx, prepared, phase.name, phase.action); err != nil {
			return err
		}
	}
	fmt.Fprintf(d.out, "deployed P-chain beacon and %d L1 node(s)\n", len(prepared.selected))
	return nil
}

func (d *Deployer) phase(
	ctx context.Context,
	deployment deployment,
	name string,
	action func(context.Context, deployment, nodeDeployment) error,
) error {
	if len(deployment.selected) == 0 {
		return nil
	}
	fmt.Fprintf(d.out, "%s phase\n", name)
	for _, node := range deployment.selected {
		if err := action(ctx, deployment, node); err != nil {
			return fmt.Errorf("%s phase node %d (%s): %w", name, node.node.Number, node.node.Host, err)
		}
	}
	return nil
}

func (d *Deployer) prepare(beaconMode string) (deployment, func(), error) {
	noCleanup := func() {}
	if beaconMode != frozenMode && beaconMode != followMode {
		return deployment{}, noCleanup, fmt.Errorf("deploy mode must be %q or %q, got %q", frozenMode, followMode, beaconMode)
	}
	if beaconMode == frozenMode {
		archivePath := filepath.Join(d.root, pchainArchive)
		if info, err := os.Stat(archivePath); err != nil {
			if os.IsNotExist(err) {
				return deployment{}, noCleanup, fmt.Errorf("frozen deploy requires ./%s; file not found", pchainArchive)
			}
			return deployment{}, noCleanup, fmt.Errorf("frozen deploy requires ./%s: %w", pchainArchive, err)
		} else if !info.Mode().IsRegular() {
			return deployment{}, noCleanup, fmt.Errorf("frozen deploy requires ./%s to be a regular file", pchainArchive)
		}
	}
	environment, err := config.LoadFleetEnvironment(filepath.Join(d.root, ".env"))
	if err != nil {
		return deployment{}, noCleanup, err
	}
	if !sshUserPattern.MatchString(environment.SSHUser) {
		return deployment{}, noCleanup, fmt.Errorf(".env: SSH_USER must be a Linux user name, got %q", environment.SSHUser)
	}
	nodes, err := config.LoadNodes(filepath.Join(d.root, "nodes.ini"))
	if err != nil {
		return deployment{}, noCleanup, err
	}
	for _, node := range nodes {
		if strings.ContainsAny(node.Host, " \t\r\n,") {
			return deployment{}, noCleanup, fmt.Errorf("node %d host must not contain whitespace or a comma, got %q", node.Number, node.Host)
		}
	}

	public, _, err := creation.LoadPublic(filepath.Join(d.root, "deployment", "public.json"))
	if err != nil {
		return deployment{}, noCleanup, err
	}
	publicByNode := make(map[int]creation.PublicNode, len(public.Nodes))
	for _, node := range public.Nodes {
		publicByNode[node.Node] = node
	}
	for _, node := range nodes {
		generated, exists := publicByNode[node.Number]
		if !exists {
			return deployment{}, noCleanup, fmt.Errorf("deployment/public.json has no identity for inventory node %d", node.Number)
		}
		if generated.Role != node.Role {
			return deployment{}, noCleanup, fmt.Errorf("node %d role differs: nodes.ini=%s public.json=%s", node.Number, node.Role, generated.Role)
		}
	}
	if len(publicByNode) != len(nodes) {
		return deployment{}, noCleanup, fmt.Errorf("deployment/public.json has %d nodes but nodes.ini has %d", len(publicByNode), len(nodes))
	}

	state, err := godotenv.Read(filepath.Join(d.root, "deployment", "network.env"))
	if err != nil {
		return deployment{}, noCleanup, fmt.Errorf("read required deployment state deployment/network.env: %w", err)
	}
	if state["NETWORK"] != environment.Network {
		return deployment{}, noCleanup, fmt.Errorf("deployment/network.env NETWORK=%q does not match .env NETWORK=%q", state["NETWORK"], environment.Network)
	}
	chainID, err := requiredID(state, "CHAIN_ID")
	if err != nil {
		return deployment{}, noCleanup, err
	}
	subnetID, err := requiredID(state, "SUBNET_ID")
	if err != nil {
		return deployment{}, noCleanup, err
	}
	managerSubnetID, err := requiredID(state, "MANAGER_SUBNET_ID")
	if err != nil {
		return deployment{}, noCleanup, err
	}

	requiredFiles := []string{
		filepath.Join(d.root, "bin", "avalanchego"),
		filepath.Join(d.root, "bin", pluginID),
		filepath.Join(d.root, "node-config.json"),
		filepath.Join(d.root, "chain-config.json"),
		filepath.Join(d.root, "chain-config-rpc.json"),
		filepath.Join(d.root, "subnet-config.json"),
	}
	for _, path := range requiredFiles {
		if info, err := os.Stat(path); err != nil {
			return deployment{}, noCleanup, fmt.Errorf("required deployment artifact %s is unavailable: %w", path, err)
		} else if info.IsDir() {
			return deployment{}, noCleanup, fmt.Errorf("required deployment artifact %s is a directory", path)
		}
	}
	for _, path := range requiredFiles[:2] {
		info, _ := os.Stat(path)
		if info.Mode()&0o111 == 0 {
			return deployment{}, noCleanup, fmt.Errorf("required deployment binary %s is not executable", path)
		}
	}
	if err := verifyConsensusConfig(filepath.Join(d.root, "subnet-config.json")); err != nil {
		return deployment{}, noCleanup, err
	}

	ports := portsByNode(nodes)
	renderRoot, err := os.MkdirTemp("", "fleet-deploy-")
	if err != nil {
		return deployment{}, noCleanup, fmt.Errorf("create deployment render directory: %w", err)
	}
	cleanup := func() { os.RemoveAll(renderRoot) }
	result := deployment{
		environment:     environment,
		chainID:         chainID,
		subnetID:        subnetID,
		managerSubnetID: managerSubnetID,
		expectedMain:    make(map[ids.NodeID]struct{}),
		expectedManager: make(map[ids.NodeID]struct{}),
		beaconMode:      beaconMode,
	}
	for _, node := range public.Nodes {
		if node.Role == config.RoleValidator {
			nodeID, _ := ids.NodeIDFromString(node.NodeID)
			result.expectedMain[nodeID] = struct{}{}
		}
	}
	for _, manager := range public.Managers {
		nodeID, _ := ids.NodeIDFromString(manager.NodeID)
		result.expectedManager[nodeID] = struct{}{}
	}

	var beacon config.Node
	for _, node := range nodes {
		if node.Role == config.RoleBeacon {
			beacon = node
			break
		}
	}
	beaconIdentity := publicByNode[beacon.Number]
	if err := validateIdentityFiles(d.root, beaconIdentity); err != nil {
		cleanup()
		return deployment{}, noCleanup, err
	}
	beaconRender := filepath.Join(renderRoot, strconv.Itoa(beacon.Number))
	if err := renderNode(beaconRender, d.root, environment, beacon, beaconIdentity, chainID, subnetID, ports[beacon.Number], beaconMode, "", "", "", ""); err != nil {
		cleanup()
		return deployment{}, noCleanup, err
	}
	result.beacon = nodeDeployment{
		node:      beacon,
		identity:  beaconIdentity,
		httpPort:  ports[beacon.Number][0],
		renderDir: beaconRender,
	}

	for _, node := range nodes {
		if node.Role == config.RoleBeacon {
			continue
		}
		generated := publicByNode[node.Number]
		if err := validateIdentityFiles(d.root, generated); err != nil {
			cleanup()
			return deployment{}, noCleanup, err
		}
		renderDir := filepath.Join(renderRoot, strconv.Itoa(node.Number))
		bootstrapIP := fmt.Sprintf("%s:%d", beacon.Host, ports[beacon.Number][1])
		stateSyncIPs, stateSyncIDs := stateSyncPeers(node, nodes, publicByNode, ports)
		if err := renderNode(
			renderDir,
			d.root,
			environment,
			node,
			generated,
			chainID,
			subnetID,
			ports[node.Number],
			beaconMode,
			bootstrapIP,
			beaconIdentity.NodeID,
			stateSyncIPs,
			stateSyncIDs,
		); err != nil {
			cleanup()
			return deployment{}, noCleanup, err
		}
		result.selected = append(result.selected, nodeDeployment{
			node:      node,
			identity:  generated,
			httpPort:  ports[node.Number][0],
			renderDir: renderDir,
		})
	}
	return result, cleanup, nil
}

func stateSyncPeers(
	node config.Node,
	nodes []config.Node,
	public map[int]creation.PublicNode,
	ports map[int][2]int,
) (string, string) {
	var peerIPs []string
	var peerIDs []string
	for _, peer := range nodes {
		if peer.Number == node.Number || peer.Role == config.RoleBeacon {
			continue
		}
		peerIPs = append(peerIPs, fmt.Sprintf("%s:%d", peer.Host, ports[peer.Number][1]))
		peerIDs = append(peerIDs, public[peer.Number].NodeID)
	}
	return strings.Join(peerIPs, ","), strings.Join(peerIDs, ",")
}

func requiredID(values map[string]string, field string) (ids.ID, error) {
	value := strings.TrimSpace(values[field])
	if value == "" {
		return ids.Empty, fmt.Errorf("deployment/network.env: required field %s is not provided; creation is incomplete", field)
	}
	id, err := ids.FromString(value)
	if err != nil {
		return ids.Empty, fmt.Errorf("deployment/network.env: invalid %s: %w", field, err)
	}
	return id, nil
}

func portsByNode(nodes []config.Node) map[int][2]int {
	occurrences := make(map[string]int)
	result := make(map[int][2]int, len(nodes))
	for _, node := range nodes {
		index := occurrences[node.Host]
		result[node.Number] = [2]int{9650 + 2*index, 9651 + 2*index}
		occurrences[node.Host]++
	}
	return result
}

func renderNode(
	renderDir, root string,
	environment config.FleetEnvironment,
	node config.Node,
	generated creation.PublicNode,
	chainID, subnetID ids.ID,
	ports [2]int,
	beaconMode string,
	bootstrapIP, bootstrapID string,
	stateSyncIPs, stateSyncIDs string,
) error {
	if err := os.Mkdir(renderDir, 0o700); err != nil {
		return fmt.Errorf("create node %d render directory: %w", node.Number, err)
	}
	baseConfig, err := os.ReadFile(filepath.Join(root, "node-config.json"))
	if err != nil {
		return err
	}
	cfg := make(map[string]any)
	if err := json.Unmarshal(baseConfig, &cfg); err != nil {
		return fmt.Errorf("decode node-config.json: %w", err)
	}
	nodeRoot := filepath.Join(remoteDataDir, strconv.Itoa(node.Number))
	nodePackage := filepath.Join(remotePackageDir, strconv.Itoa(node.Number))
	stakingDir := filepath.Join(remoteConfigDir, strconv.Itoa(node.Number), "staking")
	cfg["network-id"] = environment.Network
	cfg["data-dir"] = nodeRoot
	cfg["db-dir"] = filepath.Join(nodeRoot, "db")
	cfg["log-dir"] = filepath.Join(nodeRoot, "logs")
	cfg["http-host"] = "0.0.0.0"
	cfg["http-port"] = ports[0]
	cfg["staking-port"] = ports[1]
	cfg["public-ip"] = node.Host
	cfg["partial-sync-primary-network"] = true
	cfg["staking-tls-cert-file"] = filepath.Join(stakingDir, "staker.crt")
	cfg["staking-tls-key-file"] = filepath.Join(stakingDir, "staker.key")
	if node.Role == config.RoleBeacon {
		cfg["p-chain-follow-only"] = true
		cfg["staking-ephemeral-signer-enabled"] = true
		if beaconMode == frozenMode {
			cfg["bootstrap-ips"] = ""
			cfg["bootstrap-ids"] = ""
		} else {
			delete(cfg, "bootstrap-ips")
			delete(cfg, "bootstrap-ids")
		}
		delete(cfg, "state-sync-ips")
		delete(cfg, "state-sync-ids")
	} else {
		cfg["track-subnets"] = subnetID.String()
		cfg["plugin-dir"] = filepath.Join(nodePackage, "plugins")
		cfg["chain-config-dir"] = filepath.Join(remoteConfigDir, strconv.Itoa(node.Number), "chains")
		cfg["subnet-config-dir"] = filepath.Join(remoteConfigDir, strconv.Itoa(node.Number), "subnets")
		cfg["bootstrap-ips"] = bootstrapIP
		cfg["bootstrap-ids"] = bootstrapID
		cfg["state-sync-ips"] = stateSyncIPs
		cfg["state-sync-ids"] = stateSyncIDs
		if node.Role == config.RoleValidator {
			cfg["staking-signer-key-file"] = filepath.Join(stakingDir, "signer.key")
		} else {
			cfg["staking-ephemeral-signer-enabled"] = true
		}
	}
	if err := writeJSON(filepath.Join(renderDir, "node.json"), cfg); err != nil {
		return err
	}
	if node.Role != config.RoleBeacon {
		chainConfig := "chain-config.json"
		if node.Role == config.RoleRPC {
			chainConfig = "chain-config-rpc.json"
		}
		for _, copyPair := range [][2]string{
			{filepath.Join(root, chainConfig), filepath.Join(renderDir, "chain.json")},
			{filepath.Join(root, "subnet-config.json"), filepath.Join(renderDir, "subnet.json")},
		} {
			contents, err := os.ReadFile(copyPair[0])
			if err != nil {
				return err
			}
			if err := os.WriteFile(copyPair[1], contents, 0o600); err != nil {
				return err
			}
		}
	}
	unit := fmt.Sprintf(`[Unit]
Description=Avalanche benchmark node %d (%s, %s)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
ExecStart=%s/bin/avalanchego --config-file=%s/%d/node.json
Restart=on-failure
RestartSec=2
LimitNOFILE=1048576
KillMode=control-group

[Install]
WantedBy=multi-user.target
`, node.Number, generated.Identity, node.Role, environment.SSHUser, nodePackage, remoteConfigDir, node.Number)
	return os.WriteFile(filepath.Join(renderDir, "node.service"), []byte(unit), 0o600)
}

func writeJSON(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(contents, '\n'), 0o600)
}

func verifyConsensusConfig(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg struct {
		Snow struct {
			K               int `json:"k"`
			AlphaPreference int `json:"alphaPreference"`
			AlphaConfidence int `json:"alphaConfidence"`
			Beta            int `json:"beta"`
		} `json:"snowParameters"`
		ProposerWindow                int  `json:"proposerWindowMilliseconds"`
		ProposerMillisecondTimestamps bool `json:"proposerMillisecondTimestamps"`
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return fmt.Errorf("decode immutable consensus config %s: %w", path, err)
	}
	if cfg.Snow.K != 30 || cfg.Snow.AlphaPreference != 16 || cfg.Snow.AlphaConfidence != 17 || cfg.Snow.Beta != 12 || cfg.ProposerWindow != 100 {
		return fmt.Errorf("%s does not contain the verified consensus parameters k=30 alphaPreference=16 alphaConfidence=17 beta=12 proposerWindowMilliseconds=100", path)
	}
	return nil
}

func validateIdentityFiles(root string, generated creation.PublicNode) error {
	dir := filepath.Join(root, "deployment", "identities", generated.Identity)
	names := []string{"staker.crt", "staker.key"}
	if generated.Role == config.RoleValidator {
		names = append(names, "signer.key")
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err != nil {
			return fmt.Errorf("required identity file %s is unavailable: %w", path, err)
		} else if info.IsDir() {
			return fmt.Errorf("required identity file %s is a directory", path)
		}
	}
	actualNodeID, err := identity.LoadNodeID(filepath.Join(dir, "staker.crt"))
	if err != nil {
		return err
	}
	if actualNodeID.String() != generated.NodeID {
		return fmt.Errorf("identity %s certificate is %s but public.json records %s", generated.Identity, actualNodeID, generated.NodeID)
	}
	return nil
}

func (d *Deployer) sshArgs(deployment deployment, node nodeDeployment, remoteCommand string) []string {
	args := sshOptions(deployment.environment.SSHKeyPath)
	args = append(args, fmt.Sprintf("%s@%s", deployment.environment.SSHUser, node.node.Host), remoteCommand)
	return args
}

func sshOptions(keyPath string) []string {
	return []string{
		"-i", keyPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
	}
}

func (d *Deployer) runSSH(ctx context.Context, deployment deployment, node nodeDeployment, command string) error {
	if err := d.runner.Run(ctx, "ssh", d.sshArgs(deployment, node, command)...); err != nil {
		return fmt.Errorf("ssh command failed: %w", err)
	}
	return nil
}

func (d *Deployer) runSSHOutput(ctx context.Context, deployment deployment, node nodeDeployment, command string) ([]byte, error) {
	output, err := d.runner.Output(ctx, "ssh", d.sshArgs(deployment, node, command)...)
	if err != nil {
		return nil, fmt.Errorf("ssh command failed: %w", err)
	}
	return output, nil
}

func (d *Deployer) rsync(ctx context.Context, deployment deployment, node nodeDeployment, local, remote string) error {
	sshCommand := append([]string{"ssh"}, sshOptions(deployment.environment.SSHKeyPath)...)
	args := []string{
		"-rt", "--delete",
		"-e", strings.Join(sshCommand, " "),
		local + "/",
		fmt.Sprintf("%s@%s:%s/", deployment.environment.SSHUser, node.node.Host, remote),
	}
	if err := d.runner.Run(ctx, "rsync", args...); err != nil {
		return fmt.Errorf("rsync %s: %w", local, err)
	}
	return nil
}

func (d *Deployer) rsyncFile(ctx context.Context, deployment deployment, node nodeDeployment, local, remote string) error {
	sshCommand := append([]string{"ssh"}, sshOptions(deployment.environment.SSHKeyPath)...)
	args := []string{
		"-rt",
		"-e", strings.Join(sshCommand, " "),
		local,
		fmt.Sprintf("%s@%s:%s/", deployment.environment.SSHUser, node.node.Host, remote),
	}
	if err := d.runner.Run(ctx, "rsync", args...); err != nil {
		return fmt.Errorf("rsync %s: %w", local, err)
	}
	return nil
}

func serviceName(node nodeDeployment) string {
	return servicePrefix + strconv.Itoa(node.node.Number) + ".service"
}

func stagingDir(node nodeDeployment, suffix string) string {
	return fmt.Sprintf("/tmp/avalanche-benchmark-%d-%s", node.node.Number, suffix)
}

func (d *Deployer) stop(ctx context.Context, deployment deployment, node nodeDeployment) error {
	unit := serviceName(node)
	command := fmt.Sprintf(
		"if sudo systemctl cat %s >/dev/null 2>&1; then sudo systemctl stop %s && test \"$(sudo systemctl is-active %s)\" = inactive; fi",
		unit, unit, unit)
	return d.runSSH(ctx, deployment, node, command)
}

func (d *Deployer) installPackage(ctx context.Context, deployment deployment, node nodeDeployment) error {
	packageStage := stagingDir(node, "package")
	binaryStage := stagingDir(node, "binary")
	command := fmt.Sprintf("rm -rf %s %s && mkdir -m 700 %s %s", packageStage, binaryStage, packageStage, binaryStage)
	if err := d.runSSH(ctx, deployment, node, command); err != nil {
		return err
	}
	if err := d.rsync(ctx, deployment, node, node.renderDir, packageStage); err != nil {
		return err
	}
	if err := d.rsyncFile(ctx, deployment, node, filepath.Join(d.root, "bin", "avalanchego"), binaryStage); err != nil {
		return err
	}
	if err := d.rsyncFile(ctx, deployment, node, filepath.Join(d.root, "bin", pluginID), binaryStage); err != nil {
		return err
	}
	install := fmt.Sprintf(
		"sudo install -d -m 0755 %[1]s/%[3]d/bin %[1]s/%[3]d/plugins %[2]s/%[3]d %[4]s/%[3]d/db %[4]s/%[3]d/logs && "+
			"sudo install -m 0755 %[5]s/avalanchego %[1]s/%[3]d/bin/avalanchego && "+
			"sudo install -m 0755 %[5]s/%[6]s %[1]s/%[3]d/plugins/%[6]s && "+
			"sudo install -m 0644 %[7]s/node.json %[2]s/%[3]d/node.json && "+
			"sudo chown -R %[8]s:%[8]s %[4]s/%[3]d %[2]s/%[3]d",
		remotePackageDir, remoteConfigDir, node.node.Number, remoteDataDir,
		binaryStage, pluginID, packageStage, deployment.environment.SSHUser)
	if node.node.Role != config.RoleBeacon {
		install += fmt.Sprintf(
			" && sudo install -d -m 0755 %[1]s/%[2]d/chains/%[3]s %[1]s/%[2]d/subnets && "+
				"sudo install -m 0644 %[4]s/chain.json %[1]s/%[2]d/chains/%[3]s/config.json && "+
				"sudo install -m 0644 %[4]s/subnet.json %[1]s/%[2]d/subnets/%[5]s.json",
			remoteConfigDir, node.node.Number, deployment.chainID, packageStage, deployment.subnetID)
	}
	return d.runSSH(ctx, deployment, node, install)
}

func (d *Deployer) installUnit(ctx context.Context, deployment deployment, node nodeDeployment) error {
	stage := stagingDir(node, "package")
	unit := serviceName(node)
	command := fmt.Sprintf(
		"sudo install -m 0644 %s/node.service /etc/systemd/system/%s && sudo systemctl daemon-reload && sudo systemctl enable %s",
		stage, unit, unit)
	return d.runSSH(ctx, deployment, node, command)
}

func (d *Deployer) installIdentity(ctx context.Context, deployment deployment, node nodeDeployment) error {
	stage := stagingDir(node, "identity")
	if err := d.runSSH(ctx, deployment, node, "rm -rf "+stage+" && mkdir -m 700 "+stage); err != nil {
		return err
	}
	local := filepath.Join(d.root, "deployment", "identities", node.identity.Identity)
	if err := d.rsync(ctx, deployment, node, local, stage); err != nil {
		return err
	}
	target := fmt.Sprintf("%s/%d/staking", remoteConfigDir, node.node.Number)
	files := "staker.crt staker.key"
	if node.node.Role == config.RoleValidator {
		files += " signer.key"
	}
	command := fmt.Sprintf(
		"sudo rm -rf %[1]s && sudo install -d -o %[2]s -g %[2]s -m 0700 %[1]s && "+
			"for file in %[3]s; do sudo install -o %[2]s -g %[2]s -m 0600 %[4]s/$file %[1]s/$file; done && "+
			"rm -rf %[4]s",
		target, deployment.environment.SSHUser, files, stage)
	return d.runSSH(ctx, deployment, node, command)
}

func (d *Deployer) seedBeacon(ctx context.Context, deployment deployment, node nodeDeployment) error {
	dataDir := fmt.Sprintf("%s/%d", remoteDataDir, node.node.Number)
	databaseDir := dataDir + "/db"
	output, err := d.runSSHOutput(
		ctx,
		deployment,
		node,
		fmt.Sprintf("if [ -n \"$(find %s -mindepth 1 -print -quit 2>/dev/null)\" ]; then printf present; else printf missing; fi", databaseDir),
	)
	if err != nil {
		return err
	}
	switch strings.TrimSpace(string(output)) {
	case "present":
		fmt.Fprintf(d.out, "P-chain beacon database already exists; preserving %s\n", databaseDir)
		return nil
	case "missing":
	default:
		return fmt.Errorf("unexpected P-chain database check result %q", strings.TrimSpace(string(output)))
	}

	stage := dataDir + "/.pchain-import"
	if err := d.runSSH(
		ctx,
		deployment,
		node,
		fmt.Sprintf(
			"sudo rm -rf %[1]s && sudo install -d -o %[2]s -g %[2]s -m 0700 %[1]s",
			stage,
			deployment.environment.SSHUser,
		),
	); err != nil {
		return err
	}
	if err := d.rsyncFile(ctx, deployment, node, filepath.Join(d.root, pchainArchive), stage); err != nil {
		return err
	}
	command := fmt.Sprintf(
		"mkdir -m 700 %[1]s/unpacked && "+
			"tar -xzf %[1]s/%[2]s -C %[1]s/unpacked && "+
			"test -n \"$(find %[1]s/unpacked/db -mindepth 1 -print -quit 2>/dev/null)\" && "+
			"sudo rm -rf %[3]s && "+
			"sudo mv %[1]s/unpacked/db %[3]s && "+
			"sudo chown -R %[4]s:%[4]s %[3]s && "+
			"sudo rm -rf %[1]s",
		stage,
		pchainArchive,
		databaseDir,
		deployment.environment.SSHUser,
	)
	if err := d.runSSH(ctx, deployment, node, command); err != nil {
		return fmt.Errorf("%s must contain a non-empty db/ directory: %w", pchainArchive, err)
	}
	fmt.Fprintf(d.out, "restored P-chain beacon database from %s\n", pchainArchive)
	return nil
}

func (d *Deployer) start(ctx context.Context, deployment deployment, node nodeDeployment) error {
	return d.runSSH(ctx, deployment, node, "sudo systemctl start "+serviceName(node))
}

func (d *Deployer) waitBeaconReady(ctx context.Context, deployment deployment) error {
	deadline := time.Now().Add(d.waitLimit)
	uri := fmt.Sprintf("http://%s:%d", deployment.beacon.node.Host, deployment.beacon.httpPort)
	client := platformvm.NewClient(uri)
	var lastError error
	for time.Now().Before(deadline) {
		manager, managerErr := client.GetCurrentValidators(ctx, deployment.managerSubnetID, nil)
		main, mainErr := client.GetCurrentValidators(ctx, deployment.subnetID, nil)
		if managerErr == nil && mainErr == nil &&
			containsValidators(manager, deployment.expectedManager) &&
			containsValidators(main, deployment.expectedMain) {
			fmt.Fprintf(d.out, "P-chain beacon contains management and main L1 validator state\n")
			return nil
		}
		lastError = fmt.Errorf("management=%v main=%v", managerErr, mainErr)
		if err := wait(ctx, time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("P-chain state did not become ready within %s: %v", d.waitLimit, lastError)
}

func containsValidators(validators []platformvm.ClientPermissionlessValidator, expected map[ids.NodeID]struct{}) bool {
	found := make(map[ids.NodeID]struct{}, len(validators))
	for _, validator := range validators {
		found[validator.NodeID] = struct{}{}
	}
	for nodeID := range expected {
		if _, exists := found[nodeID]; !exists {
			return false
		}
	}
	return true
}

func (d *Deployer) waitL1Ready(ctx context.Context, deployment deployment, node nodeDeployment) error {
	deadline := time.Now().Add(d.waitLimit)
	url := fmt.Sprintf("http://%s:%d/ext/bc/%s/rpc", node.node.Host, node.httpPort, deployment.chainID)
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)
	var lastError error
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := d.http.Do(request)
		if err == nil {
			var result struct {
				Result string          `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&result)
			response.Body.Close()
			if response.StatusCode == http.StatusOK && decodeErr == nil && result.Result != "" && len(result.Error) == 0 {
				fmt.Fprintf(d.out, "node %d serves L1 at height %s\n", node.node.Number, result.Result)
				return nil
			}
			lastError = fmt.Errorf("HTTP %s", response.Status)
		} else {
			lastError = err
		}
		if err := wait(ctx, time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("L1 did not become ready within %s: %v", d.waitLimit, lastError)
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
