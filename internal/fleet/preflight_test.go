package fleet

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
)

func TestPreflightCommandMatchesTheInstallMode(t *testing.T) {
	user := preflightCommand(layoutFor(config.FleetEnvironment{RemoteDir: "/home/op/kit", RemoteDataDir: "/nvme/data"}))
	for _, want := range []string{"/home/op/kit/pkg", "/home/op/kit/config", "/nvme/data", "pgrep", "pkill", "tar", "rsync", "df -Pk", "exit $fail"} {
		if !strings.Contains(user, want) {
			t.Fatalf("user preflight lacks %q: %s", want, user)
		}
	}
	if strings.Contains(user, "sudo") || strings.Contains(user, "systemctl") {
		t.Fatalf("user preflight reaches for root: %s", user)
	}

	system := preflightCommand(layoutFor(config.FleetEnvironment{}))
	for _, want := range []string{"sudo -n true", "systemctl", "tar", "rsync", remoteDataDir, "exit $fail"} {
		if !strings.Contains(system, want) {
			t.Fatalf("system preflight lacks %q: %s", want, system)
		}
	}

	// The whole point of a preflight is that it changes nothing on the host.
	for _, command := range []string{user, system} {
		for _, mutator := range []string{"mkdir", "install -", "rm -", "mktemp", "touch ", "chown", "tee ", " > "} {
			if strings.Contains(command, mutator) {
				t.Fatalf("preflight mutates the host (%q): %s", mutator, command)
			}
		}
	}
}

// A failed preflight must name EVERY failing host, not just the first: an
// operator fixing hosts one error at a time re-runs the deploy once per
// problem, against the very fleet the preflight exists to protect.
func TestPreflightReportsEveryFailingHostAndChangesNothing(t *testing.T) {
	runner := &recordingRunner{runErrors: map[int]error{0: errors.New("no sudo"), 1: errors.New("no sudo")}}
	deployer := &Deployer{root: t.TempDir(), out: io.Discard, runner: runner}
	prepared := deployment{environment: config.FleetEnvironment{SSHUser: "ubuntu", SSHKeyPath: "/key"}}
	targets := []nodeDeployment{
		{node: config.Node{Number: 13, Host: "pchain", Role: config.RolePChain}},
		{node: config.Node{Number: 1, Host: "v1", Role: config.RoleValidator}},
	}
	err := deployer.preflightHosts(context.Background(), prepared, targets)
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("preflight error = %v", err)
	}
	if !strings.Contains(err.Error(), "node 13") || !strings.Contains(err.Error(), "node 1 (") {
		t.Fatalf("preflight did not report every failing host: %v", err)
	}
	if len(runner.runs) != 2 {
		t.Fatalf("preflight ran %d commands, want exactly one per host: %v", len(runner.runs), runner.runs)
	}
}
