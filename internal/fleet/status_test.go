package fleet

import (
	"strings"
	"testing"
)

func TestCollapseServiceState(t *testing.T) {
	cases := []struct {
		present bool
		active  string
		enabled string
		want    string
	}{
		{false, "inactive", "", statusNotInstalled},
		{true, "active", "enabled", statusUp},
		{true, "activating", "enabled", statusUp},
		{true, "failed", "enabled", statusFailed},
		{true, "inactive", "disabled", statusDown},
		{true, "inactive", "enabled", statusDown},
	}
	for _, test := range cases {
		if got := collapseServiceState(test.present, test.active, test.enabled); got != test.want {
			t.Errorf("collapseServiceState(%v, %q, %q) = %q, want %q", test.present, test.active, test.enabled, got, test.want)
		}
	}
}

func TestRenderStatusTableKeepsPlaceholderCells(t *testing.T) {
	table := renderStatusTable([]statusRow{
		{number: 1, dc: "A", role: "validator", id: "a", weight: "100000", state: statusUp, height: "812345"},
		{number: 9, dc: "A", role: "rpc", id: "i", weight: statusNA, state: statusUp, height: "812344"},
		{number: 3, dc: "", role: "validator", id: "c", weight: statusUnknown, state: statusNotInstalled, height: statusNA},
	})
	lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("table has %d lines:\n%s", len(lines), table)
	}
	if !strings.HasPrefix(lines[0], "NODE") || !strings.Contains(lines[0], "HEIGHT") {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "validator") || !strings.Contains(lines[1], "100000") {
		t.Fatalf("validator row = %q", lines[1])
	}
	if !strings.Contains(lines[2], "rpc") || !strings.Contains(lines[2], "-") {
		t.Fatalf("rpc row = %q", lines[2])
	}
	// An unset dc stays visibly unset instead of being invented.
	if fields := strings.Fields(lines[3]); fields[1] != statusNA || fields[4] != statusUnknown {
		t.Fatalf("unset dc row = %q", lines[3])
	}
	if !strings.Contains(lines[3], statusNotInstalled) {
		t.Fatalf("not installed row = %q", lines[3])
	}
}

func TestFatalProbeIgnoresDeliberateStates(t *testing.T) {
	cases := []struct {
		state       string
		apiAnswered bool
		drift       bool
		want        bool
	}{
		{statusUp, true, false, false},
		{statusUp, false, false, true},
		{statusDown, false, false, false},
		{statusNotInstalled, false, false, false},
		{statusFailed, false, false, false},
		{statusUp, true, true, true},
		{statusDown, true, true, true},
	}
	for _, test := range cases {
		if got := fatalProbe(test.state, test.apiAnswered, test.drift); got != test.want {
			t.Errorf("fatalProbe(%q, %v, %v) = %v, want %v", test.state, test.apiAnswered, test.drift, got, test.want)
		}
	}
}

func TestPChainStatusRow(t *testing.T) {
	visible := statusPChainProbe{
		number: 13, serviceState: statusUp, created: true, bootstrapped: true,
		localOK: true, localHeight: 289700,
		setsOK: true, mainVisible: true, managerVisible: true,
	}
	following := func(mutate func(*statusPChainProbe)) statusPChainRow {
		probe := visible
		probe.mode = followMode
		probe.upstreamOK = true
		probe.upstreamHeight = 289700
		mutate(&probe)
		return pchainStatusRow(probe)
	}

	row := following(func(*statusPChainProbe) {})
	want := statusPChainRow{13, "synced", "289700", "289700", "0", "complete", "yes"}
	if row != want {
		t.Errorf("synced row = %+v, want %+v", row, want)
	}

	row = following(func(probe *statusPChainProbe) { probe.upstreamHeight = 289900 })
	want = statusPChainRow{13, "catching-up", "289700", "289900", "200", "complete", "no"}
	if row != want {
		t.Errorf("catching-up row = %+v, want %+v", row, want)
	}

	// Local ahead of the sample is still synced and never a negative lag.
	row = following(func(probe *statusPChainProbe) { probe.upstreamHeight = 289600 })
	if row.mode != "synced" || row.lag != "0" || row.ready != "yes" {
		t.Errorf("local ahead row = %+v", row)
	}

	// Unreachable upstream: upstream and lag unknown, never ready to freeze.
	row = following(func(probe *statusPChainProbe) { probe.upstreamOK = false })
	if row.upstream != statusUnknown || row.lag != statusUnknown || row.ready != "no" {
		t.Errorf("unreachable upstream row = %+v, want unknown upstream and lag with ready no", row)
	}

	// A bootstrapping node cannot answer locally, but the upstream height was
	// still observed and must be reported rather than shown as unobservable.
	row = following(func(probe *statusPChainProbe) { probe.localOK = false })
	if row.upstream != "289700" {
		t.Errorf("upstream = %q, want the observed height even when the local node is silent", row.upstream)
	}
	if row.local != statusUnknown || row.lag != statusUnknown || row.mode != statusUnknown || row.ready != "no" {
		t.Errorf("silent local row = %+v, want unknown local, lag and mode with ready no", row)
	}

	// Still replaying: the mode says so and carries the progress, and the node
	// is never called synced no matter how the heights compare.
	row = following(func(probe *statusPChainProbe) {
		probe.bootstrapped = false
		probe.progress = "executing 97.5%, eta 9s"
	})
	if row.mode != statusBootstrapping+" executing 97.5%, eta 9s" {
		t.Errorf("bootstrapping mode = %q", row.mode)
	}
	if row.local != "289700" || row.upstream != "289700" || row.lag != "0" || row.ready != "no" {
		t.Errorf("bootstrapping row = %+v", row)
	}

	// Replaying without a progress line yet still reports the state.
	row = following(func(probe *statusPChainProbe) { probe.bootstrapped = false })
	if row.mode != statusBootstrapping {
		t.Errorf("bootstrapping without progress mode = %q", row.mode)
	}

	// A frozen node that is still replaying reports the replay, not the mode it
	// was rendered in, and never claims readiness.
	replayingFrozen := visible
	replayingFrozen.mode = frozenMode
	replayingFrozen.bootstrapped = false
	replayingFrozen.progress = "fetching 40.2%, eta 3m"
	row = pchainStatusRow(replayingFrozen)
	if row.mode != statusBootstrapping+" fetching 40.2%, eta 3m" || row.ready != statusNA {
		t.Errorf("replaying frozen row = %+v", row)
	}

	// Synced but the validator sets are incomplete: not ready to freeze.
	row = following(func(probe *statusPChainProbe) { probe.managerVisible = false })
	if row.mode != "synced" || row.l1State != "partial" || row.ready != "no" {
		t.Errorf("partial sets row = %+v", row)
	}

	// Frozen never contacts the upstream and never reports readiness.
	frozen := visible
	frozen.mode = frozenMode
	row = pchainStatusRow(frozen)
	want = statusPChainRow{13, frozenMode, "289700", statusNA, statusNA, "complete", statusNA}
	if row != want {
		t.Errorf("frozen row = %+v, want %+v", row, want)
	}

	// Undeterminable mode is reported, never guessed.
	unknown := visible
	row = pchainStatusRow(unknown)
	if row.mode != statusUnknown || row.upstream != statusUnknown || row.ready != "no" {
		t.Errorf("unknown mode row = %+v", row)
	}

	// A deliberately stopped P-chain machine is all not applicable.
	stopped := visible
	stopped.serviceState = statusDown
	row = pchainStatusRow(stopped)
	want = statusPChainRow{13, statusNA, statusNA, statusNA, statusNA, statusNA, statusNA}
	if row != want {
		t.Errorf("stopped row = %+v, want %+v", row, want)
	}

	// Before l1 create there is no validator set to see.
	uncreated := visible
	uncreated.created = false
	uncreated.mode = followMode
	uncreated.upstreamOK = true
	uncreated.upstreamHeight = 289700
	row = pchainStatusRow(uncreated)
	if row.local != "289700" || row.l1State != statusNA || row.ready != "no" {
		t.Errorf("uncreated row = %+v", row)
	}
}
