package proxmox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"freehold/contract/client"
)

// SSHExec returns the transient-access ExecFunc: each command runs as root on
// the Proxmox host over SSH, authenticating with a 0600 private-key file (the
// box's derived DOOR_SPEC key). No served runner is needed, so install/teardown
// can reach the host before (or after) the CP's co-located runner exists.
//
// `cmd` is passed as the remote command string, so the SSH login shell parses
// it — guest commands wrapped in `pct exec <id> -- sh -c '...'` behave exactly
// as they do through the runner.
func SSHExec(host, keyPath string) ExecFunc {
	return func(cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
		ctx := context.Background()
		if timeoutS > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutS)*time.Second)
			defer cancel()
		}
		c := exec.CommandContext(ctx, "ssh", sshArgs(host, keyPath, cmd)...)
		var stdout, stderr bytes.Buffer
		c.Stdout = &stdout
		c.Stderr = &stderr
		err := c.Run()
		exit := 0
		timedOut := ctx.Err() == context.DeadlineExceeded
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				exit = ee.ExitCode()
			} else {
				return nil, fmt.Errorf("ssh %s: %w", host, err)
			}
		}
		return &client.ExecOutcome{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: &exit,
			TimedOut: timedOut,
		}, nil
	}
}

// sshArgs is the exact ssh invocation (kept separate so it is testable).
// BatchMode fails closed instead of prompting; accept-new pins the host key on
// first contact and refuses a changed one thereafter.
func sshArgs(host, keyPath, cmd string) []string {
	return []string{
		"-i", keyPath,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=" + strconv.Itoa(15),
		"root@" + host,
		cmd,
	}
}

// WriteTempKey writes a private-key PEM to a 0600 temp file and returns its
// path plus a cleanup func the caller must defer. The key is the DOOR_SPEC
// private half — never log it, never leave it behind.
func WriteTempKey(pem []byte) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "freehold-door-*.key")
	if err != nil {
		return "", nil, err
	}
	path = f.Name()
	cleanup = func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if _, err := f.Write(pem); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}
