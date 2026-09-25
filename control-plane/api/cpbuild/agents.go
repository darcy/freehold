package cpbuild

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"freehold/agents"
	"freehold/contract/console"
	"freehold/contract/identity"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	"freehold/platform/provisioning/planebase"
	dnsman "freehold/platform/services/externaldns/cloudflare"
)

// agentToolsRoot is the CP's durable agent-tools state dir (registry.json +
// facts.json), a sibling of the console state dir. Mirrors deployAgentTools.
func (s *Spec) agentToolsRoot() string {
	return filepath.Join(filepath.Dir(s.StateDir), "agent-tools")
}

// agentToolsAudience resolves the pubkey the agent pods' stdio bridge signs
// against: the AGENT-TOOLS server's own identity, read from the durable
// plane. NOT spec.Audience — for the console executor that is the console's
// runner-signing identity (its runner auth + the seed revoke), which only
// coincides with the agent-tools audience when the build runs inside the
// agent-tools serve itself. A pod manifest stamped with the wrong audience
// leaves the pod signing a dead key: every CP tool call fails "-32001
// signature does not verify" while the world otherwise looks healthy.
func (s *Spec) agentToolsAudience() string {
	if id, err := identity.Load(s.agentToolsRoot()); err == nil {
		if pk, perr := id.NostrPubkeyHex(); perr == nil {
			return pk
		}
	}
	return s.Audience
}

// agentToolsServeFlags builds the `freehold-agent-tools serve` argv. Extracted
// from deployAgentTools so a registry/facts write can restart the serve process
// (and reload it) without re-running the one-time seed/grant/pin steps.
func (s *Spec) agentToolsServeFlags() string {
	atState := s.agentToolsRoot()
	relayDial := "http://" + s.RelayHost + ":3000"
	serveFlags := fmt.Sprintf(
		"--state-dir %s --addr 0.0.0.0:"+AgentToolsPort+" --relay-url %s --relay-lxc %d --relay-compose %s --k3s-vmid %d --runner-addr %s --runner-pubkey %s --runner-target %s --cpa-name %s --owner-pubkey %s --self-url %s",
		atState, relayDial, s.RelayLxc, s.RelayCompose, s.K3sVmid,
		s.RunnerAddr, s.RunnerPK, s.RunnerTarget, s.CpaName, s.OwnerPub, s.SelfURL)
	if s.RelayPK != "" {
		serveFlags += " --relay-pubkey " + s.RelayPK
	}
	if s.RelayAuthURL != "" {
		serveFlags += " --relay-auth-url " + s.RelayAuthURL
	} else {
		serveFlags += " --relay-auth-url " + s.RelayURL
	}
	if s.RelayWS != "" {
		serveFlags += " --relay-ws " + s.RelayWS
	}
	if s.LitellmBaseURL != "" {
		serveFlags += " --litellm-base " + s.LitellmBaseURL
	}
	if s.PlanePool != "" {
		serveFlags += " --plane-pool " + s.PlanePool
	}
	if s.PlaneKind != "" {
		serveFlags += " --plane-kind " + s.PlaneKind
	}
	if s.ThinPool != "" {
		serveFlags += " --thin-pool " + s.ThinPool
	}
	if s.SizeGB != 0 {
		serveFlags += fmt.Sprintf(" --size-gb %d", s.SizeGB)
	}
	if s.PoolSizeGB != 0 {
		serveFlags += fmt.Sprintf(" --pool-size-gb %d", s.PoolSizeGB)
	}
	if s.RootfsGB != 0 {
		serveFlags += fmt.Sprintf(" --rootfs-gb %d", s.RootfsGB)
	}
	if s.MemoryMB != 0 {
		serveFlags += fmt.Sprintf(" --memory-mb %d", s.MemoryMB)
	}
	if s.StorageName != "" {
		serveFlags += " --storage " + s.StorageName
	}
	if s.RelayGW != "" {
		serveFlags += " --relay-gw " + s.RelayGW
	}
	if s.Bridge != "" {
		serveFlags += " --bridge " + s.Bridge
	}
	if s.RepoURL != "" {
		serveFlags += " --repo-url " + s.RepoURL
	}
	if s.CpLxc != 0 {
		serveFlags += fmt.Sprintf(" --cp-lxc %d", s.CpLxc)
	}
	if s.ProxyIP != "" {
		serveFlags += " --proxy-ip " + s.ProxyIP
	}
	if s.LitellmIP != "" {
		serveFlags += " --litellm-ip " + s.LitellmIP
	}
	if s.RelayHost != "" {
		serveFlags += " --relay-host " + s.RelayHost
	}
	if s.RelayIP != "" {
		serveFlags += " --relay-ip " + s.RelayIP
	}
	if s.CpHost != "" {
		serveFlags += " --cp-host " + s.CpHost
	}
	if s.CpIP != "" {
		serveFlags += " --cp-ip " + s.CpIP
	}
	return serveFlags
}

// startAgentTools (re)launches the CP-side agent-tools serve process in the cp
// guest and waits for it to answer. The process loads its registry.json /
// facts.json at startup, so a restart is how a CP-side registry/facts write
// becomes visible to the running server (its rows live in memory).
func (s *Spec) startAgentTools() error {
	// Re-assert the relay-host pin EVERY converge: /etc/hosts is PVE-ephemeral
	// (a rebuilt CP guest loses it), the full-deploy path that drops the pin
	// only runs on a fresh/changed agent-tools stage, and the console's relay
	// publishes + this serve's roster queries dial the relay by HOSTNAME —
	// without the pin they resolve to the Caddy EDGE record first.
	if err := s.pinRelayHost(); err != nil {
		return fmt.Errorf("agent-tools relay pin: %w", err)
	}
	binDir, _ := s.cpGuestDirs()
	bin := binDir + "/freehold-agent-tools"
	atState := s.agentToolsRoot()
	if err := s.run(fmt.Sprintf("pct exec %d -- sh -c 'p=$(cat %s/serve.pid 2>/dev/null); [ -n \"$p\" ] && kill \"$p\" >/dev/null 2>&1; rm -f %s/serve.pid; true'", s.CpLxc, atState, atState), 30); err != nil {
		return fmt.Errorf("agent-tools stop prior: %w", err)
	}
	start := fmt.Sprintf(
		"pct exec %d -- sh -c 'setsid nohup %s serve %s >> %s/serve.log 2>&1 < /dev/null & echo $! | tee %s/serve.pid'",
		s.CpLxc, bin, s.agentToolsServeFlags(), atState, atState)
	if err := s.run(start, 30); err != nil {
		return fmt.Errorf("start agent-tools: %w", err)
	}
	up := false
	for i := 0; i < 15; i++ {
		code, err := s.runOut(fmt.Sprintf("pct exec %d -- curl -s -m 3 -o /dev/null -w %%{http_code} http://127.0.0.1:"+AgentToolsPort+"/mcp", s.CpLxc), 15)
		if err == nil && strings.TrimSpace(code) != "000" {
			up = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !up {
		return fmt.Errorf("agent-tools serve did not answer within the poll window — check %s/serve.log", atState)
	}
	return nil
}

// cpaNameOrDefault returns the configured CPA name or the default.
func (s *Spec) cpaNameOrDefault() string {
	if s.CpaName != "" {
		return s.CpaName
	}
	return agent.DefaultCPAName
}

// reconcileAgents is the CP-owned agent org reconcile (the server-side home of
// the box's old stageCpa / stageDepartments / reconcileCreatedAgents): create
// the CPA first, then the four departments, then re-create every other registry
// row so a rebuild reseats pods with the same durable identity. All create
// calls run in-process through BuildCreateAgentFn and register into the CP's
// durable registry.
func (s *Spec) reconcileAgents() error {
	regPath := filepath.Join(s.agentToolsRoot(), "registry.json")
	reg, err := agenttools.OpenRegistry(regPath)
	if err != nil {
		return fmt.Errorf("open agent registry: %w", err)
	}
	return s.reconcileAgentsInto(reg)
}

// reconcileAgentsInto is reconcileAgents against an already-open registry (the
// running agent-tools server's in-process instance, when build runs there).
func (s *Spec) reconcileAgentsInto(reg *agenttools.Registry) error {
	if s.CpaName == "" {
		s.CpaName = agent.DefaultCPAName
	}
	tools := &agent.Tools{Console: reg, Create: BuildCreateAgentFn(s)}
	cpa := s.cpaNameOrDefault()
	cpaPurpose := "the control plane agent — freehold's main reasoning touchpoint"
	if _, err := tools.CreateAgent(cpa, cpaPurpose, nil, false); err != nil {
		return fmt.Errorf("create CPA over the registry: %w", err)
	}
	if err := reg.SetPurpose(cpa, cpaPurpose); err != nil {
		return err
	}
	departments := map[string]bool{cpa: true}
	for _, name := range agents.DepartmentNames() {
		purpose, _ := agents.DepartmentPurpose(name)
		channels := agents.DepartmentChannels(name)
		pub, err := tools.CreateAgent(name, purpose, channels, true)
		if err != nil {
			return fmt.Errorf("create department %s: %w", name, err)
		}
		_ = reg.SetPurpose(name, purpose)
		_ = reg.SetChannels(name, channels, true)
		// Grant the department's identity onto its capability runner (a no-op
		// for a department without one). This is the raw capability grant — it
		// attaches to the department identity, never to a custom agent.
		if err := s.grantDepartmentRunner(name, pub); err != nil {
			return fmt.Errorf("grant department runner %s: %w", name, err)
		}
		departments[name] = true
	}
	rows, err := reg.Agents()
	if err != nil {
		return err
	}
	for _, a := range rows {
		if a.Name == "" || departments[a.Name] {
			continue
		}
		channels, private := reconciledChannels(a)
		if _, err := tools.CreateAgent(a.Name, a.Purpose, channels, private); err != nil {
			return fmt.Errorf("reconcile created agent %s: %w", a.Name, err)
		}
		_ = reg.SetChannels(a.Name, channels, private)
	}
	return nil
}

// reconciledChannels derives an agent's channel list + visibility: a reserved
// department re-derives its fixed channels; any other agent rejoins the full
// list the registry preserved, falling back to the single recorded channel for
// rows written before Channels was persisted.
func reconciledChannels(a console.AgentInfo) ([]string, bool) {
	if channels := agents.DepartmentChannels(a.Name); channels != nil {
		return channels, true
	}
	channels := a.Channels
	private := a.Private
	if len(channels) == 0 && strings.TrimSpace(a.Channel) != "" {
		channels = []string{a.Channel}
	}
	return channels, private
}

// registerWorldFactsServer pushes the deployer-side world facts onto the CP's
// durable facts store (the server-side home of the box's registerWorldFacts):
// domains, the durable-plane layout, and the edge cert metadata, so any box's
// DATA/Certs views resolve from the CP.
func (s *Spec) registerWorldFactsServer(mounts map[planebase.Tenant][]planebase.MountSpec) error {
	path := filepath.Join(s.agentToolsRoot(), "facts.json")
	fs, err := agenttools.OpenFacts(path)
	if err != nil {
		return fmt.Errorf("open world facts: %w", err)
	}
	return s.registerWorldFactsInto(fs, mounts)
}

// registerWorldFactsInto is registerWorldFactsServer against an already-open
// facts store (the running agent-tools server's in-process instance).
func (s *Spec) registerWorldFactsInto(fs *agenttools.FactsStore, mounts map[planebase.Tenant][]planebase.MountSpec) error {
	facts := agenttools.WorldFacts{
		Domains: agenttools.WorldDomains{
			Relay: s.RelayHost,
			CP:    s.CpHost,
			Proxy: s.ProxyIP,
		},
		Plane: agenttools.WorldPlane{
			Backend:     "pve",
			BackendKind: s.PlaneKind,
			ThinPool:    s.ThinPool,
		},
	}
	for tenant, mts := range mounts {
		for _, m := range mts {
			facts.Plane.Mounts = append(facts.Plane.Mounts, agenttools.WorldPlaneMount{
				Tenant: tenant.String(), Source: m.Source, GuestPath: m.GuestPath, Backup: true,
			})
		}
	}
	issuer := "lego (DNS-01)"
	for _, slot := range []struct{ slot, host string }{
		{"relay", s.RelayHost}, {"cp", s.CpHost},
	} {
		if slot.host == "" {
			continue
		}
		facts.Certs = append(facts.Certs, agenttools.WorldCert{
			Slot: slot.slot, Domain: slot.host, Issuer: issuer,
			Expiry: s.certExpiryFromDurable(slot.slot),
		})
	}
	return fs.Register(facts)
}

// certExpiryFromDurable reads a slot's edge cert notAfter from the durable
// mirror on the k3s node, as RFC3339 ("" when unreadable).
func (s *Spec) certExpiryFromDurable(slot string) string {
	raw, err := s.durableFullchain(s.K3sVmid, slot)
	if err != nil {
		return ""
	}
	var notAfter time.Time
	for {
		block, rest := pem.Decode(raw)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			if c, err := x509.ParseCertificate(block.Bytes); err == nil {
				notAfter = c.NotAfter
				break
			}
		}
		raw = rest
	}
	if notAfter.IsZero() {
		return ""
	}
	return notAfter.UTC().Format(time.RFC3339)
}

// manageDomainDNS upserts the public A records (relay/cp -> proxy) on the CP's
// stored DNS credential (the server-side home of the box's manageDomainDNS).
// Best-effort at build time: no edge, no proxy IP, or no stored relay cred is a
// no-op; a provider other than Cloudflare is reported and skipped.
func (s *Spec) manageDomainDNS() error {
	if s.ProxyIP == "" || s.RelayHost == "" {
		return nil
	}
	provider, env, err := s.dnsCredFromStore("relay")
	if err != nil {
		// No credential stored yet (manual DNS) — nothing to manage.
		return nil
	}
	if provider != "cloudflare" {
		// A non-Cloudflare credential is valid for cert DNS-01; A-record
		// management is Cloudflare-only, so skip it rather than fail the build.
		return nil
	}
	m, err := dnsman.For(provider, env)
	if err != nil {
		return err
	}
	ip := s.ProxyIP
	for _, host := range []string{s.RelayHost, s.CpHost} {
		if host == "" {
			continue
		}
		if err := m.UpsertA(host, ip); err != nil {
			return fmt.Errorf("manage DNS for %s: %w", host, err)
		}
	}
	return nil
}
