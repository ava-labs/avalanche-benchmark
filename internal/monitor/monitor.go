package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	refreshInterval = 500 * time.Millisecond // Faster updates for smoother feel
	maxDataPoints   = 60                      // 30 seconds of history at 500ms
)

// Shared HTTP client
var httpClient = &http.Client{
	Timeout: 2 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     30 * time.Second,
	},
}

// Styles - minimal and clean
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212")) // Pink/magenta

	bigNumberStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("86")) // Cyan

	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")) // Gray

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252")) // Light gray

	goodStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("78")) // Green

	warnStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")) // Orange

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("238")) // Dark gray

	graphStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("39")) // Blue
)

// NodeHealth contains health info for a single node
type NodeHealth struct {
	URI            string
	Label          string // "V1", "V2", "R1" etc
	Healthy        bool
	Reachable      bool
	L1Healthy      bool
	L1Height       uint64
	DiskPercent    int
	DiskOK         bool
	ConnectedPeers int
	FailedChecks   []string
}

// Config holds monitor configuration
type Config struct {
	NodeURI       string   // Primary node for block queries
	ChainID       string   // L1 chain ID
	ValidatorURIs []string // L1 validator node URIs
	RPCNodeURIs   []string // L1 RPC node URIs
}

type Model struct {
	config Config
	rpcURL string

	// Core metrics
	blockHeight uint64
	txCount     int
	gasUsed     uint64
	gasLimit    uint64

	// Node health
	nodeHealth []NodeHealth

	// TPS tracking
	tpsHistory []float64
	currentTPS float64
	avgTPS     float64
	peakTPS    float64

	// Block time tracking
	blockTimeHistory []float64
	lastBlockTime    time.Time
	avgBlockTime     float64

	// Timing
	lastBlockNum uint64
	startTime    time.Time

	err error
}

type tickMsg time.Time
type dataMsg struct {
	blockNum   uint64
	txCount    int
	gasUsed    uint64
	gasLimit   uint64
	nodeHealth []NodeHealth
}
type errMsg error

func Run(ctx context.Context, cfg Config) error {
	rpcURL := fmt.Sprintf("%s/ext/bc/%s/rpc", cfg.NodeURI, cfg.ChainID)

	m := Model{
		config:           cfg,
		rpcURL:           rpcURL,
		tpsHistory:       make([]float64, 0, maxDataPoints),
		blockTimeHistory: make([]float64, 0, maxDataPoints),
		startTime:        time.Now(),
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	if err != nil && strings.Contains(err.Error(), "TTY") {
		return runSimple(ctx, cfg)
	}
	return err
}

func runSimple(ctx context.Context, cfg Config) error {
	rpcURL := fmt.Sprintf("%s/ext/bc/%s/rpc", cfg.NodeURI, cfg.ChainID)

	fmt.Println("Benchmark Monitor (simple mode)")
	fmt.Println(strings.Repeat("─", 60))

	var lastBlock uint64
	var lastTime time.Time
	var tpsHistory []float64

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			block, txCount, _, _, _ := getBlock(rpcURL)
			nodeHealth := checkAllNodesHealth(cfg)

			var tps float64
			if lastBlock > 0 && block > lastBlock {
				elapsed := time.Since(lastTime).Seconds()
				if elapsed > 0 {
					tps = float64(txCount) / elapsed
					tpsHistory = append(tpsHistory, tps)
					if len(tpsHistory) > 10 {
						tpsHistory = tpsHistory[1:]
					}
				}
			}

			if block > lastBlock {
				lastBlock = block
				lastTime = time.Now()
			}

			var avg float64
			for _, t := range tpsHistory {
				avg += t
			}
			if len(tpsHistory) > 0 {
				avg /= float64(len(tpsHistory))
			}

			// Build status string
			var statusParts []string
			for _, nh := range nodeHealth {
				symbol := "●"
				if !nh.Reachable {
					symbol = "✗"
				} else if !nh.L1Healthy {
					symbol = "○"
				}
				statusParts = append(statusParts, fmt.Sprintf("%s%s", symbol, nh.Label))
			}

			fmt.Printf("[%s] Block %d | TPS: %.0f | Avg: %.0f\n",
				strings.Join(statusParts, " "), block, tps, avg)
		}
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(tick(), fetch(m.rpcURL, m.config))
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "q" || msg.String() == "ctrl+c" {
			return m, tea.Quit
		}

	case tickMsg:
		return m, tea.Batch(tick(), fetch(m.rpcURL, m.config))

	case dataMsg:
		m.err = nil
		now := time.Now()

		// Calculate TPS and block time when we see a new block
		if m.lastBlockNum > 0 && msg.blockNum > m.lastBlockNum {
			elapsed := now.Sub(m.lastBlockTime).Seconds()
			if elapsed > 0 {
				// TPS calculation
				tps := float64(msg.txCount) / elapsed
				m.currentTPS = tps
				m.tpsHistory = append(m.tpsHistory, tps)
				if len(m.tpsHistory) > maxDataPoints {
					m.tpsHistory = m.tpsHistory[1:]
				}

				if tps > m.peakTPS {
					m.peakTPS = tps
				}

				var tpsSum float64
				for _, t := range m.tpsHistory {
					tpsSum += t
				}
				m.avgTPS = tpsSum / float64(len(m.tpsHistory))

				// Block time calculation (ms per block)
				blockCount := msg.blockNum - m.lastBlockNum
				blockTimeMs := (elapsed * 1000) / float64(blockCount)
				m.blockTimeHistory = append(m.blockTimeHistory, blockTimeMs)
				if len(m.blockTimeHistory) > maxDataPoints {
					m.blockTimeHistory = m.blockTimeHistory[1:]
				}

				var btSum float64
				for _, bt := range m.blockTimeHistory {
					btSum += bt
				}
				m.avgBlockTime = btSum / float64(len(m.blockTimeHistory))
			}
		}

		// Update block tracking
		if msg.blockNum > m.lastBlockNum {
			m.lastBlockNum = msg.blockNum
			m.lastBlockTime = now
		}

		m.blockHeight = msg.blockNum
		m.txCount = msg.txCount
		m.gasUsed = msg.gasUsed
		m.gasLimit = msg.gasLimit
		m.nodeHealth = msg.nodeHealth

	case errMsg:
		m.err = msg
	}

	return m, nil
}

func (m Model) View() string {
	var b strings.Builder

	// Header
	b.WriteString("\n")
	b.WriteString(titleStyle.Render("  ▲ AVALANCHE BENCHMARK"))
	b.WriteString("\n\n")

	// Big TPS display
	tpsStr := fmt.Sprintf("%.0f", m.currentTPS)
	b.WriteString("  ")
	b.WriteString(bigNumberStyle.Render(tpsStr))
	b.WriteString(" ")
	b.WriteString(labelStyle.Render("TPS"))
	b.WriteString("\n\n")

	// Stats line
	blockTimeStr := "---"
	blockTimeStyle := dimStyle
	if m.avgBlockTime > 0 {
		blockTimeStr = fmt.Sprintf("%.0fms", m.avgBlockTime)
		if m.avgBlockTime < 1500 {
			blockTimeStyle = goodStyle
		} else if m.avgBlockTime < 3000 {
			blockTimeStyle = warnStyle
		}
	}

	b.WriteString(fmt.Sprintf("  %s %s    %s %s    %s %s    %s %s\n",
		labelStyle.Render("avg"),
		valueStyle.Render(fmt.Sprintf("%.0f", m.avgTPS)),
		labelStyle.Render("peak"),
		goodStyle.Render(fmt.Sprintf("%.0f", m.peakTPS)),
		labelStyle.Render("block"),
		valueStyle.Render(fmt.Sprintf("%d", m.blockHeight)),
		labelStyle.Render("rate"),
		blockTimeStyle.Render(blockTimeStr),
	))

	// Gas utilization bar
	gasPercent := float64(0)
	if m.gasLimit > 0 {
		gasPercent = float64(m.gasUsed) / float64(m.gasLimit) * 100
	}
	b.WriteString(fmt.Sprintf("  %s %s\n",
		labelStyle.Render("gas"),
		renderBar(gasPercent, 20),
	))
	b.WriteString("\n")

	// Graph
	b.WriteString(m.renderGraph())
	b.WriteString("\n")

	// Node health section
	b.WriteString(labelStyle.Render("  NODES"))
	b.WriteString("\n")

	for _, nh := range m.nodeHealth {
		b.WriteString(m.renderNodeHealth(nh))
	}
	b.WriteString("\n")

	// Uptime
	elapsed := time.Since(m.startTime).Round(time.Second)
	b.WriteString(fmt.Sprintf("  %s %s\n",
		labelStyle.Render("uptime"),
		valueStyle.Render(elapsed.String()),
	))

	// Error
	if m.err != nil {
		b.WriteString(fmt.Sprintf("\n  %s\n", warnStyle.Render(m.err.Error())))
	}

	// Help
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("  q to quit"))
	b.WriteString("\n")

	return b.String()
}

func (m Model) renderNodeHealth(nh NodeHealth) string {
	var b strings.Builder

	// Status symbol
	symbol := goodStyle.Render("●")
	if !nh.Reachable {
		symbol = warnStyle.Render("✗")
	} else if !nh.L1Healthy {
		symbol = warnStyle.Render("○")
	}

	// Label with padding
	label := fmt.Sprintf("%-3s", nh.Label)

	// Build status details
	var details []string

	if !nh.Reachable {
		details = append(details, warnStyle.Render("unreachable"))
	} else {
		// L1 height
		if nh.L1Height > 0 {
			details = append(details, fmt.Sprintf("h:%d", nh.L1Height))
		}

		// Disk status
		if !nh.DiskOK {
			details = append(details, warnStyle.Render(fmt.Sprintf("disk:%d%%", nh.DiskPercent)))
		}

		// Failed checks (show first one if any)
		if len(nh.FailedChecks) > 0 && nh.L1Healthy {
			// Only show if L1 is healthy but other checks failed
			details = append(details, warnStyle.Render(nh.FailedChecks[0]))
		}
	}

	b.WriteString(fmt.Sprintf("  %s %s  %s\n",
		symbol,
		labelStyle.Render(label),
		dimStyle.Render(strings.Join(details, "  ")),
	))

	return b.String()
}

func (m Model) renderGraph() string {
	const width = 50
	const height = 8

	var b strings.Builder

	if len(m.tpsHistory) == 0 {
		// Empty graph placeholder
		for i := 0; i < height; i++ {
			b.WriteString("  ")
			b.WriteString(dimStyle.Render("│"))
			b.WriteString(strings.Repeat(" ", width))
			b.WriteString("\n")
		}
		b.WriteString("  ")
		b.WriteString(dimStyle.Render("└" + strings.Repeat("─", width)))
		b.WriteString("\n")
		return b.String()
	}

	// Find range
	maxVal := m.tpsHistory[0]
	for _, v := range m.tpsHistory {
		if v > maxVal {
			maxVal = v
		}
	}
	if maxVal == 0 {
		maxVal = 1
	}

	// Resample data to fit width
	data := resample(m.tpsHistory, width)

	// Braille-style blocks for smooth rendering
	blocks := []rune{' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

	// Render rows (top to bottom)
	for row := height - 1; row >= 0; row-- {
		b.WriteString("  ")
		b.WriteString(dimStyle.Render("│"))

		var line strings.Builder
		for _, v := range data {
			normalized := v / maxVal
			cellHeight := normalized * float64(height)
			cellFill := cellHeight - float64(row)

			if cellFill <= 0 {
				line.WriteRune(' ')
			} else if cellFill >= 1 {
				line.WriteRune(blocks[len(blocks)-1])
			} else {
				idx := int(cellFill * float64(len(blocks)-1))
				line.WriteRune(blocks[idx])
			}
		}
		b.WriteString(graphStyle.Render(line.String()))
		b.WriteString("\n")
	}

	// X-axis
	b.WriteString("  ")
	b.WriteString(dimStyle.Render("└" + strings.Repeat("─", width)))
	b.WriteString("\n")

	// Labels
	b.WriteString(fmt.Sprintf("  %s%s%s\n",
		dimStyle.Render("30s ago"),
		strings.Repeat(" ", width-14),
		dimStyle.Render("now"),
	))

	return b.String()
}

func renderBar(percent float64, width int) string {
	filled := int(percent / 100 * float64(width))
	if filled > width {
		filled = width
	}

	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)

	style := valueStyle
	if percent > 80 {
		style = goodStyle
	} else if percent < 20 {
		style = dimStyle
	}

	return style.Render(bar) + labelStyle.Render(fmt.Sprintf(" %.0f%%", percent))
}

func resample(data []float64, width int) []float64 {
	if len(data) == 0 {
		return make([]float64, width)
	}

	result := make([]float64, width)

	if len(data) <= width {
		// Pad left with zeros, data on right
		offset := width - len(data)
		copy(result[offset:], data)
	} else {
		// Downsample
		ratio := float64(len(data)) / float64(width)
		for i := 0; i < width; i++ {
			idx := int(float64(i) * ratio)
			if idx >= len(data) {
				idx = len(data) - 1
			}
			result[i] = data[idx]
		}
	}

	return result
}

// Commands
func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func fetch(rpcURL string, cfg Config) tea.Cmd {
	return func() tea.Msg {
		block, txCount, gasUsed, gasLimit, err := getBlock(rpcURL)
		if err != nil {
			return errMsg(err)
		}

		nodeHealth := checkAllNodesHealth(cfg)

		return dataMsg{
			blockNum:   block,
			txCount:    txCount,
			gasUsed:    gasUsed,
			gasLimit:   gasLimit,
			nodeHealth: nodeHealth,
		}
	}
}

func checkAllNodesHealth(cfg Config) []NodeHealth {
	var nodes []NodeHealth

	// Add validators
	for i, uri := range cfg.ValidatorURIs {
		nodes = append(nodes, NodeHealth{
			URI:   uri,
			Label: fmt.Sprintf("V%d", i+1),
		})
	}

	// Add RPC nodes
	for i, uri := range cfg.RPCNodeURIs {
		nodes = append(nodes, NodeHealth{
			URI:   uri,
			Label: fmt.Sprintf("R%d", i+1),
		})
	}

	// Check all nodes in parallel
	var wg sync.WaitGroup
	for i := range nodes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			nodes[idx] = checkNodeHealth(nodes[idx].URI, nodes[idx].Label, cfg.ChainID)
		}(i)
	}
	wg.Wait()

	return nodes
}

func checkNodeHealth(nodeURI, label, chainID string) NodeHealth {
	nh := NodeHealth{
		URI:       nodeURI,
		Label:     label,
		Reachable: true,
	}

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "health.health",
		"params":  map[string]interface{}{},
		"id":      1,
	}

	body, _ := json.Marshal(req)
	resp, err := httpClient.Post(nodeURI+"/ext/health", "application/json", strings.NewReader(string(body)))
	if err != nil {
		nh.Reachable = false
		return nh
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		nh.Reachable = false
		return nh
	}

	var result struct {
		Result struct {
			Healthy bool                   `json:"healthy"`
			Checks  map[string]interface{} `json:"checks"`
		} `json:"result"`
	}

	if err := json.Unmarshal(data, &result); err != nil {
		nh.Reachable = false
		return nh
	}

	nh.Healthy = result.Result.Healthy

	// Parse individual checks
	for name, checkData := range result.Result.Checks {
		checkMap, ok := checkData.(map[string]interface{})
		if !ok {
			continue
		}

		// Check for errors in this check
		if errMsg, hasError := checkMap["error"]; hasError && errMsg != nil {
			nh.FailedChecks = append(nh.FailedChecks, name)
		}

		// Parse specific checks
		switch name {
		case "diskspace":
			if msg, ok := checkMap["message"].(map[string]interface{}); ok {
				if pct, ok := msg["availableDiskPercentage"].(float64); ok {
					nh.DiskPercent = int(pct)
					nh.DiskOK = pct >= 10 // Consider OK if >= 10%
				}
			}
			if _, hasError := checkMap["error"]; hasError {
				nh.DiskOK = false
			}

		case "network":
			if msg, ok := checkMap["message"].(map[string]interface{}); ok {
				if peers, ok := msg["connectedPeers"].(float64); ok {
					nh.ConnectedPeers = int(peers)
				}
			}

		default:
			// Check if this is the L1 chain (chainID matches)
			if name == chainID {
				nh.L1Healthy = true // Present means it's being tracked
				if msg, ok := checkMap["message"].(map[string]interface{}); ok {
					if engine, ok := msg["engine"].(map[string]interface{}); ok {
						if consensus, ok := engine["consensus"].(map[string]interface{}); ok {
							if height, ok := consensus["lastAcceptedHeight"].(float64); ok {
								nh.L1Height = uint64(height)
							}
						}
					}
				}
				// Check if L1 has errors
				if _, hasError := checkMap["error"]; hasError {
					nh.L1Healthy = false
				}
			}
		}
	}

	return nh
}

// RPC helpers
type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      int           `json:"id"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type blockData struct {
	Number       string   `json:"number"`
	Transactions []string `json:"transactions"`
	GasUsed      string   `json:"gasUsed"`
	GasLimit     string   `json:"gasLimit"`
}

func getBlock(rpcURL string) (num uint64, txCount int, gasUsed, gasLimit uint64, err error) {
	req := rpcRequest{
		JSONRPC: "2.0",
		Method:  "eth_getBlockByNumber",
		Params:  []interface{}{"latest", false},
		ID:      1,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return
	}

	resp, err := httpClient.Post(rpcURL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	var rpcResp rpcResponse
	if err = json.Unmarshal(data, &rpcResp); err != nil {
		return
	}

	if rpcResp.Error != nil {
		err = fmt.Errorf("%s", rpcResp.Error.Message)
		return
	}

	var block blockData
	if err = json.Unmarshal(rpcResp.Result, &block); err != nil {
		return
	}

	fmt.Sscanf(block.Number, "0x%x", &num)
	fmt.Sscanf(block.GasUsed, "0x%x", &gasUsed)
	fmt.Sscanf(block.GasLimit, "0x%x", &gasLimit)
	txCount = len(block.Transactions)

	return
}
