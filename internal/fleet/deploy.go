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
	"sync"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/placement"
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
		root:   root,
		out:    out,
		runner: osCommandRunner{stdout: out, stderr: os.Stderr},
		http:   &http.Client{Timeout: 5 * time.Second},
		// 30 minutes, not 10: twelve nodes bootstrapping the P-chain from one
		// frozen relay measured about 7 to 8 minutes each, and the old limit
		// aborted deploys whose nodes were healthy and still progressing.
		waitLimit: 30 * time.Minute,
	}
}

type deployment struct {
	environment     config.FleetEnvironment
	pchain          nodeDeployment
	selected        []nodeDeployment
	chainID         ids.ID
	subnetID        ids.ID
	managerSubnetID ids.ID
	oracleChainID   ids.ID
	oracleSubnetID  ids.ID
	expectedMain    map[ids.NodeID]struct{}
	expectedManager map[ids.NodeID]struct{}
	expectedOracle  map[ids.NodeID]struct{}
	pchainMode      string
}

// oracleRole reports whether the role lives on the oracle L1.
func oracleRole(role config.Role) bool {
	return role == config.RoleOracleValidator || role == config.RoleOracleRPC
}

// l1For returns the chain and subnet a node serves: oracle roles live on the
// oracle L1, every other L1 role on the main one.
func (p deployment) l1For(role config.Role) (ids.ID, ids.ID) {
	if oracleRole(role) {
		return p.oracleChainID, p.oracleSubnetID
	}
	return p.chainID, p.subnetID
}

type nodeDeployment struct {
	node      config.Node
	identity  creation.PublicNode
	httpPort  int
	renderDir string
}

// l1DeployPhases is the per-node sequence that installs software and brings an
// L1 node back up.
var l1DeployPhases = []struct {
	name   string
	action func(*Deployer) func(context.Context, deployment, nodeDeployment) error
}{
	{"stop", func(d *Deployer) func(context.Context, deployment, nodeDeployment) error { return d.stop }},
	{"package", func(d *Deployer) func(context.Context, deployment, nodeDeployment) error { return d.installPackage }},
	{"systemd", func(d *Deployer) func(context.Context, deployment, nodeDeployment) error { return d.installUnit }},
	{"identity", func(d *Deployer) func(context.Context, deployment, nodeDeployment) error { return d.installIdentity }},
	{"start", func(d *Deployer) func(context.Context, deployment, nodeDeployment) error { return d.start }},
	{"readiness", func(d *Deployer) func(context.Context, deployment, nodeDeployment) error { return d.waitL1Ready }},
}

// Deploy installs software on the L1 fleet and brings it up.
//
// With no selectors it deploys the whole inventory: the P-chain node first,
// gated on its validator sets, then every L1 node phase by phase (all stop,
// all package, ... , all start). That ordering is REQUIRED for a fresh fleet,
// because a node only finishes bootstrapping once 75% of stake is connected,
// so no node can become ready until its peers are also running.
//
// With selectors it is a ROLLING UPGRADE of already-running nodes: the P-chain
// node is left alone (upgrade it with `fleet pchain freeze|follow`, which
// reinstalls its package too) and each selected node runs the entire phase
// sequence, including readiness, before the next node is touched. Use this to
// replace binaries on a live fleet. Restarting several nodes at once loses the
// peers that serve state-sync summaries, and a node that cannot get a summary
// replays the whole chain from genesis instead of syncing.
//
// dryRun stops after the preflight: every host is validated, nothing is
// installed, stopped, or started. It exists so a release can be checked
// against the hosts without committing the running fleet to the upgrade.
func (d *Deployer) Deploy(ctx context.Context, pchainMode string, selectors []string, dryRun bool) error {
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

	rolling := len(selectors) > 0
	if rolling {
		chosen, err := selectNodes(l1Only(prepared.selected), selectors)
		if err != nil {
			if selectorsName(selectors, prepared.pchain.node.Number) {
				err = fmt.Errorf("%w. Node %d is the P-chain machine and a rolling deploy leaves it alone; reinstall it with fleet pchain freeze|follow, or just restart it with fleet pchain start", err, prepared.pchain.node.Number)
			}
			return err
		}
		picked := make([]nodeDeployment, 0, len(chosen))
		for _, want := range chosen {
			for _, node := range prepared.selected {
				if node.node.Number == want.Number {
					picked = append(picked, node)
					break
				}
			}
		}
		prepared.selected = picked
	}

	// EVERY host is validated before ANY node is stopped. A full deploy also
	// covers the P-chain machine, because reconciling it is the next thing
	// that happens; a rolling deploy never touches it.
	preflightTargets := prepared.selected
	if !rolling {
		preflightTargets = append([]nodeDeployment{prepared.pchain}, prepared.selected...)
	}
	if err := d.preflightHosts(ctx, prepared, preflightTargets); err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintln(d.out, "dry run: nothing was deployed")
		return nil
	}

	if !rolling {
		// The P-chain node is reconciled and accepted before any L1 node is
		// touched. This is a phase barrier, not a best-effort bootstrap hint.
		if err := d.reconcilePChain(ctx, prepared, prepared.pchainMode == frozenMode); err != nil {
			return err
		}
		if err := d.waitPChainReady(ctx, prepared); err != nil {
			return fmt.Errorf("P-chain readiness phase node %d (%s): %w", prepared.pchain.node.Number, prepared.pchain.node.Host, err)
		}
	}

	if rolling {
		for _, node := range prepared.selected {
			fmt.Fprintf(d.out, "== node %d (%s)\n", node.node.Number, node.node.Host)
			one := prepared
			one.selected = []nodeDeployment{node}
			for _, phase := range l1DeployPhases {
				if err := d.phase(ctx, one, phase.name, phase.action(d)); err != nil {
					return err
				}
			}
		}
		fmt.Fprintf(d.out, "rolled %d L1 node(s), one at a time\n", len(prepared.selected))
		return nil
	}

	for _, phase := range l1DeployPhases {
		if err := d.phase(ctx, prepared, phase.name, phase.action(d)); err != nil {
			return err
		}
	}
	fmt.Fprintf(d.out, "deployed P-chain node and %d L1 node(s)\n", len(prepared.selected))
	return nil
}

// l1Only projects the prepared deployments back to plain inventory nodes so the
// shared selector parser can be reused.
func l1Only(nodes []nodeDeployment) []config.Node {
	result := make([]config.Node, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, node.node)
	}
	return result
}

func (d *Deployer) FollowPChain(ctx context.Context) error {
	prepared, cleanup, err := d.prepare(followMode, false)
	if err != nil {
		return err
	}
	defer cleanup()

	// Reconciling stops the node before reinstalling it, so the host is
	// validated first: a mismatch must not leave the node down.
	if err := d.preflightHosts(ctx, prepared, []nodeDeployment{prepared.pchain}); err != nil {
		return err
	}
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

// howToProduceArchive turns the empty-state dead end into instructions. A
// frozen deploy cannot invent the archive, so the error names the exact
// sequence that produces one and the alternative that needs none.
const howToProduceArchive = `
Produce the archive from a synchronized P-chain node:

  fleet pchain follow    start the P-chain node following its upstream
  fleet status           wait for synced and both validator sets visible
  fleet pchain archive   write ./` + pchainArchive + `
  fleet deploy frozen    retry this command

Or deploy without an archive and let the P-chain node follow the public
network instead:

  fleet deploy follow`

func (d *Deployer) validateFrozenDeployArchive() error {
	archivePath := filepath.Join(d.root, pchainArchive)
	info, err := os.Stat(archivePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("frozen deploy requires ./%s; file not found\n%s", pchainArchive, howToProduceArchive)
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
	l := layoutFor(environment)
	unit := serviceName(pchain)
	if err := d.runSSH(ctx, state, pchain, l.isActiveCommand(pchain)); err != nil {
		return fmt.Errorf("P-chain node service %s must be running: %w", unit, err)
	}
	stopErr := d.runSSH(ctx, state, pchain, l.stopCommand(pchain))
	if stopErr != nil {
		recoveryCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		restartErr := d.runSSH(recoveryCtx, state, pchain, l.startCommand(pchain))
		return errors.Join(
			fmt.Errorf("stop P-chain node: %w", stopErr),
			wrapIfError("restore P-chain node after uncertain stop", restartErr),
		)
	}

	dataDir := fmt.Sprintf("%s/%d", l.data, pchainNode.Number)
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
	restartErr := d.runSSH(recoveryCtx, state, pchain, l.startCommand(pchain))
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

// phaseParallel runs an action on every selected machine concurrently and
// reports the first failure by machine number, so a fleet-wide push costs one
// round trip instead of twelve. Use it only for actions that are independent per
// machine: anything that must be ordered, or gated on all nodes reaching a state
// first, belongs in phase.
func (d *Deployer) phaseParallel(
	ctx context.Context,
	deployment deployment,
	name string,
	action func(context.Context, deployment, nodeDeployment) error,
) error {
	if len(deployment.selected) == 0 {
		return nil
	}
	fmt.Fprintf(d.out, "%s phase\n", name)
	failures := make([]error, len(deployment.selected))
	var wait sync.WaitGroup
	for i, node := range deployment.selected {
		wait.Add(1)
		go func(i int, node nodeDeployment) {
			defer wait.Done()
			if err := action(ctx, deployment, node); err != nil {
				failures[i] = fmt.Errorf("%s phase node %d (%s): %w", name, node.node.Number, node.node.Host, err)
			}
		}(i, node)
	}
	wait.Wait()
	// Report in machine order, not completion order, so the same fleet state
	// always produces the same error.
	return errors.Join(failures...)
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

	// Which identity belongs on a machine is placement.json, NOT public.json.
	// public.json records the ORIGINAL keygen assignment and never changes, so
	// resolving identities from it makes deploy silently undo every key swap: a
	// failover that moved an identity off a dead box would put it straight back
	// on the next deploy. placement.json is the control-side truth that place
	// and start already honour.
	currentPlacement, err := placement.Load(placement.Path(d.root))
	if err != nil {
		return deployment{}, noCleanup, err
	}
	if err := placement.Validate(currentPlacement, public, nodes); err != nil {
		return deployment{}, noCleanup, err
	}
	identityByLetter := make(map[string]creation.PublicNode, len(public.Nodes))
	for _, node := range public.Nodes {
		identityByLetter[node.Identity] = node
	}
	assignedIdentity := func(node config.Node) (creation.PublicNode, error) {
		letter, placed := currentPlacement[node.Number]
		if !placed {
			return creation.PublicNode{}, fmt.Errorf("deployment/placement.json has no identity for node %d", node.Number)
		}
		identity, known := identityByLetter[letter]
		if !known {
			return creation.PublicNode{}, fmt.Errorf("deployment/placement.json assigns unknown identity %q to node %d", letter, node.Number)
		}
		return identity, nil
	}

	// Peer pairings resolve through placement, exactly like a node's own
	// identity. Rendering them from publicByNode, the original keygen mapping,
	// is what silently broke after the first key swap.
	assignedByNode := make(map[int]creation.PublicNode, len(nodes))
	for _, node := range nodes {
		identity, err := assignedIdentity(node)
		if err != nil {
			return deployment{}, noCleanup, err
		}
		assignedByNode[node.Number] = identity
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
	hasOracle, hasArchive := false, false
	for _, node := range nodes {
		hasOracle = hasOracle || oracleRole(node.Role)
		hasArchive = hasArchive || node.Role == config.RoleArchive
	}
	var oracleChainID, oracleSubnetID ids.ID
	if hasOracle {
		if oracleChainID, err = requiredID(state, "ORACLE_CHAIN_ID"); err != nil {
			return deployment{}, noCleanup, err
		}
		if oracleSubnetID, err = requiredID(state, "ORACLE_SUBNET_ID"); err != nil {
			return deployment{}, noCleanup, err
		}
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
		if hasOracle {
			requiredFiles = append(requiredFiles, filepath.Join(d.root, "subnet-config-oracle.json"))
		}
		if hasArchive {
			requiredFiles = append(requiredFiles, filepath.Join(d.root, "chain-config-archive.json"))
		}
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
		oracleChainID:   oracleChainID,
		oracleSubnetID:  oracleSubnetID,
		expectedMain:    make(map[ids.NodeID]struct{}),
		expectedManager: make(map[ids.NodeID]struct{}),
		expectedOracle:  make(map[ids.NodeID]struct{}),
		pchainMode:      pchainMode,
	}
	for _, node := range public.Nodes {
		switch node.Role {
		case config.RoleValidator:
			nodeID, _ := ids.NodeIDFromString(node.NodeID)
			result.expectedMain[nodeID] = struct{}{}
		case config.RoleOracleValidator:
			nodeID, _ := ids.NodeIDFromString(node.NodeID)
			result.expectedOracle[nodeID] = struct{}{}
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
	pchainIdentity, err := assignedIdentity(pchain)
	if err != nil {
		cleanup()
		return deployment{}, noCleanup, err
	}
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
	// deploy is declarative: it installs and brings up the machines it is given,
	// so it renders the whole inventory into the address book and does not care
	// which machines happen to be up right now. Narrowing the book to what is
	// live is a lifecycle concern, handled by start, stop, destroy and place.
	deployUp := make(map[int]bool, len(nodes))
	for _, node := range nodes {
		deployUp[node.Number] = node.Role != config.RolePChain
	}
	for _, node := range nodes {
		if node.Role == config.RolePChain {
			continue
		}
		generated, err := assignedIdentity(node)
		if err != nil {
			cleanup()
			return deployment{}, noCleanup, err
		}
		if err := validateIdentityFiles(d.root, generated); err != nil {
			cleanup()
			return deployment{}, noCleanup, err
		}
		renderDir := filepath.Join(renderRoot, strconv.Itoa(node.Number))
		bootstrapIP := fmt.Sprintf("%s:%d", pchain.Host, ports[pchain.Number][1])
		stateSyncIPs, stateSyncIDs := stateSyncPeers(node, nodes, assignedByNode, ports, deployUp)
		nodeChainID, nodeSubnetID := result.l1For(node.Role)
		if err := renderNode(
			renderDir,
			d.root,
			environment,
			node,
			generated,
			nodeChainID,
			nodeSubnetID,
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

// stateSyncPeers builds a node's L1 address book: every other L1 machine paired
// with the identity placement currently assigns to it. The P-chain machine is
// excluded because it does not track the L1.
// stateSyncPeers builds a node's L1 address book: every other L1 machine that is
// MEANT TO BE UP, paired with the identity placement currently assigns to it. The
// P-chain machine is excluded because it does not track the L1.
//
// Listing only the intended-up machines is what keeps state sync usable through a
// site loss. The list doubles as the state-sync beacon set at weight 1 per entry
// with alpha = count/2 + 1, computed over the LIST rather than over who answers,
// so a list naming machines that are down raises the bar without adding anybody
// who can clear it. Twelve entries with six down leaves five reachable against an
// alpha of six and NO node can state-sync; five entries with all five reachable
// needs three and works. An empty list drops the override entirely, which falls
// back to the stake-weighted subnet validator set, a fine default.
func stateSyncPeers(
	node config.Node,
	nodes []config.Node,
	assigned map[int]creation.PublicNode,
	ports map[int][2]int,
	up map[int]bool,
) (string, string) {
	var peerIPs []string
	var peerIDs []string
	for _, peer := range nodes {
		if peer.Number == node.Number || peer.Role == config.RolePChain || !up[peer.Number] {
			continue
		}
		// Only a node on the same L1 holds that chain's state-sync summaries.
		if oracleRole(peer.Role) != oracleRole(node.Role) {
			continue
		}
		peerIPs = append(peerIPs, fmt.Sprintf("%s:%d", peer.Host, ports[peer.Number][1]))
		peerIDs = append(peerIDs, assigned[peer.Number].NodeID)
	}
	return strings.Join(peerIPs, ","), strings.Join(peerIDs, ",")
}

// intendedUp reports which L1 machines the fleet is meant to be running, then
// applies this command's own effect: `bringingUp` counts as up even though it has
// not started yet, `takingDown` counts as down even though it is still running.
// Without that adjustment every command would render the address book for the
// fleet as it was before the command rather than after it.
//
// Intent is systemd's enabled flag, the same single source of up/down truth the
// rest of the kit uses, so `stop` and `destroy` record it for free by disabling
// the unit. A machine that does not answer counts as DOWN: a lost site never gets
// to record its intent, and that is precisely the case this exists for.
func (d *Deployer) intendedUp(ctx context.Context, inv inventory, bringingUp, takingDown []int) (map[int]bool, error) {
	l := layoutFor(inv.environment)
	l1 := inv.l1Nodes()
	enabled := make([]bool, len(l1))
	var wait sync.WaitGroup
	for i, node := range l1 {
		target, err := inv.target(node)
		if err != nil {
			return nil, err
		}
		wait.Add(1)
		go func(i int, target nodeDeployment) {
			defer wait.Done()
			output, err := d.runSSHOutput(ctx, deployment{environment: inv.environment}, target,
				l.enabledProbe(target))
			// "enabled" is systemd's answer, "installed" the user layout's:
			// both mean this machine is meant to be up.
			state := strings.TrimSpace(string(output))
			enabled[i] = err == nil && (state == "enabled" || state == "installed")
		}(i, target)
	}
	wait.Wait()

	up := make(map[int]bool, len(l1))
	for i, node := range l1 {
		up[node.Number] = enabled[i]
	}
	for _, number := range takingDown {
		up[number] = false
	}
	for _, number := range bringingUp {
		up[number] = true
	}
	return up, nil
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
	l := layoutFor(environment)
	nodeRoot := filepath.Join(l.data, strconv.Itoa(node.Number))
	nodePackage := filepath.Join(l.pkg, strconv.Itoa(node.Number))
	nodeConfig := filepath.Join(l.cfg, strconv.Itoa(node.Number))
	stakingDir := filepath.Join(nodeConfig, "staking")
	cfg["network-id"] = environment.Network
	cfg["data-dir"] = nodeRoot
	cfg["db-dir"] = filepath.Join(nodeRoot, "db")
	cfg["log-dir"] = filepath.Join(nodeRoot, "logs")
	cfg["http-host"] = "0.0.0.0"
	cfg["http-port"] = ports[0]
	cfg["staking-port"] = ports[1]
	cfg["public-ip"] = node.Host
	cfg["partial-sync-primary-network"] = true
	// Isolated fleets address each other by private IPs, which AvalancheGo
	// refuses to dial on public networks unless told otherwise. Nodes with
	// public inventory addresses are unaffected.
	cfg["network-allow-private-ips"] = true
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
		cfg["chain-config-dir"] = filepath.Join(nodeConfig, "chains")
		cfg["subnet-config-dir"] = filepath.Join(nodeConfig, "subnets")
		// The P-chain node is the sole bootstrap: a peer-to-peer rendezvous
		// point for P-chain state, which it serves without tracking the L1.
		cfg["bootstrap-ips"] = bootstrapIP
		cfg["bootstrap-ids"] = bootstrapID
		// state-sync-ips/ids is ALSO the L1 address book, not just a beacon
		// override: node.go feeds both it and the bootstrappers to
		// Net.ManuallyTrack. Nothing else can introduce these nodes to each
		// other, because they run partial-sync-primary-network (so they are not
		// primary-network validators whose IPs get gossiped) and the sole
		// bootstrapper does not track the L1. Drop this list and every node
		// comes back from a restart holding exactly one peer, the P-chain node,
		// and never reaches the 75% connected-stake gate. Measured 2026-07-31.
		//
		// The pairing therefore resolves through placement, never public.json:
		// the entry must name the identity the machine runs NOW, or the dial
		// fails TLS verification and the peer is silently unreachable.
		cfg["state-sync-ips"] = stateSyncIPs
		cfg["state-sync-ids"] = stateSyncIDs
		if node.Role == config.RoleValidator || node.Role == config.RoleOracleValidator {
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
		switch node.Role {
		case config.RoleRPC, config.RoleOracleRPC:
			chainConfig = "chain-config-rpc.json"
		case config.RoleArchive:
			chainConfig = "chain-config-archive.json"
		}
		subnetConfig := "subnet-config.json"
		if oracleRole(node.Role) {
			subnetConfig = "subnet-config-oracle.json"
		}
		for _, copyPair := range [][2]string{
			{filepath.Join(root, chainConfig), filepath.Join(renderDir, "chain.json")},
			{filepath.Join(root, subnetConfig), filepath.Join(renderDir, "subnet.json")},
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
	if l.user {
		// A user install has no systemd: the node is a plain process started by
		// this script, with the pidfile as its handle. No boot persistence and no
		// auto-restart; the operator owns the lifecycle.
		run := fmt.Sprintf(`#!/bin/sh
# Avalanche benchmark node %[1]d (%[2]s, %[3]s), user-level install.
mkdir -p %[4]s/logs
setsid nohup %[5]s/bin/avalanchego --config-file=%[6]s/node.json >> %[4]s/logs/console.log 2>&1 &
echo $! > %[7]s
`, node.Number, generated.Identity, node.Role, nodeRoot, nodePackage, nodeConfig, l.pidFile(node.Number))
		return os.WriteFile(filepath.Join(renderDir, "run.sh"), []byte(run), 0o700)
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
`, node.Number, generated.Identity, node.Role, environment.SSHUser, nodePackage, l.cfg, node.Number)
	return os.WriteFile(filepath.Join(renderDir, "node.service"), []byte(unit), 0o600)
}

func writeJSON(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(contents, '\n'), 0o600)
}

func validateIdentityFiles(root string, generated creation.PublicNode) error {
	dir := filepath.Join(root, "deployment", "identities", generated.Identity)
	names := []string{"staker.crt", "staker.key"}
	if generated.Role == config.RoleValidator || generated.Role == config.RoleOracleValidator {
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

// stagingDir keys the upload staging path on the remote user: /tmp is shared
// and sticky, so a fixed name left behind by one operator is undeletable by
// the next (observed 2026-08-04 on shared dev hosts).
func stagingDir(user string, node nodeDeployment, suffix string) string {
	return fmt.Sprintf("/tmp/avalanche-benchmark-%s-%d-%s", user, node.node.Number, suffix)
}

func (d *Deployer) stop(ctx context.Context, deployment deployment, node nodeDeployment) error {
	return d.runSSH(ctx, deployment, node, layoutFor(deployment.environment).stopCommand(node))
}

func (d *Deployer) installPackage(ctx context.Context, deployment deployment, node nodeDeployment) error {
	packageStage := stagingDir(deployment.environment.SSHUser, node, "package")
	binaryStage := stagingDir(deployment.environment.SSHUser, node, "binary")
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
	l := layoutFor(deployment.environment)
	number := node.node.Number
	segments := []string{
		l.sudo(fmt.Sprintf("install -d -m 0755 %[1]s/%[3]d/bin %[2]s/%[3]d %[4]s/%[3]d/db %[4]s/%[3]d/logs",
			l.pkg, l.cfg, number, l.data)),
		l.sudo(fmt.Sprintf("install -m 0755 %s/avalanchego %s/%d/bin/avalanchego", binaryStage, l.pkg, number)),
		l.sudo(fmt.Sprintf("install -m 0644 %s/node.json %s/%d/node.json", packageStage, l.cfg, number)),
	}
	// A user install already owns its files; only a root install has ownership
	// to hand over to the service user.
	if !l.user {
		segments = append(segments, l.sudo(fmt.Sprintf("chown -R %[1]s:%[1]s %[2]s/%[4]d %[3]s/%[4]d",
			deployment.environment.SSHUser, l.data, l.cfg, number)))
	}
	if node.node.Role != config.RolePChain {
		nodeChainID, nodeSubnetID := deployment.l1For(node.node.Role)
		segments = append(segments,
			l.sudo(fmt.Sprintf("install -d -m 0755 %[1]s/%[2]d/plugins %[3]s/%[2]d/chains/%[4]s %[3]s/%[2]d/subnets",
				l.pkg, number, l.cfg, nodeChainID)),
			l.sudo(fmt.Sprintf("install -m 0755 %[1]s/%[2]s %[3]s/%[4]d/plugins/%[2]s", binaryStage, pluginID, l.pkg, number)),
			l.sudo(fmt.Sprintf("install -m 0644 %s/chain.json %s/%d/chains/%s/config.json", packageStage, l.cfg, number, nodeChainID)),
			l.sudo(fmt.Sprintf("install -m 0644 %s/subnet.json %s/%d/subnets/%s.json", packageStage, l.cfg, number, nodeSubnetID)),
		)
	}
	return d.runSSH(ctx, deployment, node, strings.Join(segments, " && "))
}

func (d *Deployer) installUnit(ctx context.Context, deployment deployment, node nodeDeployment) error {
	return d.runSSH(ctx, deployment, node, layoutFor(deployment.environment).installUnitCommand(stagingDir(deployment.environment.SSHUser, node, "package"), node))
}

// renderConfigs renders node.json for each target from the CURRENT inventory,
// resolving every identity through placement, and returns the targets with
// renderDir populated. start and place use it to refresh identity-derived
// configuration without a full deploy.
func (d *Deployer) renderConfigs(inv inventory, targets []nodeDeployment, up map[int]bool) ([]nodeDeployment, func(), error) {
	noCleanup := func() {}
	pchainIdentity, err := inv.assigned(inv.pchain)
	if err != nil {
		return nil, noCleanup, err
	}
	assignedByNode := make(map[int]creation.PublicNode, len(inv.nodes))
	for _, node := range inv.nodes {
		identity, err := inv.assigned(node)
		if err != nil {
			return nil, noCleanup, err
		}
		assignedByNode[node.Number] = identity
	}
	renderRoot, err := os.MkdirTemp("", "fleet-config-")
	if err != nil {
		return nil, noCleanup, fmt.Errorf("create config render directory: %w", err)
	}
	cleanup := func() { os.RemoveAll(renderRoot) }
	bootstrapIP := fmt.Sprintf("%s:%d", inv.pchain.Host, inv.ports[inv.pchain.Number][1])
	rendered := make([]nodeDeployment, 0, len(targets))
	for _, target := range targets {
		renderDir := filepath.Join(renderRoot, strconv.Itoa(target.node.Number))
		stateSyncIPs, stateSyncIDs := stateSyncPeers(target.node, inv.nodes, assignedByNode, inv.ports, up)
		// The mode argument only reaches the P-chain branch, and every target
		// here is an L1 machine.
		if err := renderNode(
			renderDir, d.root, inv.environment, target.node, target.identity,
			inv.chainID, inv.subnetID, inv.ports[target.node.Number],
			frozenMode, bootstrapIP, pchainIdentity.NodeID,
			stateSyncIPs, stateSyncIDs,
		); err != nil {
			cleanup()
			return nil, noCleanup, err
		}
		target.renderDir = renderDir
		rendered = append(rendered, target)
	}
	return rendered, cleanup, nil
}

// installConfig pushes the freshly rendered node.json and nothing else:
// binaries, chain configs, and units stay deploy's job.
func (d *Deployer) installConfig(ctx context.Context, deployment deployment, node nodeDeployment) error {
	stage := stagingDir(deployment.environment.SSHUser, node, "config")
	if err := d.runSSH(ctx, deployment, node, "rm -rf "+stage+" && mkdir -m 700 "+stage); err != nil {
		return err
	}
	if err := d.rsyncFile(ctx, deployment, node, filepath.Join(node.renderDir, "node.json"), stage); err != nil {
		return err
	}
	l := layoutFor(deployment.environment)
	return d.runSSH(ctx, deployment, node, strings.Join([]string{
		l.sudo(fmt.Sprintf("install -d -m 0755 %s/%d", l.cfg, node.node.Number)),
		l.sudo(fmt.Sprintf("install -m 0644 %s/node.json %s/%d/node.json", stage, l.cfg, node.node.Number)),
		"rm -rf " + stage,
	}, " && "))
}

func (d *Deployer) installIdentity(ctx context.Context, deployment deployment, node nodeDeployment) error {
	stage := stagingDir(deployment.environment.SSHUser, node, "identity")
	if err := d.runSSH(ctx, deployment, node, "rm -rf "+stage+" && mkdir -m 700 "+stage); err != nil {
		return err
	}
	local := filepath.Join(d.root, "deployment", "identities", node.identity.Identity)
	if err := d.rsync(ctx, deployment, node, local, stage); err != nil {
		return err
	}
	files := "staker.crt staker.key"
	if node.node.Role == config.RoleValidator || node.node.Role == config.RoleOracleValidator {
		files += " signer.key"
	}
	command := layoutFor(deployment.environment).installIdentityCommand(
		stage, files, deployment.environment.SSHUser, node.node.Number)
	return d.runSSH(ctx, deployment, node, command)
}

func (d *Deployer) seedPChain(ctx context.Context, deployment deployment, node nodeDeployment) error {
	l := layoutFor(deployment.environment)
	dataDir := fmt.Sprintf("%s/%d", l.data, node.node.Number)
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
	owner := deployment.environment.SSHUser
	if err := d.runSSH(
		ctx,
		deployment,
		node,
		l.sudo("rm -rf "+stage)+" && "+l.sudo(seedStageInstall(l, owner, stage)),
	); err != nil {
		return err
	}
	if err := d.rsyncFile(ctx, deployment, node, filepath.Join(d.root, pchainArchive), stage); err != nil {
		return err
	}
	segments := []string{
		fmt.Sprintf("mkdir -m 700 %s/unpacked", stage),
		fmt.Sprintf("tar -xzf %[1]s/%[2]s -C %[1]s/unpacked", stage, pchainArchive),
		fmt.Sprintf("test -n \"$(find %s/unpacked/db -mindepth 1 -print -quit 2>/dev/null)\"", stage),
		fmt.Sprintf("test -z \"$(find %s -mindepth 1 -print -quit 2>/dev/null)\"", databaseDir),
	}
	// A user install extracted the archive as itself; there is no ownership to
	// hand over, and naming a group would wrongly assume one matching the
	// user's name exists (a Debian convention RHEL hosts do not follow).
	if !l.user {
		segments = append(segments, l.sudo(fmt.Sprintf("chown -R %[1]s:%[1]s %[2]s/unpacked/db", owner, stage)))
	}
	segments = append(segments,
		l.sudo(fmt.Sprintf("mv -T %s/unpacked/db %s", stage, databaseDir)),
		l.sudo("rm -rf "+stage),
	)
	command := strings.Join(segments, " && ")
	if err := d.runSSH(ctx, deployment, node, command); err != nil {
		return fmt.Errorf("%s must contain a non-empty db/ directory: %w", pchainArchive, err)
	}
	fmt.Fprintf(d.out, "restored P-chain database from %s\n", pchainArchive)
	return nil
}

func (d *Deployer) start(ctx context.Context, deployment deployment, node nodeDeployment) error {
	return d.runSSH(ctx, deployment, node, layoutFor(deployment.environment).startOnlyCommand(node))
}

func (d *Deployer) startAndVerify(ctx context.Context, deployment deployment, node nodeDeployment) error {
	return d.runSSH(ctx, deployment, node, layoutFor(deployment.environment).startCommand(node))
}

// waitPChainReady blocks until the P-chain node passes the gate for its mode.
// The node runs with --p-chain-follow-only, so it never finishes bootstrapping
// and every platform.* call against it answers 503 forever: readiness is
// observed from its health check, log, and metrics, and the validator sets come
// from the public API. See pchainReadyOnce for the per-mode gate.
func (d *Deployer) waitPChainReady(ctx context.Context, deployment deployment) error {
	deadline := time.Now().Add(d.waitLimit)
	var lastError error
	for time.Now().Before(deadline) {
		ready, err := d.pchainReadyOnce(ctx, deployment)
		if err == nil {
			fmt.Fprintln(d.out, ready)
			return nil
		}
		lastError = err
		// One attempt costs an ssh round trip, and the node logs its progress
		// every 5 seconds anyway, so polling faster only adds connections.
		if err := wait(ctx, 5*time.Second); err != nil {
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
	nodeChainID, _ := deployment.l1For(node.node.Role)
	url := fmt.Sprintf("http://%s:%d/ext/bc/%s/rpc", node.node.Host, node.httpPort, nodeChainID)
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
