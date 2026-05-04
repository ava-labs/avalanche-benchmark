package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// remote shells out to ssh/scp via os/exec. We don't use x/crypto/ssh
// because it adds key management plumbing for no real benefit at this
// scale, and ssh/scp give us the same UNIX semantics the operator gets
// when debugging by hand.
type remote struct {
	user string
	key  string // optional; empty -> ssh defaults
}

func newRemote(user, key string) *remote {
	return &remote{user: user, key: key}
}

func (r *remote) sshArgs(host string) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
	}
	if r.key != "" {
		args = append(args, "-i", r.key)
	}
	args = append(args, fmt.Sprintf("%s@%s", r.user, host))
	return args
}

func (r *remote) scpArgs() []string {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-q",
	}
	if r.key != "" {
		args = append(args, "-i", r.key)
	}
	return args
}

// run executes a remote command on host. Stdout/stderr are returned in
// combined form, prefixed with [host] in error messages.
func (r *remote) run(ctx context.Context, host, script string) (string, error) {
	args := append(r.sshArgs(host), "bash", "-s")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("[%s] ssh: %w\n%s", host, err, string(out))
	}
	return string(out), nil
}

// scpUp copies localPath to host:remotePath. Recursive if localPath is
// a directory.
func (r *remote) scpUp(ctx context.Context, host, localPath, remotePath string) error {
	args := r.scpArgs()
	args = append(args, "-r", localPath, fmt.Sprintf("%s@%s:%s", r.user, host, remotePath))
	cmd := exec.CommandContext(ctx, "scp", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("[%s] scp %s -> %s: %w\n%s", host, localPath, remotePath, err, string(out))
	}
	return nil
}

// fanOut runs fn against every host concurrently and collects errors.
// All goroutines run; failures are aggregated rather than short-circuiting,
// because partial-failure visibility is more useful than fail-fast for an
// operator looking at "which host broke".
func fanOut(ctx context.Context, hosts []string, fn func(context.Context, string) error) error {
	var wg sync.WaitGroup
	errs := make([]error, len(hosts))
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			errs[i] = fn(ctx, h)
		}(i, h)
	}
	wg.Wait()

	var msgs []string
	for i, err := range errs {
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("  [%s] %v", hosts[i], err))
		}
	}
	if len(msgs) > 0 {
		return fmt.Errorf("%d/%d hosts failed:\n%s", len(msgs), len(hosts), strings.Join(msgs, "\n"))
	}
	return nil
}

// localCmd is a small helper for spawning local processes.
type localCmd struct {
	*exec.Cmd
}

func startLocal(ctx context.Context, name string, args ...string) (*localCmd, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &localCmd{Cmd: cmd}, nil
}
