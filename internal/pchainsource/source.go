package pchainsource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	serviceName = "avalanche-benchmark-pchain-source.service"
	localAPI    = "http://127.0.0.1:9650"
)

type nodeConfig struct {
	NetworkID                 string  `json:"network-id"`
	DataDir                   string  `json:"data-dir"`
	HTTPHost                  string  `json:"http-host"`
	HTTPPort                  uint16  `json:"http-port"`
	StakingPort               uint16  `json:"staking-port"`
	PartialSyncPrimaryNetwork bool    `json:"partial-sync-primary-network"`
	PChainFollowOnly          bool    `json:"p-chain-follow-only"`
	BootstrapIPs              *string `json:"bootstrap-ips,omitempty"`
	BootstrapIDs              *string `json:"bootstrap-ids,omitempty"`
}

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type Manager struct {
	root       string
	network    string
	publicAPI  string
	out        io.Writer
	runner     Runner
	httpClient *http.Client
	sleep      func(time.Duration)
}

func New(root, network, publicAPI string, out io.Writer) *Manager {
	return &Manager{
		root:      root,
		network:   network,
		publicAPI: strings.TrimRight(publicAPI, "/"),
		out:       out,
		runner:    commandRunner{},
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		sleep: time.Sleep,
	}
}

func (m *Manager) Follow(ctx context.Context) error {
	if err := m.install(ctx, nil, nil); err != nil {
		return err
	}

	fmt.Fprintf(m.out, "following AvalancheGo's built-in %s bootstrap peers; waiting up to 30s for a peer\n", m.network)
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, err := m.fetchLocalStatus(ctx)
		if err == nil && len(status.PeerNodeIDs) > 0 {
			return m.printStatus(ctx)
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("source restarted but its API did not become ready: %w", err)
			}
			return fmt.Errorf("source restarted but did not connect to an AvalancheGo default bootstrap peer within 30s")
		}
		m.sleep(time.Second)
	}
}

func (m *Manager) Freeze(ctx context.Context) error {
	cfg, err := m.loadConfig()
	if err != nil {
		return fmt.Errorf("freeze requires an existing P-chain source: %w", err)
	}
	if cfg.BootstrapIPs != nil {
		return fmt.Errorf("P-chain source is already frozen")
	}
	empty := ""
	if err := m.install(ctx, &empty, &empty); err != nil {
		return err
	}
	fmt.Fprintln(m.out, "frozen with explicit empty bootstrap IP and NodeID lists")
	return m.waitForAPIAndPrint(ctx)
}

func (m *Manager) Status(ctx context.Context) error {
	if _, err := m.loadConfig(); err != nil {
		return fmt.Errorf("status requires an existing P-chain source: %w", err)
	}
	active, err := m.run(ctx, "systemctl", "is-active", serviceName)
	if err != nil {
		return fmt.Errorf("%s is not active: %w", serviceName, err)
	}
	if strings.TrimSpace(string(active)) != "active" {
		return fmt.Errorf("%s is not active: %s", serviceName, strings.TrimSpace(string(active)))
	}
	return m.printStatus(ctx)
}

func (m *Manager) install(ctx context.Context, bootstrapIPs, bootstrapIDs *string) error {
	binaryPath := filepath.Join(m.root, "bin", "avalanchego")
	info, err := os.Stat(binaryPath)
	if err != nil {
		return fmt.Errorf("required packaged binary %s is unavailable: %w", binaryPath, err)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("required packaged binary %s is not executable", binaryPath)
	}

	sourceDir := filepath.Join(m.root, "data", "pchain-source")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		return fmt.Errorf("create P-chain source directory %s: %w", sourceDir, err)
	}
	cfg := nodeConfig{
		NetworkID:                 m.network,
		DataDir:                   sourceDir,
		HTTPHost:                  "127.0.0.1",
		HTTPPort:                  9650,
		StakingPort:               9651,
		PartialSyncPrimaryNetwork: true,
		PChainFollowOnly:          true,
		BootstrapIPs:              bootstrapIPs,
		BootstrapIDs:              bootstrapIDs,
	}
	configPath := filepath.Join(sourceDir, "config.json")
	if err := writeJSON(configPath, cfg); err != nil {
		return err
	}

	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("determine service user: %w", err)
	}
	unitPath := filepath.Join(sourceDir, serviceName)
	unit := renderUnit(currentUser.Username, binaryPath, configPath)
	if err := os.WriteFile(unitPath, []byte(unit), 0o600); err != nil {
		return fmt.Errorf("write systemd unit %s: %w", unitPath, err)
	}
	if _, err := m.run(ctx, "sudo", "install", "-m", "0644", unitPath, filepath.Join("/etc/systemd/system", serviceName)); err != nil {
		return err
	}
	if _, err := m.run(ctx, "sudo", "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if _, err := m.run(ctx, "sudo", "systemctl", "enable", serviceName); err != nil {
		return err
	}
	_, err = m.run(ctx, "sudo", "systemctl", "restart", serviceName)
	return err
}

func (m *Manager) waitForAPIAndPrint(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := m.fetchLocalStatus(ctx); err == nil {
			return m.printStatus(ctx)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("source restarted but its API did not become ready within 30s")
		}
		m.sleep(time.Second)
	}
}

type localStatus struct {
	NodeID      string
	Height      uint64
	HeightReady bool
	PeerNodeIDs []string
}

func (m *Manager) printStatus(ctx context.Context) error {
	cfg, err := m.loadConfig()
	if err != nil {
		return err
	}
	local, err := m.fetchLocalStatus(ctx)
	if err != nil {
		return err
	}
	publicHeight, err := m.fetchPublicHeight(ctx)
	if err != nil {
		return err
	}
	mode := "frozen"
	if cfg.BootstrapIPs == nil {
		mode = "following"
	}
	fmt.Fprintf(m.out, "mode: %s\n", mode)
	fmt.Fprintf(m.out, "source NodeID: %s\n", local.NodeID)
	if mode == "following" {
		fmt.Fprintf(m.out, "upstream: AvalancheGo's built-in %s bootstrap peers\n", m.network)
	}
	if !local.HeightReady {
		fmt.Fprintln(m.out, "P-chain height: initializing")
		fmt.Fprintf(m.out, "public P-chain height: %d\n", publicHeight)
	} else if publicHeight >= local.Height {
		fmt.Fprintf(m.out, "P-chain height: %d\n", local.Height)
		fmt.Fprintf(m.out, "public P-chain height: %d, lag=%d\n", publicHeight, publicHeight-local.Height)
	} else {
		fmt.Fprintf(m.out, "P-chain height: %d\n", local.Height)
		fmt.Fprintf(m.out, "public P-chain height: %d, source ahead by %d\n", publicHeight, local.Height-publicHeight)
	}
	fmt.Fprintf(m.out, "peers: %d\n", len(local.PeerNodeIDs))
	return nil
}

func (m *Manager) loadConfig() (nodeConfig, error) {
	path := filepath.Join(m.root, "data", "pchain-source", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nodeConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg nodeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nodeConfig{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if cfg.NetworkID != m.network {
		return nodeConfig{}, fmt.Errorf("%s network is %q, .env NETWORK is %q", path, cfg.NetworkID, m.network)
	}
	if (cfg.BootstrapIPs == nil) != (cfg.BootstrapIDs == nil) {
		return nodeConfig{}, fmt.Errorf("%s has only one bootstrap field", path)
	}
	if cfg.BootstrapIPs != nil && (*cfg.BootstrapIPs != "" || *cfg.BootstrapIDs != "") {
		return nodeConfig{}, fmt.Errorf("%s has unsupported explicit bootstrap peers; rerun fleet pchain follow to use AvalancheGo defaults", path)
	}
	return cfg, nil
}

func (m *Manager) fetchLocalStatus(ctx context.Context) (localStatus, error) {
	var nodeResult struct {
		NodeID string `json:"nodeID"`
	}
	if err := m.rpc(ctx, localAPI, "info.getNodeID", map[string]any{}, &nodeResult); err != nil {
		return localStatus{}, fmt.Errorf("read source NodeID: %w", err)
	}
	var peersResult struct {
		Peers []struct {
			NodeID string `json:"nodeID"`
		} `json:"peers"`
	}
	if err := m.rpc(ctx, localAPI, "info.peers", map[string]any{}, &peersResult); err != nil {
		return localStatus{}, fmt.Errorf("read source peers: %w", err)
	}
	accepted, found, err := m.fetchMetric(ctx, localAPI+"/ext/metrics", `avalanche_snowman_bs_accepted{chain="P"}`)
	if err != nil {
		return localStatus{}, err
	}
	// Follow-only intentionally remains in bootstrap and rejects platform.getHeight.
	// AvalancheGo logs the database height at process start and exposes every block
	// accepted afterward as this process's bootstrap counter. Their sum is the
	// current height without inventing another persisted height that can drift.
	startHeight, startFound, err := m.fetchStartHeight(ctx)
	if err != nil {
		return localStatus{}, err
	}
	status := localStatus{
		NodeID:      nodeResult.NodeID,
		Height:      startHeight + uint64(accepted),
		HeightReady: found && startFound,
	}
	for _, peer := range peersResult.Peers {
		status.PeerNodeIDs = append(status.PeerNodeIDs, peer.NodeID)
	}
	return status, nil
}

func (m *Manager) fetchStartHeight(ctx context.Context) (uint64, bool, error) {
	output, err := m.run(
		ctx,
		"journalctl",
		"--unit", serviceName,
		"--boot",
		"--output", "cat",
		"--no-pager",
		"--grep", "starting bootstrapper",
		"--lines", "1",
	)
	if err != nil {
		return 0, false, err
	}
	line := strings.TrimSpace(string(output))
	objectStart := strings.LastIndexByte(line, '{')
	if objectStart < 0 {
		return 0, false, nil
	}
	var fields struct {
		LastAcceptedHeight uint64 `json:"lastAcceptedHeight"`
	}
	if err := json.Unmarshal([]byte(line[objectStart:]), &fields); err != nil {
		return 0, false, fmt.Errorf("decode P-chain startup height from journal: %w", err)
	}
	return fields.LastAcceptedHeight, true, nil
}

func (m *Manager) fetchPublicHeight(ctx context.Context) (uint64, error) {
	var result struct {
		Height string `json:"height"`
	}
	if err := m.rpc(ctx, m.publicAPI, "platform.getHeight", map[string]any{}, &result); err != nil {
		return 0, fmt.Errorf("read public P-chain height from %s: %w", m.publicAPI, err)
	}
	height, err := strconv.ParseUint(result.Height, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decode public P-chain height %q: %w", result.Height, err)
	}
	return height, nil
}

func (m *Manager) fetchMetric(ctx context.Context, endpoint, name string) (float64, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("read source metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("read source metrics: HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, false, fmt.Errorf("read source metrics body: %w", err)
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == name {
			value, err := strconv.ParseFloat(fields[1], 64)
			return value, true, err
		}
	}
	return 0, false, nil
}

func (m *Manager) rpc(ctx context.Context, baseURL, method string, params any, result any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/ext/info"
	if strings.HasPrefix(method, "platform.") {
		endpoint = strings.TrimRight(baseURL, "/") + "/ext/bc/P"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if envelope.Error != nil {
		return fmt.Errorf("RPC %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if len(envelope.Result) == 0 {
		return errors.New("RPC response has no result")
	}
	return json.Unmarshal(envelope.Result, result)
}

func (m *Manager) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := m.runner.Run(ctx, name, args...)
	if err != nil {
		return output, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func renderUnit(username, binaryPath, configPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Avalanche benchmark P-chain source
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
ExecStart=%s --config-file=%s
Restart=on-failure
RestartSec=2
LimitNOFILE=32768

[Install]
WantedBy=multi-user.target
`, username, systemdQuote(binaryPath), systemdQuote(configPath))
}

func systemdQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".config-")
	if err != nil {
		return fmt.Errorf("create temporary P-chain config: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install P-chain config %s: %w", path, err)
	}
	return nil
}
