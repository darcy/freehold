package proxmox

import (
	"fmt"
	"strconv"
	"strings"

	"freehold/contract/client"
	"freehold/platform/provisioning"
	"freehold/platform/provisioning/bootstrap"
	"freehold/providers/proxmox/drive"
)

// Provider is the Proxmox implementation of the platform provisioning seam.
// The transport is injected, so the same provider backs the box engine
// (self-exec) and the composition roots (runner client).
type Provider struct {
	exec ExecFunc
}

// New builds a Proxmox provider over a host-command transport.
func New(exec ExecFunc) *Provider { return &Provider{exec: exec} }

// FromClient builds a provider whose transport is a runner MCP client.
func FromClient(clientConn *client.McpClient, target string) *Provider {
	return New(func(cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
		return bootstrap.Exec(clientConn, target, cmd, timeoutS)
	})
}

func (p *Provider) GuestExec(guest, cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
	return p.exec(LxcExec(guest, cmd), timeoutS)
}

// ListGuests parses `pct list` into (id, name) rows.
func (p *Provider) ListGuests() ([]provisioning.Guest, error) {
	out, err := p.exec("pct list", 120)
	if err != nil {
		return nil, err
	}
	if err := bootstrap.ExpectOK(out, "pct list"); err != nil {
		return nil, err
	}
	var guests []provisioning.Guest
	for _, line := range strings.Split(out.Stdout, "\n")[1:] {
		cols := strings.Fields(line)
		if len(cols) < 2 {
			continue
		}
		guests = append(guests, provisioning.Guest{ID: cols[0], Name: cols[len(cols)-1]})
	}
	return guests, nil
}

func (p *Provider) GuestIPv4(guest string) (string, error) {
	out, err := p.exec(fmt.Sprintf("pct exec %s -- ip -4 -o addr show eth0", guest), 30)
	if err != nil {
		return "", fmt.Errorf("ip readback failed on LXC %s: %w", guest, err)
	}
	return parseLxcIP(out.Stdout, guest)
}

func (p *Provider) GuestMounts(guest string) ([]string, error) {
	out, err := p.exec(fmt.Sprintf("pct config %s", guest), 0)
	if err != nil {
		return nil, err
	}
	return parsePctMounts(out.Stdout), nil
}

// LocalLvmStatus returns the pool PVE's local-lvm currently points at and how
// many LVs ride it.
func (p *Provider) LocalLvmStatus() (string, int, error) {
	riders, err := p.runScript(drive.LocalLvmRidersScript, 30)
	if err != nil {
		return "", 0, err
	}
	current, err := p.runScript(drive.LocalLvmProbeScript, 30)
	if err != nil {
		return "", 0, err
	}
	return strings.TrimSpace(current), countLines(riders), nil
}

func (p *Provider) RepointLocalLvm(thinPool string) error {
	return drive.RepointLocalLvm(p.runScript, thinPool)
}

// runScript runs one host script and returns its stdout, failing on a non-zero
// exit (the discipline box's self-exec used).
func (p *Provider) runScript(script string, timeoutS uint64) (string, error) {
	out, err := p.exec(script, timeoutS)
	if err != nil {
		return "", err
	}
	if out.ExitCode != nil && *out.ExitCode != 0 {
		return out.Stdout, fmt.Errorf("script failed (exit %d):\n%s", *out.ExitCode, out.Stdout)
	}
	if out.TimedOut {
		return out.Stdout, fmt.Errorf("script timed out")
	}
	return out.Stdout, nil
}

// findLxcVmid parses a `pct list` dump for an exact-name row (used by tests).
func findVmidInList(out, exact string) (uint32, error) {
	for _, line := range strings.Split(out, "\n")[1:] {
		cols := strings.Fields(line)
		if len(cols) < 2 || cols[len(cols)-1] != exact {
			continue
		}
		vmid, err := strconv.ParseUint(cols[0], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("unparseable vmid %q in %q", cols[0], line)
		}
		return uint32(vmid), nil
	}
	return 0, fmt.Errorf("no container named %s found on the host:\n%s", exact, out)
}

// parseLxcIP extracts the first `/`-token, refusing loopback.
func parseLxcIP(out, guest string) (string, error) {
	for _, t := range strings.Fields(out) {
		if strings.Contains(t, "/") {
			if t == "127.0.0.1/8" {
				break
			}
			return t, nil
		}
	}
	return "", fmt.Errorf("no ipv4 on LXC %s eth0:\n%s", guest, out)
}

// parsePctMounts extracts the `mp=` guest paths of `mp<digits>:` lines, in
// order (a loose `mp` prefix would catch unrelated keys).
func parsePctMounts(out string) []string {
	var mounts []string
	for _, l := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(l, "mp")
		if !ok {
			continue
		}
		idx, rest, ok := strings.Cut(rest, ":")
		if !ok || idx == "" {
			continue
		}
		digits := true
		for _, c := range idx {
			if c < '0' || c > '9' {
				digits = false
				break
			}
		}
		if !digits {
			continue
		}
		for _, kv := range strings.Split(rest, ",") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(kv), "mp="); ok {
				mounts = append(mounts, v)
				break
			}
		}
	}
	return mounts
}

func countLines(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

// DestroyGuest destroys a guest and its rootfs (idempotent: an absent guest is
// a no-op). Used by the transient uninstall path when no runner is available.
func (p *Provider) DestroyGuest(guest string) error {
	out, err := p.exec(fmt.Sprintf("pct status %s >/dev/null 2>&1 || exit 0; pct destroy %s --purge", guest, guest), 300)
	if err != nil {
		return err
	}
	if out.ExitCode != nil && *out.ExitCode != 0 {
		return fmt.Errorf("pct destroy %s: exit %d: %s", guest, *out.ExitCode, strings.TrimSpace(out.Stdout))
	}
	return nil
}
