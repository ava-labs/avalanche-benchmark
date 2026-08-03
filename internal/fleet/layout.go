package fleet

import (
	"fmt"
	"strings"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
)

// layout answers where fleet files live on the machines and how node
// processes are managed there. Two modes:
//
//   - system (default): /opt, /etc, /var/lib + systemd units. Every install
//     and lifecycle command runs under sudo, and nodes restart on failure and
//     on boot.
//   - user (REMOTE_DIR set): everything under one user-owned directory, no
//     sudo anywhere, nodes run as plain processes started by a rendered
//     run.sh with a pidfile. No boot persistence and no auto-restart; the
//     operator owns the lifecycle, as in the original REMOTE_DIR toolset.
type layout struct {
	pkg  string // binaries and plugins, per node number
	cfg  string // node.json, chain/subnet configs, staking keys
	data string // databases and logs
	user bool
}

func layoutFor(environment config.FleetEnvironment) layout {
	if environment.RemoteDir == "" {
		return layout{
			pkg:  remotePackageDir,
			cfg:  remoteConfigDir,
			data: remoteDataDir,
		}
	}
	root := strings.TrimRight(environment.RemoteDir, "/")
	data := environment.RemoteDataDir
	if data == "" {
		data = root + "/data"
	}
	return layout{
		pkg:  root + "/pkg",
		cfg:  root + "/config",
		data: strings.TrimRight(data, "/"),
		user: true,
	}
}

// sudo prefixes a command with sudo in the system layout and leaves it alone
// in a user-level install.
func (l layout) sudo(command string) string {
	if l.user {
		return command
	}
	return "sudo " + command
}

// pidFile is the user-mode process handle for a node. It lives beside the
// run script, NOT in the data directory: the data directory is the one path
// an operator legitimately re-points (REMOTE_DATA_DIR), and a pidfile that
// moves with it orphans every running process, which stop can then not find
// (observed 2026-08-03: a re-deploy with a changed data dir left the old
// P-chain node holding its port and the new one died at start).
func (l layout) pidFile(number int) string {
	return fmt.Sprintf("%s/%d/avalanchego.pid", l.cfg, number)
}

// processPattern matches a node's avalanchego by the config file it was
// started with, as a pidfile-independent fallback for stop and kill. The
// bracket keeps the pattern from matching the pkill shell's own argv.
func (l layout) processPattern(number int) string {
	return fmt.Sprintf("[c]onfig/%d/node.json", number)
}

// runScript is the user-mode replacement for a systemd unit.
func (l layout) runScript(number int) string {
	return fmt.Sprintf("%s/%d/run.sh", l.cfg, number)
}

// pluginPattern matches a node's subnet-evm plugin process by its on-disk
// path. The bracket keeps the pattern from matching the pgrep/pkill shell
// whose own argv contains it.
func (l layout) pluginPattern(number int) string {
	return fmt.Sprintf("%s/%d/[p]lugins/", l.pkg, number)
}

// startCommand starts a node and reports failure if it did not come up. Both
// modes are idempotent: an already-running node is a success.
func (l layout) startCommand(node nodeDeployment) string {
	if !l.user {
		unit := serviceName(node)
		return fmt.Sprintf("sudo systemctl start %[1]s && sudo systemctl is-active --quiet %[1]s", unit)
	}
	number := node.node.Number
	return fmt.Sprintf(
		"if kill -0 \"$(cat %[1]s 2>/dev/null)\" 2>/dev/null; then exit 0; fi; sh %[2]s && sleep 1 && kill -0 \"$(cat %[1]s)\"",
		l.pidFile(number), l.runScript(number))
}

// stopCommand stops a node if it is installed/running and verifies it is
// gone. In user mode it also reaps an orphaned plugin child, which otherwise
// holds the plugin binary open (ETXTBSY) and blocks the next upload.
func (l layout) stopCommand(node nodeDeployment) string {
	if !l.user {
		unit := serviceName(node)
		return fmt.Sprintf(
			"if sudo systemctl cat %[1]s >/dev/null 2>&1; then sudo systemctl stop %[1]s && test \"$(sudo systemctl is-active %[1]s)\" = inactive; fi",
			unit)
	}
	number := node.node.Number
	return fmt.Sprintf(
		"if [ -f %[1]s ]; then kill \"$(cat %[1]s)\" 2>/dev/null || true; "+
			"for i in $(seq 1 40); do kill -0 \"$(cat %[1]s)\" 2>/dev/null || break; sleep 0.25; done; "+
			"kill -KILL \"$(cat %[1]s)\" 2>/dev/null || true; rm -f %[1]s; fi; "+
			"pkill -f '%[3]s' 2>/dev/null || true; "+
			"for i in $(seq 1 40); do pgrep -f '%[3]s' >/dev/null 2>&1 || break; sleep 0.25; done; "+
			"pkill -KILL -f '%[3]s' 2>/dev/null || true; "+
			"pkill -KILL -f '%[2]s' 2>/dev/null || true",
		l.pidFile(number), l.pluginPattern(number), l.processPattern(number))
}

// isActiveCommand exits zero when the node process is running.
func (l layout) isActiveCommand(node nodeDeployment) string {
	if !l.user {
		return "sudo systemctl is-active --quiet " + serviceName(node)
	}
	return fmt.Sprintf("kill -0 \"$(cat %s 2>/dev/null)\" 2>/dev/null", l.pidFile(node.node.Number))
}

// enabledProbe prints the boot-enablement state. User-mode installs have no
// boot persistence, so the probe reports the run script's presence instead:
// "installed" plays the role systemd's "enabled" plays for status displays.
func (l layout) enabledProbe(node nodeDeployment) string {
	if !l.user {
		return fmt.Sprintf("sudo systemctl is-enabled %s 2>/dev/null || true", serviceName(node))
	}
	return fmt.Sprintf("test -f %s && echo installed || true", l.runScript(node.node.Number))
}

// installUnitCommand wires up process management from the staged render:
// a systemd unit in the system layout, the run script in a user install.
func (l layout) installUnitCommand(stage string, node nodeDeployment) string {
	if !l.user {
		unit := serviceName(node)
		return fmt.Sprintf(
			"sudo install -m 0644 %s/node.service /etc/systemd/system/%s && sudo systemctl daemon-reload && sudo systemctl enable %s",
			stage, unit, unit)
	}
	return fmt.Sprintf("install -m 0755 %s/run.sh %s", stage, l.runScript(node.node.Number))
}

// startOnlyCommand starts a node without proving it came up; deploy's start
// phase leaves the proving to the readiness gate that follows it.
func (l layout) startOnlyCommand(node nodeDeployment) string {
	if !l.user {
		return "sudo systemctl start " + serviceName(node)
	}
	return l.startCommand(node)
}

// enableAndStartCommand restores boot intent and starts the node. A user
// install has no boot intent to restore, so it is just a start.
func (l layout) enableAndStartCommand(node nodeDeployment) string {
	if !l.user {
		unit := serviceName(node)
		return fmt.Sprintf(
			"sudo systemctl enable %[1]s && sudo systemctl start %[1]s", unit)
	}
	return l.startCommand(node)
}

// disableAndStopCommand records down intent and gracefully stops the node. A
// user install has no down intent to record, so it is just a stop.
func (l layout) disableAndStopCommand(node nodeDeployment) string {
	if !l.user {
		unit := serviceName(node)
		return fmt.Sprintf(
			"if sudo systemctl cat %[1]s >/dev/null 2>&1; then "+
				"sudo systemctl disable %[1]s && sudo systemctl stop %[1]s && "+
				"test \"$(sudo systemctl is-active %[1]s)\" = inactive; fi",
			unit)
	}
	return l.stopCommand(node)
}

// killCommand simulates abrupt machine loss: no graceful shutdown, no pending
// restart, and the node proven gone. Every step is best effort so an
// already-dead node is still destroyable.
func (l layout) killCommand(node nodeDeployment) string {
	if !l.user {
		unit := serviceName(node)
		return fmt.Sprintf(
			"if sudo systemctl cat %[1]s >/dev/null 2>&1; then "+
				"sudo systemctl disable %[1]s || true; "+
				"sudo systemctl kill -s SIGKILL %[1]s || true; "+
				"sudo systemctl stop %[1]s || true; "+
				"sudo systemctl reset-failed %[1]s >/dev/null 2>&1 || true; "+
				"test \"$(sudo systemctl is-active %[1]s)\" = inactive; fi",
			unit)
	}
	number := node.node.Number
	return fmt.Sprintf(
		"if [ -f %[1]s ]; then kill -KILL \"$(cat %[1]s)\" 2>/dev/null || true; rm -f %[1]s; fi; "+
			"pkill -KILL -f '%[3]s' 2>/dev/null || true; "+
			"pkill -KILL -f '%[2]s' 2>/dev/null || true",
		l.pidFile(number), l.pluginPattern(number), l.processPattern(number))
}

// isActiveProbe prints the node's activity state; callers compare the output
// against "active".
func (l layout) isActiveProbe(node nodeDeployment) string {
	if !l.user {
		return fmt.Sprintf("sudo systemctl is-active %s 2>/dev/null || true", serviceName(node))
	}
	return fmt.Sprintf(
		"if kill -0 \"$(cat %s 2>/dev/null)\" 2>/dev/null; then echo active; else echo inactive; fi",
		l.pidFile(node.node.Number))
}

// serviceProbe prints "present|missing <active-state> <enabled-state>" in one
// round trip. In a user install presence is the run script and the enabled
// column reports "installed", which collapseServiceState treats as down when
// the process is not running: exactly right, since user installs have no boot
// persistence.
func (l layout) serviceProbe(node nodeDeployment) string {
	if !l.user {
		unit := serviceName(node)
		return fmt.Sprintf(
			"if sudo systemctl cat %[1]s >/dev/null 2>&1; then printf 'present '; else printf 'missing '; fi; "+
				"printf '%%s ' \"$(sudo systemctl is-active %[1]s)\"; "+
				"printf '%%s' \"$(sudo systemctl is-enabled %[1]s 2>/dev/null)\"",
			unit)
	}
	number := node.node.Number
	return fmt.Sprintf(
		"if [ -f %[1]s ]; then printf 'present '; else printf 'missing '; fi; "+
			"if kill -0 \"$(cat %[2]s 2>/dev/null)\" 2>/dev/null; then printf 'active '; else printf 'inactive '; fi; "+
			"test -f %[1]s && printf installed || true",
		l.runScript(number), l.pidFile(number))
}

// activeEnabledProbe prints "<active-state> <enabled-state>". A stopped node
// in a user install never reports "enabled", so reconciliation treats it as
// deliberately down: with no boot intent on record, the operator's stop is
// the only intent there is.
func (l layout) activeEnabledProbe(node nodeDeployment) string {
	if !l.user {
		unit := serviceName(node)
		return fmt.Sprintf(
			"printf '%%s %%s' \"$(sudo systemctl is-active %[1]s 2>/dev/null)\" \"$(sudo systemctl is-enabled %[1]s 2>/dev/null)\"",
			unit)
	}
	number := node.node.Number
	return fmt.Sprintf(
		"if kill -0 \"$(cat %[1]s 2>/dev/null)\" 2>/dev/null; then printf 'active '; else printf 'inactive '; fi; "+
			"test -f %[2]s && printf installed || true",
		l.pidFile(number), l.runScript(number))
}

// installIdentityCommand moves staged key material into its staking directory.
// The system layout installs as root and hands ownership to the service user;
// a user install already owns everything, so the ownership flags disappear
// with the sudo.
func (l layout) installIdentityCommand(stage, files, owner string, number int) string {
	target := fmt.Sprintf("%s/%d/staking", l.cfg, number)
	if l.user {
		return fmt.Sprintf(
			"rm -rf %[1]s && install -d -m 0700 %[1]s && "+
				"for file in %[2]s; do install -m 0600 %[3]s/$file %[1]s/$file; done && "+
				"rm -rf %[3]s",
			target, files, stage)
	}
	return fmt.Sprintf(
		"sudo rm -rf %[1]s && sudo install -d -o %[2]s -g %[2]s -m 0700 %[1]s && "+
			"for file in %[3]s; do sudo install -o %[2]s -g %[2]s -m 0600 %[4]s/$file %[1]s/$file; done && "+
			"rm -rf %[4]s",
		target, owner, files, stage)
}
