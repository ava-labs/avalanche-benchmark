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
	out        io.Writer
	startedAt  time.Time
	history    []float64 // mined TPS per tick
	p50History []float64 // p50 mine latency (ms) per tick
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
		out:        out,
		startedAt:  time.Now(),
		history:    make([]float64, 0, tuiHistorySeconds),
		p50History: make([]float64, 0, tuiHistorySeconds),
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

	// p50 in ms; carry forward the last value on ticks with no landings so the
	// latency line holds steady instead of dropping to a misleading 0.
	p50ms := float64(s.p50) / float64(time.Millisecond)
	if s.latencySamples == 0 && len(ui.p50History) > 0 {
		p50ms = ui.p50History[len(ui.p50History)-1]
	}
	ui.p50History = append(ui.p50History, p50ms)
	if len(ui.p50History) > tuiHistorySeconds {
		ui.p50History = ui.p50History[len(ui.p50History)-tuiHistorySeconds:]
	}

	width, height := terminalSize()
	chartWidth := width
	if chartWidth < 2 {
		chartWidth = 2
	}
	// Two charts share the vertical space; ~12 lines reserved for header,
	// both captions, spacing, and the two stats lines.
	chartArea := height - 12
	if chartArea < 8 {
		chartArea = 8
	}
	topH := chartArea / 2
	botH := chartArea - topH
	tpsChart := renderLineChart(ui.history, chartWidth, topH, "mined transactions per second")
	p50Chart := renderLineChart(ui.p50History, chartWidth, botH, "p50 mine latency (ms)")

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
	b.WriteString(tpsChart)
	b.WriteByte('\n')
	b.WriteByte('\n')
	b.WriteString(p50Chart)
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

func renderLineChart(history []float64, width, height int, caption string) string {
	maxPoints := width
	for points := maxPoints; points >= 2; points-- {
		chart := asciigraph.Plot(
			zeroPaddedLatestSeries(history, points),
			asciigraph.Height(height),
			asciigraph.LowerBound(0),
			asciigraph.Caption(caption),
			asciigraph.Precision(0),
		)
		chart = moveAxisLabelsRight(chart)
		if maxLineWidth(chart) <= width {
			return rightAlignLines(chart, width)
		}
	}

	return rightAlignLines("not enough room for chart", width)
}

// moveAxisLabelsRight flips asciigraph's Y-axis gutter to the right. asciigraph
// prints each plot row as "<right-justified label><axis rune><plot>" with the
// axis rune (┤, or ┼ at the origin) at a single fixed column and nowhere else.
// This rewrites those rows to "<plot><axis rune><label>" so the numbers read
// down the right edge. Rows without an axis rune (caption, X-axis) pass through.
func moveAxisLabelsRight(chart string) string {
	lines := strings.Split(chart, "\n")
	type row struct {
		i, axis int
		runes   []rune
	}
	// asciigraph right-trims each row, so plot rows have unequal lengths. Pad
	// every plot to the widest before appending the gutter, else the axis
	// column and labels come out ragged.
	var rows []row
	maxPlot := 0
	for i, line := range lines {
		runes := []rune(line)
		axis := -1
		for j, r := range runes {
			if r == '┤' || r == '┼' {
				axis = j
				break
			}
		}
		if axis < 0 {
			continue
		}
		rows = append(rows, row{i, axis, runes})
		if n := len(runes) - axis - 1; n > maxPlot {
			maxPlot = n
		}
	}
	for _, rw := range rows {
		plot := string(rw.runes[rw.axis+1:])
		if pad := maxPlot - (len(rw.runes) - rw.axis - 1); pad > 0 {
			plot += strings.Repeat(" ", pad)
		}
		lines[rw.i] = plot + string(rw.runes[rw.axis]) + string(rw.runes[:rw.axis])
	}
	return strings.Join(lines, "\n")
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
