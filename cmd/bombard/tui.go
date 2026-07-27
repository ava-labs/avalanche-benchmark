package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/guptarohit/asciigraph"
	"golang.org/x/term"
)

const (
	tuiHistorySeconds = 24 * 60 * 60

	ansiClearScreen = "\x1b[H\x1b[2J"
	ansiHideCursor  = "\x1b[?25l"
	ansiShowCursor  = "\x1b[?25h"
)

type statsSnapshot struct {
	at time.Time

	targetRPS int
	cap       int

	issued    uint64
	mined     uint64
	inflight  uint64
	resubmits uint64
	minedTPS  float64
	atCap     bool

	latencySamples int
	p50            time.Duration
	p95            time.Duration
	p99            time.Duration
}

type terminalStatsUI struct {
	out       io.Writer
	startedAt time.Time
	history   []float64
}

func stdoutIsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func restoreTerminal() {
	if stdoutIsTerminal() {
		fmt.Fprint(os.Stdout, ansiShowCursor)
	}
}

func newTerminalStatsUI(out io.Writer) *terminalStatsUI {
	fmt.Fprint(out, ansiHideCursor)
	return &terminalStatsUI{
		out:       out,
		startedAt: time.Now(),
		history:   make([]float64, 0, tuiHistorySeconds),
	}
}

func (ui *terminalStatsUI) close() {
	fmt.Fprintln(ui.out, ansiShowCursor)
}

func (ui *terminalStatsUI) render(s statsSnapshot) {
	ui.history = append(ui.history, s.minedTPS)
	if len(ui.history) > tuiHistorySeconds {
		ui.history = ui.history[len(ui.history)-tuiHistorySeconds:]
	}

	width, height := terminalSize()
	chartHeight := height - 9
	if chartHeight < 4 {
		chartHeight = 4
	}
	chartWidth := width
	if chartWidth < 2 {
		chartWidth = 2
	}
	chart := renderTPSChart(ui.history, chartWidth, chartHeight)

	elapsed := s.at.Sub(ui.startedAt).Round(time.Second)
	if elapsed < 0 {
		elapsed = 0
	}

	status := "status=tracking"
	if s.atCap {
		status = "status=at-cap"
	}

	var b strings.Builder
	b.WriteString(ansiClearScreen)
	fmt.Fprintf(&b, "Bombard  target=%d rps  elapsed=%s  %s\n\n", s.targetRPS, elapsed, status)
	b.WriteString(chart)
	b.WriteByte('\n')
	b.WriteByte('\n')
	fmt.Fprintf(&b, "issued=%d  mined=%d  inflight=%d/%d  resubmits=%d  minedTps=%.0f/%d\n",
		s.issued, s.mined, s.inflight, s.cap, s.resubmits, s.minedTPS, s.targetRPS)
	if s.latencySamples > 0 {
		fmt.Fprintf(&b, "p50=%s  p95=%s  samples=%d",
			formatLatency(s.p50), formatLatency(s.p95), s.latencySamples)
	} else {
		b.WriteString("p50=-  p95=-  samples=0")
	}

	rendered := trimToHeight(b.String(), height)
	fmt.Fprint(ui.out, rendered)
}

func printStats(s statsSnapshot) {
	behind := ""
	if s.atCap {
		behind = " AT-CAP(behind)"
	}

	if s.latencySamples > 0 {
		fmt.Printf("STATS issued=%d mined=%d inflight=%d/%d resubmits=%d minedTps=%.0f/%d%s | total p50=%v p95=%v p99=%v\n",
			s.issued, s.mined, s.inflight, s.cap, s.resubmits, s.minedTPS, s.targetRPS, behind,
			s.p50.Round(time.Millisecond), s.p95.Round(time.Millisecond), s.p99.Round(time.Millisecond))
		return
	}

	fmt.Printf("STATS issued=%d mined=%d inflight=%d/%d resubmits=%d minedTps=%.0f/%d%s | no landings this tick\n",
		s.issued, s.mined, s.inflight, s.cap, s.resubmits, s.minedTPS, s.targetRPS, behind)
}

func terminalSize() (int, int) {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 || height <= 0 {
		return 100, 30
	}
	return width, height
}

func latestSeries(history []float64, max int) []float64 {
	if max <= 0 || len(history) <= max {
		return history
	}
	return history[len(history)-max:]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func renderTPSChart(history []float64, width, height int) string {
	maxPoints := width
	for points := maxPoints; points >= 2; points-- {
		chart := asciigraph.Plot(
			zeroPaddedLatestSeries(history, points),
			asciigraph.Height(height),
			asciigraph.LowerBound(0),
			asciigraph.Caption("mined transactions per second"),
			asciigraph.Precision(0),
		)
		if maxLineWidth(chart) <= width {
			return rightAlignLines(chart, width)
		}
	}

	return rightAlignLines("not enough room for TPS chart", width)
}

func zeroPaddedLatestSeries(history []float64, points int) []float64 {
	if points <= 0 {
		return nil
	}
	series := make([]float64, points)
	if len(history) == 0 {
		return series
	}

	src := latestSeries(history, points)
	copy(series[points-len(src):], src)
	return series
}

func rightAlignLines(s string, width int) string {
	if width <= 0 {
		return s
	}
	pad := width - maxLineWidth(s)
	if pad <= 0 {
		return s
	}
	prefix := strings.Repeat(" ", pad)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func maxLineWidth(s string) int {
	max := 0
	for _, line := range strings.Split(s, "\n") {
		if w := utf8.RuneCountInString(line); w > max {
			max = w
		}
	}
	return max
}

func formatLatency(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(10 * time.Millisecond).String()
}

func trimToHeight(s string, height int) string {
	if height <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= height {
		return s
	}

	return strings.Join(lines[:height], "\n")
}
