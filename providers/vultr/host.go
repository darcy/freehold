package vultr

import (
	"context"
	"fmt"
	"strings"
	"time"

	"freehold/platform/provisioning"
)

// DefaultRegion and DefaultPlan are the guided flow's defaults: the whole
// world lives on this one host (CP + relay + k3s + agent pods on top).
const (
	DefaultRegion = "ewr"
	DefaultPlan   = "vc2-4c-8gb"
)

// HostProvider is the vultr host-provisioning seam: it CREATES the world's
// host — a Vultr instance born with the door key authorized, PVE installed
// on it (the apt route, the chunk-2.5 spike's recipe) — and destroys it.
type HostProvider struct{}

func (HostProvider) Name() string       { return "vultr" }
func (HostProvider) AccessMode() string { return "api-vultr" }

// HostsGateway: the public IP belongs to the instance's NIC — no gateway
// guest can hold it, so the host itself is the gateway (host nftables with
// an input policy, the internal bridge, the subnet resolver).
func (HostProvider) HostsGateway() bool { return true }

func (HostProvider) Needs() []provisioning.HostNeed {
	return []provisioning.HostNeed{
		{Name: "VULTR_API_KEY", Label: "Vultr API key (used for this install; never stored)", Secret: true},
		{Name: "region", Label: "Vultr region", Default: DefaultRegion},
		{Name: "plan", Label: "Vultr plan (the whole world lives on this host)", Default: DefaultPlan},
		{Name: "os_id", Label: "Vultr os_id (Debian release)", Default: fmt.Sprintf("%d", DefaultOsID)},
	}
}

func (HostProvider) Defaults() map[string]string {
	// A cloud VPS has no local-lvm: the dir storage `local` hosts the
	// rootfs. The bridge keeps the codebase's name (lxc.sh + main.tf assume
	// it); relay_gw is meaningless here (guests' default route is the host
	// bridge's .1).
	return map[string]string{"storage": "local", "bridge": "vmbr0"}
}

// clientForBuilder is the session->client constructor; a package var so the
// hermetic tests can point it at an httptest server.
var clientForBuilder = defaultClientFor

func defaultClientFor(s *provisioning.HostSession) (*Client, error) {
	tok := strings.TrimSpace(s.Answers["VULTR_API_KEY"])
	if tok == "" {
		return nil, fmt.Errorf("the vultr provider needs its API key (prompt it, or set VULTR_API_KEY for headless runs)")
	}
	return &Client{Token: tok}, nil
}

// clientFor builds the API client from the session's answers. The key is
// never stored anywhere but the session.
func clientFor(s *provisioning.HostSession) (*Client, error) {
	return clientForBuilder(s)
}

// Prepare mints or re-adopts the host: a recorded instance is verified
// alive (reused) or — only when definitively 404 — re-created; every other
// lookup failure fails loudly (a timeout, a 429, a still-settling IP all
// mean the instance EXISTS and billing, and minting a second one beside it
// strands the plane). After create: wait for the address, wait for SSH,
// run the PVE install through the session's transport.
func (p HostProvider) Prepare(ctx context.Context, s *provisioning.HostSession, existingID string) (*provisioning.Host, error) {
	if s.ExecOnHost == nil {
		return nil, fmt.Errorf("the vultr provider needs the session's host transport (ExecOnHost)")
	}
	c, err := clientFor(s)
	if err != nil {
		return nil, err
	}
	if existingID != "" {
		status, ip, gerr := c.Instance(ctx, existingID)
		switch {
		case gerr == nil && status == "active" && ip != "" && ip != "0.0.0.0":
			s.Host = "root@" + ip
			s.Print("  vultr instance %s alive at %s — re-adopting it\n", existingID, ip)
			return &provisioning.Host{ID: existingID, IP: ip}, nil
		case gerr != nil && strings.Contains(gerr.Error(), "HTTP 404"):
			s.Print("  vultr instance %s is gone — re-creating it\n", existingID)
		case gerr != nil:
			return nil, fmt.Errorf("cannot verify the recorded vultr instance %s: %w — refusing to mint a second one beside it; retry when the API answers", existingID, gerr)
		default:
			return nil, fmt.Errorf("vultr instance %s exists but has no address yet (status %q, still settling) — retry in a minute", existingID, status)
		}
	}

	region, plan := s.Answers["region"], s.Answers["plan"]
	label := s.Answers["label"]
	key, err := c.EnsureSSHKey(ctx, "freehold-door", s.DoorLine)
	if err != nil {
		return nil, fmt.Errorf("vultr ssh-key: %w", err)
	}
	s.Print("  creating the vultr host (%s %s)…\n", region, plan)
	id, err := c.CreateInstance(ctx, region, plan, parseOsID(s.Answers["os_id"]), label, key)
	if err != nil {
		return nil, fmt.Errorf("vultr create: %w", err)
	}
	// The handle rides the session NOW — every later step can fail, and a
	// billed instance with an unrecorded id is the one unrecoverable state.
	s.CreatedID = id
	s.Print("  instance %s — waiting for an address…\n", id)
	ip, err := c.WaitActive(ctx, id, 8*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("instance %s: %w", id, err)
	}
	s.Host = "root@" + ip
	s.Print("  instance live at %s — waiting for sshd, then installing PVE (apt route; this takes minutes)…\n", ip)
	// Vultr reports "active" before sshd answers its first boot (and
	// ExecOnHost does one dial, no retry) — poll until the host answers,
	// and fail the Prepare (naming the id) when it never does: limping
	// into the install would just fail there.
	if err := waitSSH(ctx, s, sshdWaitTimeout); err != nil {
		return nil, fmt.Errorf("instance %s: %w", id, err)
	}
	if err := s.ExecOnHost(PVEInstallScript(), 1800); err != nil {
		return nil, fmt.Errorf("pve install on instance %s: %w", id, err)
	}
	return &provisioning.Host{ID: id, IP: ip}, nil
}

// sshdWaitTimeout is how long Prepare waits for the host's first sshd
// answer; a package var so the hermetic tests shrink it.
var sshdWaitTimeout = 10 * time.Minute

// waitSSH polls until the session's host answers over the door transport.
func waitSSH(ctx context.Context, s *provisioning.HostSession, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if s.ExecOnHost("echo ssh-ok", 30) == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the host never answered SSH within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// InstallDoorKey appends the substrate door line over the DOOR key (the
// instance was born with it authorized). Idempotent.
func (HostProvider) InstallDoorKey(s *provisioning.HostSession, key string) error {
	if key == "" {
		return nil
	}
	if s.ExecOnHost == nil {
		return fmt.Errorf("no host transport for the door install")
	}
	cmd := fmt.Sprintf(`mkdir -p /root/.ssh && chmod 700 /root/.ssh && touch /root/.ssh/authorized_keys && (grep -qF '%s' /root/.ssh/authorized_keys || echo '%s' >> /root/.ssh/authorized_keys)`, key, key)
	if err := s.ExecOnHost(cmd, 60); err != nil {
		return fmt.Errorf("door install: %w", err)
	}
	return nil
}

// Destroy deletes the instance (404 counts as destroyed). A MISSED destroy
// keeps billing — the client retries transient answers and fails loudly.
func (p HostProvider) Destroy(ctx context.Context, s *provisioning.HostSession, id string) error {
	c, err := clientFor(s)
	if err != nil {
		return err
	}
	return c.Destroy(ctx, id)
}

// parseOsID accepts a bare number; "" or garbage = the default.
func parseOsID(s string) uint32 {
	var n uint32
	for _, c := range s {
		if c < '0' || c > '9' {
			return DefaultOsID
		}
		n = n*10 + uint32(c-'0')
	}
	if n == 0 {
		return DefaultOsID
	}
	return n
}
