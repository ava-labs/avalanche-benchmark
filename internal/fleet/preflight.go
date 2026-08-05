package fleet

import (
	"context"
	"fmt"
	"strings"
)

// preflightMinimumKiB is deliberately conservative: the gate exists to catch
// a full or read-only filesystem, not to plan capacity. One GiB covers what a
// deploy itself installs; databases grow far past it, and sizing for them is
// the operator's call, not this gate's.
const preflightMinimumKiB = 1024 * 1024

// preflightCommand builds the one-round-trip host validation for a machine.
// It mutates NOTHING: writability is checked with -w on the nearest existing
// ancestor of each target directory (deploy creates the missing tail with
// install -d), disk space is read with df, tools with command -v. Every
// problem prints a "preflight:" line and the shell exits nonzero only at the
// end, so one pass reports everything wrong with a host instead of only the
// first thing.
func preflightCommand(l layout) string {
	script := []string{
		"fail=0",
		`flunk() { echo "preflight: $1"; fail=1; }`,
	}
	// tar unpacks the P-chain seed, rsync receives every upload. User-mode
	// stop and kill lean on procps; a system install is driven over systemctl.
	tools := []string{"tar", "rsync"}
	if l.user {
		tools = append(tools, "pgrep", "pkill")
	} else {
		tools = append(tools, "systemctl")
	}
	for _, tool := range tools {
		script = append(script, fmt.Sprintf(
			`command -v %[1]s >/dev/null 2>&1 || flunk "required tool %[1]s is not installed"`, tool))
	}
	script = append(script,
		`[ -w /tmp ] || flunk "/tmp is not writable by $(id -un), and deploy stages uploads there"`)
	if l.user {
		for _, dir := range []string{l.pkg, l.cfg, l.data} {
			script = append(script, fmt.Sprintf(
				`a=%[1]s; while [ ! -d "$a" ] && [ "$a" != / ]; do a=$(dirname "$a"); done; `+
					`{ [ -d "$a" ] && [ -w "$a" ]; } || flunk "cannot create %[1]s: nearest existing directory $a is not writable by $(id -un)"`,
				dir))
		}
	} else {
		script = append(script,
			`sudo -n true 2>/dev/null || flunk "passwordless sudo is unavailable for $(id -un); a system install needs root (set REMOTE_DIR in .env for a user-level install)"`)
	}
	script = append(script,
		fmt.Sprintf(`a=%s; while [ ! -d "$a" ] && [ "$a" != / ]; do a=$(dirname "$a"); done`, l.data),
		`free_kib=$(df -Pk "$a" 2>/dev/null | awk 'NR==2 {print $4}')`,
		fmt.Sprintf(`{ [ -n "$free_kib" ] && [ "$free_kib" -ge %d ]; } || flunk "data filesystem at $a has ${free_kib:-unknown} KiB free, need at least %d KiB"`,
			preflightMinimumKiB, preflightMinimumKiB),
		"exit $fail")
	return strings.Join(script, "; ")
}

// preflightHosts validates every host in one parallel pass before the deploy
// mutates anything. The ordering is the contract: the P-chain node used to be
// stopped as a deploy's first act, so the first host mismatch (missing sudo,
// an unwritable REMOTE_DIR, a full disk, an unreachable box) left the fleet's
// sole bootstrap node down with nothing installed to replace it (reported
// 2026-08-05). A deploy that cannot succeed everywhere now touches nothing.
func (d *Deployer) preflightHosts(ctx context.Context, prepared deployment, targets []nodeDeployment) error {
	state := prepared
	state.selected = targets
	command := preflightCommand(layoutFor(prepared.environment))
	if err := d.phaseParallel(ctx, state, "preflight", func(ctx context.Context, dep deployment, node nodeDeployment) error {
		return d.runSSH(ctx, dep, node, command)
	}); err != nil {
		return fmt.Errorf("preflight failed and nothing was changed: %w", err)
	}
	fmt.Fprintf(d.out, "preflight passed on %d host(s)\n", len(targets))
	return nil
}
