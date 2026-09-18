// Package bootstrap reproduces the LXC/VPS bootstrap drivers —
// the provisioning orchestration + durable-plane exec driver.
//
// Ported here: the exec helpers, template selection, host/backend DETECTION,
// and the locked backend-resolution decision (planebase-coupled). The driver
// turns the pure planebase decisions into pct/zfs/lvm/mount commands through
// the runner's ONE exec primitive. Detection stays read-only; creating a
// backend is consent-gated; idempotency is preserved (never re-create).
package bootstrap

import (
	"fmt"
	"strconv"
	"strings"

	"freehold/contract/client"
	"freehold/platform/provisioning/planebase"
)

// EExecError steps that failed with a command exit.
type StepError struct {
	Step   string
	Exit   *int
	Output string
}

func (e *StepError) Error() string {
	return fmt.Sprintf("step '%s' failed (exit %v): %s", e.Step, exitStr(e.Exit), e.Output)
}

func exitStr(e *int) string {
	if e == nil {
		return "?"
	}
	return strconv.Itoa(*e)
}

// Exec runs a command through the runner's exec tool (target as the only
// secret ref — matching bootstrap::exec).
func Exec(clientConn *client.McpClient, target, cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
	return clientConn.Exec(target, cmd, []string{target}, timeoutS)
}

// ExpectOK asserts a step's exit code is 0 (or fail).
func ExpectOK(out *client.ExecOutcome, step string) error {
	if out.TimedOut {
		return &StepError{Step: step, Output: "TIMED OUT (runner watchdog killed the command)"}
	}
	if out.ExitCode == nil || *out.ExitCode != 0 {
		return &StepError{Step: step, Exit: out.ExitCode, Output: fmt.Sprintf("stdout: %s\nstderr: %s", out.Stdout, out.Stderr)}
	}
	return nil
}

// ExecToOK runs a command and asserts it succeeds.
func ExecToOK(clientConn *client.McpClient, target, cmd, step string, timeoutS uint64) (*client.ExecOutcome, error) {
	out, err := Exec(clientConn, target, cmd, timeoutS)
	if err != nil {
		return nil, err
	}
	if err := ExpectOK(out, step); err != nil {
		return nil, err
	}
	return out, nil
}

// Plain rejects a value outside the conservative safe alphabet — these are
// interpolated into shell commands.
func Plain(s string) error {
	for _, c := range s {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
		if !ok {
			return fmt.Errorf("unexpected characters in value %q (allowed: [A-Za-z0-9._-])", s)
		}
	}
	return nil
}

// PlainPath is the filesystem-path variant: '/' is legitimate.
func PlainPath(s string) error {
	for _, c := range s {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '/'
		if !ok {
			return fmt.Errorf("unexpected characters in path %q (allowed: [A-Za-z0-9._-/])", s)
		}
	}
	return nil
}

// TemplateVersion parses `debian-12-standard_12.7-1_amd64.tar.zst` -> [12 7].
func TemplateVersion(name string) []uint32 {
	segs := strings.Split(name, "_")
	for i, seg := range segs {
		if strings.HasSuffix(seg, "standard") {
			if i+1 >= len(segs) {
				return nil
			}
			version := segs[i+1]
			parts := strings.Split(version, ".")
			var out []uint32
			for _, part := range parts {
				clean := strings.SplitN(part, "-", 2)[0]
				if v, err := strconv.ParseUint(clean, 10, 32); err == nil {
					out = append(out, uint32(v))
				} else {
					return nil
				}
			}
			return out
		}
	}
	return nil
}

// DomainLXcName is `<normalized-domain>-<suffix>` (the LXC name convention).
func DomainLXCName(domain, suffix string) (string, error) {
	dom, err := planebase.NormalizeDomain(domain)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s", dom, suffix), nil
}

// LXCName resolves a managed guest's hostname: `<name>-<suffix>` for a world
// installed with a profile name, falling back to the domain-derived
// `<normalized-domain>-<suffix>` when name is empty so worlds that pre-date
// profile names keep reconciling. name must be LXC-name safe.
func LXCName(name, domain, suffix string) (string, error) {
	if name == "" {
		return DomainLXCName(domain, suffix)
	}
	if !planebase.ValidStorageName(name) {
		return "", fmt.Errorf("world name %q is not a valid LXC-name prefix", name)
	}
	return fmt.Sprintf("%s-%s", name, suffix), nil
}

// IsHex64 reports whether s is 64 lowercase/uppercase hex.
func IsHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
