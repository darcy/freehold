package cpbuild

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"freehold/agents"
	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/contract/identity"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/state"
	"freehold/platform/provisioning/planebase"
	dnsman "freehold/platform/services/externaldns/cloudflare"
)

// agentToolsRoot is the CP's durable agent-tools state dir (registry.json +
// facts.json), a sibling of the console state dir. Mirrors deployAgentTools.
func (s *Spec) agentToolsRoot() string {
	return filepath.Join(filepath.Dir(s.StateDir), "agent-tools")
}

// cpStateDir resolves the CP state root (the dir holding state.json): the
// Spec's own StateDir when it IS the CP root (the console executor's shape),
// or the sibling control-plane dir when the build runs INSIDE the agent-tools
// serve (whose StateDir is <root>/agent-tools) — the same layout
// agentToolsRoot() infers in reverse.
func (s *Spec) cpStateDir() string {
	if filepath.Base(s.StateDir) == "agent-tools" {
		return filepath.Join(filepath.Dir(s.StateDir), "control-plane")
	}
	return s.StateDir
}

// operatorTZ reads the operator settings' timezone from state.json FRESH per
// call (read-only — never a second writer): a console-side settings edit lands
// on the next pod apply in EITHER executor, with no serve restart and no
// serve-start flag to go stale. Empty/unreadable = pods run UTC.
func (s *Spec) operatorTZ() string {
	snap, err := state.LoadReadOnly(s.cpStateDir())
	if err != nil || snap.Settings == nil {
		return ""
	}
	return snap.Settings.OperatorTZ
}

// agentToolsAudience resolves the pubkey the agent pods' stdio bridge signs
// against: the AGENT-TOOLS server's own identity, read from the durable
// plane. NOT spec.Audience — for the console executor that is the console's
// runner-signing identity (its runner auth + the seed revoke), which only
// coincides with the agent-tools audience when the build runs inside the
// agent-tools serve itself. A pod manifest stamped with the wrong audience
// leaves the pod signing a dead key: every CP tool call fails "-32001
// signature does not verify" while the world otherwise looks healthy.
//
// The fallback when the durable identity is unreadable: the IN-SERVE spec's
// own Audience IS that identity (derived from the same state dir at boot).
// The console executor has no honest fallback — its Audience is the console's
// key — so it errors and the manifest/report fail loudly instead of stamping
// a dead audience back into the world.
func (s *Spec) agentToolsAudience() (string, error) {
	if id, err := identity.Load(s.agentToolsRoot()); err == nil {
		if pk, perr := id.NostrPubkeyHex(); perr == nil {
			return pk, nil
		}
	}
	if s.AgentIdentityDir == "" {
		return s.Audience, nil
	}
	return "", fmt.Errorf("durable agent-tools identity unreadable at %s", s.agentToolsRoot())
}

// relayDial is the relay DIAL URL for event publishes/queries: the relay's
// own hostname on the LAN HTTP port when known (buzz keys the community to
// the HOST header, and the relay client presents the dial URL's hostname — a
// raw-IP dial presents the IP and reads "no community is configured for this
// host"), the recorded RelayURL otherwise. The NIP-98 signature covers the
// CANONICAL public origin (RelayAuthURL) — the dial-LAN / sign-public split.
func (s *Spec) relayDial() string {
	if d := config.RelayLanDial(s.RelayHost); d != "" {
		return d
	}
	return s.RelayURL
}

// agentToolsServeFlags builds the `freehold-agent-tools serve` argv. Extracted
// from deployAgentTools so a registry/facts write can restart the serve process
// (and reload it) without re-running the one-time seed/grant/pin steps.
func (s *Spec) agentToolsServeFlags() string {
	atState := s.agentToolsRoot()
	relayDial := config.RelayLanDial(s.RelayHost)
	serveFlags := fmt.Sprintf(
		"--state-dir %s --addr 0.0.0.0:"+AgentToolsPort+" --relay-url %s --relay-lxc %d --relay-compose %s --k3s-vmid %d --runner-addr %s --runner-pubkey %s --runner-target %s --cpa-name %s --owner-pubkey %s --self-url %s",
		atState, relayDial, s.RelayLxc, s.RelayCompose, s.K3sVmid,
		s.RunnerAddr, s.RunnerPK, s.RunnerTarget, s.CpaName, s.OwnerPub, s.SelfURL)
	if n := strings.TrimSpace(s.OperatorName); n != "" {
		// B64, never raw: the name is operator free-form ("Darcy Smith",
		// "O'Brien") and this argv rides a root `sh -c '...'` — a raw space
		// silently eats every flag after it, a quote breaks the shell. B64
		// is one argv-safe token for any unicode.
		serveFlags += " --operator-name-b64 " + operatorNameArg(n)
	}
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
	if s.K3sIP != "" {
		serveFlags += " --k3s-ip " + s.K3sIP
	}
	if s.GatewayCIDR != "" {
		serveFlags += " --gateway-cidr " + s.GatewayCIDR
		serveFlags += fmt.Sprintf(" --gateway-vlan %d", s.GatewayVlan)
		serveFlags += fmt.Sprintf(" --gateway-lxc %d", s.GatewayLxc)
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

// startAgentTools (re)starts the CP-side agent-tools serve as a REAL systemd
// unit and waits for it to answer. The unit (enabled, Restart=on-failure)
// means a CP guest reboot brings agent-tools back with the console — the
// nohup shape died with the guest and stayed dead (the crash-restart gap).
// Every converge rewrites the unit (the flags can change); the restart is
// how a CP-side registry/facts write becomes visible to the running server
// (its rows live in memory, loaded at startup).
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
	// The pid-kill of a PRE-UNIT nohup process (older worlds): its serve.pid
	// is the only handle; once the unit runs, the file is absent and this is
	// a no-op.
	if err := s.run(fmt.Sprintf("pct exec %d -- sh -c 'p=$(cat %s/serve.pid 2>/dev/null); [ -n \"$p\" ] && kill \"$p\" >/dev/null 2>&1; rm -f %s/serve.pid; true'", s.CpLxc, atState, atState), 30); err != nil {
		return fmt.Errorf("agent-tools stop prior: %w", err)
	}
	unit := fmt.Sprintf(`[Unit]
Description=freehold agent-tools
After=network-online.target

[Service]
ExecStart=%s serve %s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, bin, s.agentToolsServeFlags())
	start := fmt.Sprintf(
		"pct exec %d -- sh -c 'echo %s | base64 -d > /etc/systemd/system/freehold-agent-tools.service && systemctl daemon-reload && systemctl enable freehold-agent-tools >/dev/null 2>&1 && systemctl restart freehold-agent-tools && sleep 1 && systemctl is-active freehold-agent-tools'",
		s.CpLxc, base64.StdEncoding.EncodeToString([]byte(unit)))
	if err := s.run(start, 60); err != nil {
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
		return fmt.Errorf("agent-tools serve did not answer within the poll window — journalctl -u freehold-agent-tools on the guest")
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
	// Pre-mint every core identity (CPA + departments) BEFORE staging any pod:
	// a department's respond-to allowlist names all core pubkeys, and the pods
	// apply in sequence — without this, a fresh world's early departments would
	// ship allowlists missing their not-yet-minted siblings. Idempotent.
	for _, name := range append([]string{cpa}, agents.DepartmentNames()...) {
		if _, err := agent.EnsureIdentity(filepath.Join(s.agentIdentityDir(), "agents", sanitizeDir(name))); err != nil {
			return fmt.Errorf("mint %s identity: %w", name, err)
		}
	}
	cpaPurpose := "the control plane agent — freehold's main reasoning touchpoint"
	// The CPA holds #freehold + the open #general first-run channel (the
	// desktop app's stock onboarding is skipped — stageOperatorProfile — so
	// #general is OURS to create; the CPA owns it and add-users the operator).
	if _, err := tools.CreateAgent(cpa, cpaPurpose, []string{"#freehold", "#general"}, false, agent.CoreLiteLLMModel); err != nil {
		return fmt.Errorf("create CPA over the registry: %w", err)
	}
	if err := reg.SetPurpose(cpa, cpaPurpose); err != nil {
		return err
	}
	departments := map[string]bool{cpa: true}
	for _, name := range agents.DepartmentNames() {
		purpose, _ := agents.DepartmentPurpose(name)
		channels := agents.DepartmentChannels(name)
		pub, err := tools.CreateAgent(name, purpose, channels, true, agent.CoreLiteLLMModel)
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
		if _, err := tools.CreateAgent(a.Name, a.Purpose, channels, private, a.Model); err != nil {
			return fmt.Errorf("reconcile created agent %s: %w", a.Name, err)
		}
		_ = reg.SetChannels(a.Name, channels, private)
		// Re-assert the model choice: RegisterAgent resets the row, so the
		// persisted alias has to ride back on (the pod was just re-applied
		// with it).
		_ = reg.SetModel(a.Name, a.Model)
		// Re-assert grants for agents holding capability runners (the
		// agent-provisioned dynamic doors re-assert alongside the departments';
		// a no-op when the agent holds none).
		if err := s.grantDepartmentRunner(a.Name, a.Pubkey); err != nil {
			return fmt.Errorf("re-assert runner grants %s: %w", a.Name, err)
		}
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
