package proxmox

import (
	"context"
	"fmt"

	"freehold/platform/provisioning"
)

// HostProvider is the proxmox host-provisioning seam: the host is the
// OPERATOR'S box — reached over root SSH, never created or destroyed. Its
// needs are the host address, the edge's LAN address, and the storage-create
// consent; its door UX is the classic paste gate (the operator appends the
// door line by hand).
type HostProvider struct{}

func (HostProvider) Name() string       { return "proxmox" }
func (HostProvider) AccessMode() string { return "ssh-root-proxmox" }

// HostsGateway: a LAN host's gateway is a GUEST (the two-NIC LXC holding
// the one LAN address) — the surrounding network sees the router's LAN, and
// the gateway guest owns the proxy IP.
func (HostProvider) HostsGateway() bool { return false }

func (HostProvider) Needs() []provisioning.HostNeed {
	return []provisioning.HostNeed{
		{Name: provisioning.NeedHost, Label: "Host (address the runner will SSH into)", Default: "root@192.168.30.224"},
		{Name: provisioning.NeedProxyIP, Label: "the ONE LAN address — the gateway's (CIDR, e.g. 192.168.30.8/24) — REQUIRED; everything public resolves here"},
		{Name: provisioning.NeedConfirmStorage, Label: "If this host has no usable storage, may freehold create a new one? (freehold never erases existing data)", Bool: true},
	}
}

func (HostProvider) Defaults() map[string]string {
	return map[string]string{"storage": "local-lvm", "bridge": "vmbr0", "relay_gw": "192.168.30.1"}
}

// Prepare: nothing to provision — the host is the operator's. The address
// came from the host need; the reachability probe is the pipeline's
// HostSideLiveCheck.
func (p HostProvider) Prepare(_ context.Context, s *provisioning.HostSession, _ string) (*provisioning.Host, error) {
	if s.Host == "" {
		return nil, fmt.Errorf("the proxmox provider needs the host address (root@<box>)")
	}
	return &provisioning.Host{IP: stripUser(s.Host)}, nil
}

// InstallDoorKey is the paste gate: a freshly minted (or recovered) door key
// is shown with its authorized_keys line and the pipeline waits until the
// operator confirms it is in place. A headless run bails actionably — a
// pipeline that cannot prompt cannot install the key either.
func (p HostProvider) InstallDoorKey(s *provisioning.HostSession, key string) error {
	if key == "" {
		return nil
	}
	if !s.Interactive {
		return fmt.Errorf(
			"the door needs a NEW ssh key before the pipeline can continue — install it on %s, then re-run:\n\n    %s\n\n  (on the host: mkdir -p /root/.ssh && echo '%s' >> /root/.ssh/authorized_keys)",
			s.Host, key, key)
	}
	for {
		s.Print(`
  ─────────────────────────────────────────────────────────
  Finish the door: add this line to %s's ~/.ssh/authorized_keys:

    %s

  (on the host: mkdir -p /root/.ssh && echo '%s' >> /root/.ssh/authorized_keys)
  ─────────────────────────────────────────────────────────
`, s.Host, key, key)
		answer, err := s.Prompt("Press ENTER when it's in place, or 'r' to show it again, 'q' to quit")
		if err != nil {
			return fmt.Errorf("aborted by the operator (door not installed)")
		}
		if answer == "" {
			return nil
		}
	}
}

// Destroy: the host is the operator's box — uninstall never touches it.
func (p HostProvider) Destroy(_ context.Context, _ *provisioning.HostSession, _ string) error {
	return nil
}

// stripUser drops a "user@" prefix (the bare address for Host.IP).
func stripUser(host string) string {
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == '@' {
			return host[i+1:]
		}
	}
	return host
}
