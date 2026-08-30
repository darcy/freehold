// Package bootstrap reproduces orchestrator/src/bootstrap.rs + drive.rs —
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

	"freehold/orchestrator/internal/client"
	"freehold/orchestrator/internal/planebase"
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

// ZpoolList lists existing zpools (`zpool list -H -o name`). Read-only.
func ZpoolList(clientConn *client.McpClient, target string) ([]string, error) {
	out, err := Exec(clientConn, target, "zpool list -H -o name", 60)
	if err != nil {
		return nil, err
	}
	if out.ExitCode == nil || *out.ExitCode != 0 {
		return []string{}, nil // zpool may be absent entirely
	}
	var names []string
	for _, l := range strings.Split(out.Stdout, "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

// ZpoolExists reports whether the named zpool exists.
func ZpoolExists(clientConn *client.McpClient, target, pool string) (bool, error) {
	list, err := ZpoolList(clientConn, target)
	if err != nil {
		return false, err
	}
	for _, p := range list {
		if p == pool {
			return true, nil
		}
	}
	return false, nil
}

// VGList lists existing LVM volume groups.
func VGList(clientConn *client.McpClient, target string) ([]string, error) {
	out, err := Exec(clientConn, target, "vgs --noheadings -o vg_name 2>/dev/null || true", 60)
	if err != nil {
		return nil, err
	}
	if out.ExitCode == nil || *out.ExitCode != 0 {
		return []string{}, nil
	}
	return strings.Fields(out.Stdout), nil
}

// ThinPools lists ALL thin pools in the VG (each marked by its _tmeta/_tdata
// companion pair), in lvs order. The placement gate needs the full set:
// "reuse the detected first pool" and "is the NAMED pool already carved"
// are different questions the first-pool answer alone cannot distinguish —
// a two-pool VG with `--thin-pool <the other one>` must adopt, not claim.
func ThinPools(clientConn *client.McpClient, target, vg string) ([]string, error) {
	names, err := lvsNames(clientConn, target, vg)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	var pools []string
	for _, n := range names {
		if !strings.HasSuffix(n, "_tmeta") {
			continue
		}
		if pool := strings.TrimSuffix(n, "_tmeta"); set[pool+"_tdata"] {
			pools = append(pools, pool)
		}
	}
	return pools, nil
}

// ThinPoolName is the FIRST thin pool REUSED for freehold tenant LVs in a
// VG, if it already has one (marked by the _tmeta/_tdata companion pair).
func ThinPoolName(clientConn *client.McpClient, target, vg string) (string, bool, error) {
	pools, err := ThinPools(clientConn, target, vg)
	if err != nil {
		return "", false, err
	}
	if len(pools) == 0 {
		return "", false, nil
	}
	return pools[0], true, nil
}

// ThinLVExists reports whether a thin LV already exists for a tenant.
func ThinLVExists(clientConn *client.McpClient, target, vg, tenant string) (bool, error) {
	names, err := lvsNames(clientConn, target, vg)
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == tenant {
			return true, nil
		}
	}
	return false, nil
}

// ThinPoolExists reports whether a SPECIFIC thin pool exists in the VG
// (marked by its _tmeta/_tdata companion pair). ThinPoolName finds ANY pool;
// this one names it — the plane-placement gate's adopt-or-carve probe.
func ThinPoolExists(clientConn *client.McpClient, target, vg, pool string) (bool, error) {
	pools, err := ThinPools(clientConn, target, vg)
	if err != nil {
		return false, err
	}
	for _, p := range pools {
		if p == pool {
			return true, nil
		}
	}
	return false, nil
}

// ThinPoolNameOther returns the first thin pool in the VG EXCEPT `except` —
// the full teardown's local-lvm re-point wants a SURVIVING pool; the doomed
// one must never be picked as its own successor.
func ThinPoolNameOther(clientConn *client.McpClient, target, vg, except string) (string, bool, error) {
	pools, err := ThinPools(clientConn, target, vg)
	if err != nil {
		return "", false, err
	}
	for _, p := range pools {
		if p != except {
			return p, true, nil
		}
	}
	return "", false, nil
}

func lvsNames(clientConn *client.McpClient, target, vg string) ([]string, error) {
	out, err := Exec(clientConn, target, fmt.Sprintf("lvs -a --noheadings -o lv_name %s 2>/dev/null || true", vg), 60)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range strings.Fields(out.Stdout) {
		names = append(names, strings.Trim(f, "[]"))
	}
	return names, nil
}

// ResolveAction is the backend-resolution outcome (planebase.ResolveAction).
type ResolveAction struct {
	Kind     string // Reuse | Create | Bail
	Detected *planebase.ExistingBackend
	Backend  *planebase.Backend
	Pool     string
	Message  string
}

// ReuseAction / CreateAction / BailAction are constructors.
func ReuseAction(b planebase.ExistingBackend, pool string) *ResolveAction {
	return &ResolveAction{Kind: "Reuse", Detected: &b, Pool: pool}
}
func CreateAction(b planebase.Backend, pool string) *ResolveAction {
	return &ResolveAction{Kind: "Create", Backend: &b, Pool: pool}
}
func BailAction(msg string) *ResolveAction {
	return &ResolveAction{Kind: "Bail", Message: msg}
}

// ResolveProxmox resolves the Proxmox backend preserving the locked order
// (ZFS -> LVM-thin -> bail); consent only decides Create vs Bail, never
// reorders.
func ResolveProxmox(clientConn *client.McpClient, target string, consent bool, device *string) (*ResolveAction, error) {
	pools, err := ZpoolList(clientConn, target)
	if err != nil {
		return nil, err
	}
	if len(pools) > 0 {
		return ReuseAction(planebase.ExistingZfs, pools[0]), nil
	}
	vgs, err := VGList(clientConn, target)
	if err != nil {
		return nil, err
	}
	if len(vgs) > 0 {
		return ReuseAction(planebase.ExistingLvmThin, vgs[0]), nil
	}
	if !consent {
		return BailAction("no existing ZFS zpool or LVM VG detected, and --confirm-storage is not set — re-run with --confirm-storage to create one (or attach a disk / use a NAS)"), nil
	}
	// Consent given: prefer ZFS (needs a device), else LVM-thin.
	if device != nil && *device != "" {
		return CreateAction(planebase.BackendZfs, "rpool"), nil
	}
	return CreateAction(planebase.BackendLvmThin, "freehold"), nil
}

// EnsureZpool creates a zpool if absent (idempotent).
func EnsureZpool(clientConn *client.McpClient, target, pool string, device *string) error {
	exists, err := ZpoolExists(clientConn, target, pool)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if device == nil || *device == "" {
		return fmt.Errorf("creating a zpool needs a physical device — pass --device (e.g. /dev/sdb); or use an existing zpool/LVM backend")
	}
	_, err = ExecToOK(clientConn, target, "zpool create "+pool+" "+*device, "zpool create", 300)
	return err
}

// EnsureDataset creates a dataset if absent (idempotent).
func EnsureDataset(clientConn *client.McpClient, target, dataset string) error {
	exists, err := Exec(clientConn, target, "zfs list -H -o name "+dataset+" >/dev/null 2>&1", 60)
	if err != nil {
		return err
	}
	if exists.ExitCode != nil && *exists.ExitCode == 0 {
		return nil
	}
	_, err = ExecToOK(clientConn, target, "zfs create -p "+dataset, "zfs create dataset", 120)
	return err
}

// DomainLXcName is `<normalized-domain>-<suffix>` (the LXC name convention).
func DomainLXCName(domain, suffix string) (string, error) {
	dom, err := planebase.NormalizeDomain(domain)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s", dom, suffix), nil
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
