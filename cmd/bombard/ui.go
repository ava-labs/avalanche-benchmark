package main

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type tickMsg time.Time

type uiModel struct {
	ctx    context.Context
	cancel context.CancelFunc
	run    *benchmarkRun

	width  int
	height int

	lastAt     time.Time
	lastLanded uint64
	actualTPS  int

	p50MS int
	p95MS int

	latestBlock    uint64
	hasLatestBlock bool
}

func runTUI(ctx context.Context, cancel context.CancelFunc, run *benchmarkRun) error {
	model := uiModel{
		ctx:    ctx,
		cancel: cancel,
		run:    run,
		lastAt: time.Now(),
	}
	_, err := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

func (m uiModel) Init() tea.Cmd {
	return tick()
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.cancel()
			return m, tea.Quit
		case "+", "=":
			current := m.run.target.get()
			m.run.target.adjust(tpsStep(current))
			return m.updateStats(), nil
		case "-", "_":
			current := m.run.target.get()
			m.run.target.adjust(-tpsStep(current))
			return m.updateStats(), nil
		}
	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			switch {
			case msg.Y == 1 && msg.X >= 28 && msg.X <= 32:
				current := m.run.target.get()
				m.run.target.adjust(-tpsStep(current))
				return m.updateStats(), nil
			case msg.Y == 1 && msg.X >= 34 && msg.X <= 38:
				current := m.run.target.get()
				m.run.target.adjust(tpsStep(current))
				return m.updateStats(), nil
			}
		}
	case tickMsg:
		if m.ctx.Err() != nil {
			return m, tea.Quit
		}
		return m.updateStats(), tick()
	}
	return m, nil
}

func (m uiModel) updateStats() uiModel {
	now := time.Now()
	_, landed, _, _ := tracker.counts()
	elapsed := now.Sub(m.lastAt).Seconds()
	if elapsed > 0 {
		m.actualTPS = int(float64(landed-m.lastLanded) / elapsed)
	}
	m.lastAt = now
	m.lastLanded = landed

	m.p50MS, m.p95MS = currentLatencyPercentilesMS()
	m.latestBlock, m.hasLatestBlock = tracker.latestBlock()
	return m
}

func currentLatencyPercentilesMS() (int, int) {
	samples, _ := tracker.snapshotLatencyWindow(time.Now())
	if len(samples) == 0 {
		return 0, 0
	}
	totals := make([]time.Duration, len(samples))
	for i, sample := range samples {
		totals[i] = sample.total
	}
	sort.Slice(totals, func(i, j int) bool { return totals[i] < totals[j] })
	return int(pctDur(totals, 50).Milliseconds()), int(pctDur(totals, 95).Milliseconds())
}

func (m uiModel) View() string {
	submitted, landed, timeouts, pending := tracker.counts()
	statuses, failovers := m.run.endpoints.statuses()

	var b strings.Builder
	fmt.Fprintf(&b, "TPS: actual %-6d target %-6d [-] [+]\n", m.actualTPS, m.run.target.get())
	if m.hasLatestBlock {
		fmt.Fprintf(&b, "Block: latest %-10d\n", m.latestBlock)
	} else {
		fmt.Fprintf(&b, "Block: latest -\n")
	}
	fmt.Fprintf(&b, "Latency(10s): P50 %-6d ms  P95 %-6d ms\n", m.p50MS, m.p95MS)
	fmt.Fprintf(&b, "Counts: sub %-7d land %-7d to %-5d pend %-5d fail %-3d\n\n",
		submitted, landed, timeouts, pending, failovers)

	for i, status := range statuses {
		cursor := " "
		active := "      "
		if status.Active {
			cursor = ">"
			active = "active"
		}
		health := "DOWN"
		if status.Alive {
			health = "alive"
		}
		fmt.Fprintf(&b, "%s node %-2d %-24s %-6s %s\n", cursor, i+1, hostPort(status.URL), active, health)
	}

	fmt.Fprintf(&b, "+/- adjust target TPS   q or Ctrl-C exits")
	return b.String()
}

func hostPort(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return rawURL
	}
	return parsed.Host
}
