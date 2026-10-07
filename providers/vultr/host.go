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
	// NO os_id ask: the image must be Debian (PVE installs on it via the
	// apt route) and the newest one comes from the API's own catalog —
	// the os ids drift, so an operator's guess is always worse.
	return []provisioning.HostNeed{
		{Name: "VULTR_API_KEY", Label: "Vultr API key (used for this install; never stored)", Secret: true},
		{Name: "region", Label: "Vultr region (the datacenter id — ewr, lax, ams, …)", Default: DefaultRegion},
		{Name: "plan", Label: "Vultr plan (the whole world lives on this host)", Default: DefaultPlan},
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
		return nil, fmt.Errorf("the vultr provider needs its API key (the guided flow prompts for it; a headless run reads VULTR_API_KEY from the env)")
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
	// The re-adopt resolves id+ip (or the mint creates them) — then BOTH
	// ride the same install phase: "alive" is not "ready", and the PVE
	// ensure is idempotent (an installed host short-circuits).
	var hostID, hostIP string
	if existingID != "" {
		status, ip, gerr := c.Instance(ctx, existingID)
		switch {
		case gerr == nil && status == "active" && ip != "" && ip != "0.0.0.0":
			s.Host = "root@" + ip
			s.Print("  vultr instance %s alive at %s — re-adopting it\n", existingID, ip)
			hostID, hostIP = existingID, ip
		case gerr != nil && strings.Contains(gerr.Error(), "HTTP 404"):
			s.Print("  vultr instance %s is gone — re-creating it\n", existingID)
		case gerr != nil:
			return nil, fmt.Errorf("cannot verify the recorded vultr instance %s: %w — refusing to mint a second one beside it; retry when the API answers", existingID, gerr)
		default:
			return nil, fmt.Errorf("vultr instance %s exists but has no address yet (status %q, still settling) — retry in a minute", existingID, status)
		}
	}
	if hostID == "" {
		// The image: derived from the API's own OS catalog (the newest Debian
		// x64), ALWAYS — no answer path. The ids drift (1743 is Ubuntu 22.04
		// on today's catalog) and a stale recorded answer would re-mint an
		// Ubuntu host forever; the catalog is the only authority.
		osID, derr := c.DebianOsID(ctx)
		if derr != nil {
			return nil, derr
		}

		// The ask is free-text — validate against the API's own catalog before
		// spending anything: a continent group name ("AMER") or a typo'd plan
		// is a 400 "Invalid datacenter." deep in the create otherwise.
		region := strings.ToLower(strings.TrimSpace(s.Answers["region"]))
		plan := strings.TrimSpace(s.Answers["plan"])
		if bad := checkIn(ctx, c, "region", region, c.Regions); bad != "" {
			return nil, fmt.Errorf("%s", bad)
		}
		if bad := checkIn(ctx, c, "plan", plan, c.Plans); bad != "" {
			return nil, fmt.Errorf("%s", bad)
		}
		// The instance label derives from the world's name — an unlabelled
		// instance is unfindable in the console when the stranded-handle
		// recovery points the operator at it.
		label := ""
		if s.World != "" {
			label = "freehold-" + s.World
		}
		key, kerr := c.EnsureSSHKey(ctx, "freehold-door", s.DoorLine)
		if kerr != nil {
			return nil, fmt.Errorf("vultr ssh-key: %w", kerr)
		}
		s.Print("  creating the vultr host (%s %s, Debian os_id %d)…\n", region, plan, osID)
		body := map[string]any{"region": region, "plan": plan, "os_id": osID}
		if label != "" {
			body["label"] = label
			body["hostname"] = label
		}
		id, cerr := c.CreateInstance(ctx, body, key)
		if cerr != nil {
			return nil, fmt.Errorf("vultr create: %w", cerr)
		}
		// The handle rides the session NOW — every later step can fail, and
		// a billed instance with an unrecorded id is the one unrecoverable
		// state.
		s.CreatedID = id
		s.Print("  instance %s — waiting for an address…\n", id)
		ip, werr := c.WaitActive(ctx, id, 8*time.Minute)
		if werr != nil {
			return nil, fmt.Errorf("instance %s: %w", id, werr)
		}
		hostID, hostIP = id, ip
		s.Host = "root@" + ip
	}
	s.Print("  instance live at %s — waiting for sshd, then installing PVE (apt route; this takes minutes)…\n", hostIP)
	// Vultr reports "active" before sshd answers its first boot (and
	// ExecOnHost does one dial, no retry) — poll until the host answers,
	// and fail the Prepare (naming the id) when it never does: limping
	// into the install would just fail there.
	if err := waitSSH(ctx, s, sshdWaitTimeout); err != nil {
		return nil, fmt.Errorf("instance %s: %w", hostID, err)
	}
	if err := s.ExecOnHost(PVEInstallScript(), 1800); err != nil {
		return nil, fmt.Errorf("pve install on instance %s: %w", hostID, err)
	}
	return &provisioning.Host{ID: hostID, IP: hostIP}, nil
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

// checkIn validates a free-text create field against the API's own catalog
// (Regions/Plans): a wrong answer is an actionable error naming the valid
// set, never a 400 deep in the create call. "" = valid.
func checkIn(ctx context.Context, c *Client, what, want string, list func(context.Context) ([]string, error)) string {
	if want == "" {
		return what + " is empty — the guided flow's default (or --host-answer " + what + "=…) supplies it"
	}
	got, err := list(ctx)
	if err != nil {
		// The catalog is unreachable (a local network hiccup, a rate
		// limit) — not the operator's mistake; let the create try anyway.
		return ""
	}
	for _, id := range got {
		if id == want {
			return ""
		}
	}
	return fmt.Sprintf("invalid vultr %s %q — the API knows: %s", what, want, strings.Join(got, ", "))
}
