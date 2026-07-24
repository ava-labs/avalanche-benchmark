package fleet

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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
	pchain          nodeDeployment
	selected        []nodeDeployment
	chainID         ids.ID
	subnetID        ids.ID
	managerSubnetID ids.ID
	expectedMain    map[ids.NodeID]struct{}
	expectedManager map[ids.NodeID]struct{}
	pchainMode      string
}

type nodeDeployment struct {
	node      config.Node
	identity  creation.PublicNode
	httpPort  int
	renderDir string
}

func (d *Deployer) Deploy(ctx context.Context, pchainMode string) error {
	prepared, cleanup, err := d.prepare(pchainMode, true)
	if err != nil {
		return err
	}
	defer cleanup()
	if pchainMode == frozenMode {
		if err := d.validateFrozenDeployArchive(); err != nil {
			return err
		}
	}

	// The P-chain node is reconciled and accepted before any L1 node is
	// touched. This is a phase barrier, not a best-effort bootstrap hint.
	if err := d.reconcilePChain(ctx, prepared, prepared.pchainMode == frozenMode); err != nil {
		return err
	}
	if err := d.waitPChainReady(ctx, prepared); err != nil {
		return fmt.Errorf("P-chain readiness phase node %d (%s): %w", prepared.pchain.node.Number, prepared.pchain.node.Host, err)
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
	fmt.Fprintf(d.out, "deployed P-chain node and %d L1 node(s)\n", len(prepared.selected))
	return nil
}

func (d *Deployer) FollowPChain(ctx context.Context) error {
	prepared, cleanup, err := d.prepare(followMode, false)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := d.reconcilePChain(ctx, prepared, false); err != nil {
		return err
	}
	fmt.Fprintln(d.out, "P-chain node is running in following mode")
	return nil
}

func (d *Deployer) reconcilePChain(ctx context.Context, prepared deployment, seed bool) error {
	pchainOnly := prepared
	pchainOnly.selected = []nodeDeployment{prepared.pchain}
	for _, phase := range []struct {
		name   string
		action func(context.Context, deployment, nodeDeployment) error
	}{
		{"P-chain stop", d.stop},
		{"P-chain package", d.installPackage},
		{"P-chain systemd", d.installUnit},
		{"P-chain identity", d.installIdentity},
	} {
		if err := d.phase(ctx, pchainOnly, phase.name, phase.action); err != nil {
			return err
		}
	}
	if seed {
		if err := d.phase(ctx, pchainOnly, "P-chain seed", d.seedPChain); err != nil {
			return err
		}
	}
	return d.phase(ctx, pchainOnly, "P-chain start", d.startAndVerify)
}

func (d *Deployer) validateFrozenDeployArchive() error {
	archivePath := filepath.Join(d.root, pchainArchive)
	info, err := os.Stat(archivePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("frozen deploy requires ./%s; file not found", pchainArchive)
		}
		return fmt.Errorf("frozen deploy requires ./%s: %w", pchainArchive, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("frozen deploy requires ./%s to be a regular file", pchainArchive)
	}
	if err := validatePChainArchive(archivePath); err != nil {
		return fmt.Errorf("frozen deploy requires a valid ./%s: %w", pchainArchive, err)
	}
	return nil
}

func (d *Deployer) ArchivePChain(ctx context.Context) error {
	target := filepath.Join(d.root, pchainArchive)
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("./%s already exists; move or delete it explicitly", pchainArchive)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect ./%s: %w", pchainArchive, err)
	}

	environment, nodes, err := d.loadFleet()
	if err != nil {
		return err
	}
	var pchainNode config.Node
	for _, node := range nodes {
		if node.Role == config.RolePChain {
			pchainNode = node
			break
		}
	}
	pchain := nodeDeployment{node: pchainNode}
	state := deployment{environment: environment}
	unit := serviceName(pchain)
	if err := d.runSSH(ctx, state, pchain, "sudo systemctl is-active --quiet "+unit); err != nil {
		return fmt.Errorf("P-chain node service %s must be running: %w", unit, err)
	}
	stopErr := d.runSSH(
		ctx,
		state,
		pchain,
		fmt.Sprintf(
			"sudo systemctl stop %[1]s && test \"$(sudo systemctl is-active %[1]s)\" = inactive",
			unit,
		),
	)
	if stopErr != nil {
		recoveryCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		restartErr := d.runSSH(
			recoveryCtx,
			state,
			pchain,
			fmt.Sprintf(
				"sudo systemctl start %[1]s && sudo systemctl is-active --quiet %[1]s",
				unit,
			),
		)
		return errors.Join(
			fmt.Errorf("stop P-chain node: %w", stopErr),
			wrapIfError("restore P-chain node after uncertain stop", restartErr),
		)
	}

	dataDir := fmt.Sprintf("%s/%d", remoteDataDir, pchainNode.Number)
	remoteArchive := dataDir + "/.pchain-export.tar.gz"
	archiveErr := d.runSSH(
		ctx,
		state,
		pchain,
		fmt.Sprintf(
			"test -n \"$(find %[1]s/db -mindepth 1 -print -quit 2>/dev/null)\" && "+
				"rm -f %[2]s && tar -C %[1]s -czf %[2]s db/",
			dataDir,
			remoteArchive,
		),
	)
	recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), time.Minute)
	restartErr := d.runSSH(
		recoveryCtx,
		state,
		pchain,
		fmt.Sprintf(
			"sudo systemctl start %[1]s && sudo systemctl is-active --quiet %[1]s",
			unit,
		),
	)
	cancelRecovery()
	if archiveErr != nil || restartErr != nil {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), time.Minute)
		cleanupErr := d.runSSH(cleanupCtx, state, pchain, "rm -f "+remoteArchive)
		cancelCleanup()
		return errors.Join(
			wrapIfError("archive P-chain database", archiveErr),
			wrapIfError("restart P-chain node", restartErr),
			wrapIfError("remove remote partial archive", cleanupErr),
		)
	}
	fmt.Fprintln(d.out, "P-chain node restarted; downloading archive")

	partial := target + ".partial"
	if err := os.Remove(partial); err != nil && !os.IsNotExist(err) {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), time.Minute)
		cleanupErr := d.runSSH(cleanupCtx, state, pchain, "rm -f "+remoteArchive)
		cancelCleanup()
		return errors.Join(
			fmt.Errorf("remove stale ./%s.partial: %w", pchainArchive, err),
			wrapIfError("remove remote archive", cleanupErr),
		)
	}
	defer os.Remove(partial)
	downloadErr := d.downloadFile(ctx, state, pchain, remoteArchive, partial)
	var validateErr error
	if downloadErr == nil {
		validateErr = validatePChainArchive(partial)
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), time.Minute)
	cleanupErr := d.runSSH(cleanupCtx, state, pchain, "rm -f "+remoteArchive)
	cancelCleanup()
	if downloadErr != nil || validateErr != nil {
		return errors.Join(
			wrapIfError("download P-chain archive", downloadErr),
			wrapIfError("validate P-chain archive", validateErr),
			wrapIfError("remove remote archive", cleanupErr),
		)
	}
	if err := os.Rename(partial, target); err != nil {
		return errors.Join(
			fmt.Errorf("publish ./%s: %w", pchainArchive, err),
			wrapIfError("remove remote archive", cleanupErr),
		)
	}
	fmt.Fprintf(d.out, "created ./%s\n", pchainArchive)
	if cleanupErr != nil {
		return fmt.Errorf("archive created, but remove remote archive: %w", cleanupErr)
	}
	return nil
}

func wrapIfError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
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

func (d *Deployer) prepare(pchainMode string, includeL1 bool) (deployment, func(), error) {
	noCleanup := func() {}
	if pchainMode != frozenMode && pchainMode != followMode {
		return deployment{}, noCleanup, fmt.Errorf("deploy mode must be %q or %q, got %q", frozenMode, followMode, pchainMode)
	}
	environment, nodes, err := d.loadFleet()
	if err != nil {
		return deployment{}, noCleanup, err
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
		filepath.Join(d.root, "node-config.json"),
	}
	if includeL1 {
		requiredFiles = append(requiredFiles,
			filepath.Join(d.root, "bin", pluginID),
			filepath.Join(d.root, "chain-config.json"),
			filepath.Join(d.root, "chain-config-rpc.json"),
			filepath.Join(d.root, "subnet-config.json"),
		)
	}
	for _, path := range requiredFiles {
		if info, err := os.Stat(path); err != nil {
			return deployment{}, noCleanup, fmt.Errorf("required deployment artifact %s is unavailable: %w", path, err)
		} else if info.IsDir() {
			return deployment{}, noCleanup, fmt.Errorf("required deployment artifact %s is a directory", path)
		}
	}
	avalancheGo := filepath.Join(d.root, "bin", "avalanchego")
	info, _ := os.Stat(avalancheGo)
	if info.Mode()&0o111 == 0 {
		return deployment{}, noCleanup, fmt.Errorf("required deployment binary %s is not executable", avalancheGo)
	}
	if includeL1 {
		plugin := filepath.Join(d.root, "bin", pluginID)
		info, _ := os.Stat(plugin)
		if info.Mode()&0o111 == 0 {
			return deployment{}, noCleanup, fmt.Errorf("required deployment binary %s is not executable", plugin)
		}
		if err := verifyConsensusConfig(filepath.Join(d.root, "subnet-config.json")); err != nil {
			return deployment{}, noCleanup, err
		}
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
		pchainMode:      pchainMode,
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

	var pchain config.Node
	for _, node := range nodes {
		if node.Role == config.RolePChain {
			pchain = node
			break
		}
	}
	pchainIdentity := publicByNode[pchain.Number]
	if err := validateIdentityFiles(d.root, pchainIdentity); err != nil {
		cleanup()
		return deployment{}, noCleanup, err
	}
	pchainRender := filepath.Join(renderRoot, strconv.Itoa(pchain.Number))
	if err := renderNode(pchainRender, d.root, environment, pchain, pchainIdentity, chainID, subnetID, ports[pchain.Number], pchainMode, "", "", "", ""); err != nil {
		cleanup()
		return deployment{}, noCleanup, err
	}
	result.pchain = nodeDeployment{
		node:      pchain,
		identity:  pchainIdentity,
		httpPort:  ports[pchain.Number][0],
		renderDir: pchainRender,
	}

	if !includeL1 {
		return result, cleanup, nil
	}
	for _, node := range nodes {
		if node.Role == config.RolePChain {
			continue
		}
		generated := publicByNode[node.Number]
		if err := validateIdentityFiles(d.root, generated); err != nil {
			cleanup()
			return deployment{}, noCleanup, err
		}
		renderDir := filepath.Join(renderRoot, strconv.Itoa(node.Number))
		bootstrapIP := fmt.Sprintf("%s:%d", pchain.Host, ports[pchain.Number][1])
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
			pchainMode,
			bootstrapIP,
			pchainIdentity.NodeID,
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

func (d *Deployer) loadFleet() (config.FleetEnvironment, []config.Node, error) {
	environment, err := config.LoadFleetEnvironment(filepath.Join(d.root, ".env"))
	if err != nil {
		return config.FleetEnvironment{}, nil, err
	}
	if !sshUserPattern.MatchString(environment.SSHUser) {
		return config.FleetEnvironment{}, nil, fmt.Errorf(".env: SSH_USER must be a Linux user name, got %q", environment.SSHUser)
	}
	nodes, err := config.LoadNodes(filepath.Join(d.root, "nodes.ini"))
	if err != nil {
		return config.FleetEnvironment{}, nil, err
	}
	for _, node := range nodes {
		if strings.ContainsAny(node.Host, " \t\r\n,") {
			return config.FleetEnvironment{}, nil, fmt.Errorf("node %d host must not contain whitespace or a comma, got %q", node.Number, node.Host)
		}
	}
	return environment, nodes, nil
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
		if peer.Number == node.Number || peer.Role == config.RolePChain {
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
	pchainMode string,
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
	if node.Role == config.RolePChain {
		cfg["p-chain-follow-only"] = true
		cfg["staking-ephemeral-signer-enabled"] = true
		if pchainMode == frozenMode {
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
	if node.Role != config.RolePChain {
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

func (d *Deployer) downloadFile(ctx context.Context, deployment deployment, node nodeDeployment, remote, local string) error {
	sshCommand := append([]string{"ssh"}, sshOptions(deployment.environment.SSHKeyPath)...)
	args := []string{
		"-rt",
		"-e", strings.Join(sshCommand, " "),
		fmt.Sprintf("%s@%s:%s", deployment.environment.SSHUser, node.node.Host, remote),
		local,
	}
	if err := d.runner.Run(ctx, "rsync", args...); err != nil {
		return fmt.Errorf("rsync %s: %w", remote, err)
	}
	return nil
}

func validatePChainArchive(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()

	reader := tar.NewReader(compressed)
	hasDatabaseFile := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		if name != "db" && !strings.HasPrefix(name, "db/") {
			return fmt.Errorf("unexpected archive path %q; only db/ is allowed", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
		case tar.TypeReg, tar.TypeRegA:
			if strings.HasPrefix(name, "db/") {
				hasDatabaseFile = true
			}
		default:
			return fmt.Errorf("unsupported archive entry %q", header.Name)
		}
	}
	if !hasDatabaseFile {
		return fmt.Errorf("archive has no files under db/")
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
	if node.node.Role != config.RolePChain {
		if err := d.rsyncFile(ctx, deployment, node, filepath.Join(d.root, "bin", pluginID), binaryStage); err != nil {
			return err
		}
	}
	install := fmt.Sprintf(
		"sudo install -d -m 0755 %[1]s/%[3]d/bin %[2]s/%[3]d %[4]s/%[3]d/db %[4]s/%[3]d/logs && "+
			"sudo install -m 0755 %[5]s/avalanchego %[1]s/%[3]d/bin/avalanchego && "+
			"sudo install -m 0644 %[6]s/node.json %[2]s/%[3]d/node.json && "+
			"sudo chown -R %[7]s:%[7]s %[4]s/%[3]d %[2]s/%[3]d",
		remotePackageDir, remoteConfigDir, node.node.Number, remoteDataDir,
		binaryStage, packageStage, deployment.environment.SSHUser)
	if node.node.Role != config.RolePChain {
		install += fmt.Sprintf(
			" && sudo install -d -m 0755 %[1]s/%[2]d/plugins %[3]s/%[2]d/chains/%[4]s %[3]s/%[2]d/subnets && "+
				"sudo install -m 0755 %[5]s/%[6]s %[1]s/%[2]d/plugins/%[6]s && "+
				"sudo install -m 0644 %[7]s/chain.json %[3]s/%[2]d/chains/%[4]s/config.json && "+
				"sudo install -m 0644 %[7]s/subnet.json %[3]s/%[2]d/subnets/%[8]s.json",
			remotePackageDir, node.node.Number, remoteConfigDir, deployment.chainID,
			binaryStage, pluginID, packageStage, deployment.subnetID)
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

func (d *Deployer) seedPChain(ctx context.Context, deployment deployment, node nodeDeployment) error {
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
		fmt.Fprintf(d.out, "P-chain database already exists; preserving %s\n", databaseDir)
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
			"test -z \"$(find %[3]s -mindepth 1 -print -quit 2>/dev/null)\" && "+
			"sudo chown -R %[4]s:%[4]s %[1]s/unpacked/db && "+
			"sudo mv -T %[1]s/unpacked/db %[3]s && "+
			"sudo rm -rf %[1]s",
		stage,
		pchainArchive,
		databaseDir,
		deployment.environment.SSHUser,
	)
	if err := d.runSSH(ctx, deployment, node, command); err != nil {
		return fmt.Errorf("%s must contain a non-empty db/ directory: %w", pchainArchive, err)
	}
	fmt.Fprintf(d.out, "restored P-chain database from %s\n", pchainArchive)
	return nil
}

func (d *Deployer) start(ctx context.Context, deployment deployment, node nodeDeployment) error {
	return d.runSSH(ctx, deployment, node, "sudo systemctl start "+serviceName(node))
}

func (d *Deployer) startAndVerify(ctx context.Context, deployment deployment, node nodeDeployment) error {
	unit := serviceName(node)
	return d.runSSH(
		ctx,
		deployment,
		node,
		fmt.Sprintf("sudo systemctl start %[1]s && sudo systemctl is-active --quiet %[1]s", unit),
	)
}

func (d *Deployer) waitPChainReady(ctx context.Context, deployment deployment) error {
	deadline := time.Now().Add(d.waitLimit)
	uri := fmt.Sprintf("http://%s:%d", deployment.pchain.node.Host, deployment.pchain.httpPort)
	client := platformvm.NewClient(uri)
	var lastError error
	for time.Now().Before(deadline) {
		manager, managerErr := client.GetCurrentValidators(ctx, deployment.managerSubnetID, nil)
		main, mainErr := client.GetCurrentValidators(ctx, deployment.subnetID, nil)
		if managerErr == nil && mainErr == nil &&
			containsValidators(manager, deployment.expectedManager) &&
			containsValidators(main, deployment.expectedMain) {
			fmt.Fprintln(d.out, "P-chain node contains management and main L1 validator state")
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
