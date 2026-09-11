package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"freehold/contract/config"
	"freehold/control-plane/api/agenttools"
)

// ---- bubbletea lifecycle -------------------------------------------------

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{} })}
	if m.HasConfig && m.activity == nil {
		// the world check streams in the activity view instead of blocking
		// the first frame (the old synchronous load hung for every probe
		// timeout before anything rendered).
		cmds = append(cmds, m.startBootActivity("checking the world"))
	}
	// A persisted operator identity means we can already drive the remote CP
	// (Runners-CP, Agents, provision/grant/revoke, the web portal): re-login
	// silently so those views are authorized without pressing `l` each time.
	if cmd := m.autoLoginCmd(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

type tickMsg struct{}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// freeholdStateDir mirrors installer::state_dir (FREEHOLD_HOME override).
func freeholdStateDir() string {
	if h := os.Getenv("FREEHOLD_HOME"); h != "" {
		return h + "/control-plane"
	}
	return envOr("HOME", "/root") + "/.freehold/control-plane"
}

// load is the FAST half of startup: it reads the config (local file only —
// no network) so the first frame renders instantly. The liveness probes
// (relay, runner, CP, k3s) run AFTERWARD, streamed through the activity
// view by Init — the dashboard never hangs on a probe timeout.
func (m *Model) load(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		m.Err = "config: " + err.Error()
		return nil
	}
	if cfg == nil {
		m.Mode, m.HasConfig = ModeBootstrap, false
		return nil
	}
	m.HasConfig = true
	m.Domain = cfg.RelayHost()
	m.Converged, m.RelayLive, m.CPLive, m.K3sLive, m.LitellmLive, m.CaddyLive, m.RunnerReach = false, false, false, false, false, false, false
	// ModeRunning until the boot check proves otherwise — the activity view
	// covers the screen while the probes run, and the check settles the
	// real mode (its k3s/world-state steps populate the dashboard rows).
	m.Mode = ModeRunning
	m.buildServices(cfg)
	m.buildCerts(cfg)
	m.cfg = cfg
	m.refreshRunners(cfg)
	m.buildAgents(cfg)
	return nil
}

// cpLive pings the CP console's /healthz from INSIDE its LXC, through the
// provisioning runner (the console binds loopback and the public URL fronts
// the operator's proxy — so URL reachability is not the probe; the guest's
// OWN service must answer 200). Mirrors Rust cp_live; failure = down.
func cpLive(cfg *config.Config) bool {
	if cfg.Lxc.Cp.Vmid == nil || cfg.Runner.Addr == "" {
		return false
	}
	inner := "exec 3<>/dev/tcp/127.0.0.1/8080; printf \"GET /healthz HTTP/1.0\\r\\n\\r\\n\" >&3; grep -m1 \"^HTTP\" <&3 || true"
	out, err := runSelf("exec", "--addr", cfg.Runner.Addr,
		"--agent-dir", freeholdStateDir()+"/agent-ops",
		"--runner-pubkey", cfg.Runner.Pubkey,
		"--timeout", "10",
		cfg.Runner.Target,
		fmt.Sprintf("pct exec %d -- bash -c '%s'", *cfg.Lxc.Cp.Vmid, inner))
	if err != nil {
		return false
	}
	return strings.Contains(out, " 200 ") || strings.Contains(out, "200 OK")
}

// cpConsoleLive is the login-only-box CP liveness probe: a NIP-98 console
// session that answers /api/overview means the CP is up. A failed overview = down
// (an established session that stops answering is a dead CP, not a live one). No
// session yet (auto-login hasn't landed) falls back to bare TCP reachability of
// the CP host, so a fresh box can see its CP is up without a local runner.
func cpConsoleLive(m *Model, cfg *config.Config) bool {
	if m.console != nil && m.console.client != nil {
		// An established session is authoritative: a failed overview is a real
		// down — never fall through to the TCP reachability shortcut.
		ov, err := m.console.client.Overview()
		return err == nil && ov != nil
	}
	// No session yet — bare TCP reachability, the only signal a fresh box has
	// before auto-login lands. The honest session check is the primary path.
	return cfg != nil && cfg.CPURL != "" && config.URLReachable(cfg.CPURL)
}

// buildServices fills the Services view. With a CP session the CP is the single
// source of truth — relay + control plane + each world service the CP monitors,
// identically for every box. The config's `managed` pieces are only the offline
// fallback (no CP session).
func (m *Model) buildServices(cfg *config.Config) {
	m.Services = nil
	if m.cpWorld != nil && len(m.cpWorld.Services) > 0 {
		m.buildCpServices(cfg)
		if len(m.Services) == 0 {
			m.Services = []ServiceRow{{Name: "(none managed)", Status: styleDim.Render("add `managed` entries to config")}}
		}
		return
	}
	for _, piece := range cfg.Managed {
		row := ServiceRow{Name: piece}
		switch piece {
		case "relay":
			row.Where = guestLocation(cfg.Lxc.Relay.Vmid, cfg.Lxc.Relay.Ip)
			row.URL = cfg.RelayURL
			row.Status = boolStatus(m.RelayLive, "live", "down")
		case "cp":
			row.Name = "control plane"
			row.Where = guestLocation(cfg.Lxc.Cp.Vmid, cfg.Lxc.Cp.Ip)
			row.URL = cfg.CPURL
			row.Status = boolStatus(m.CPLive, "live", "down")
		case "k3s":
			row.Name = "k3s (kube)"
			row.Where = guestLocation(cfg.Lxc.K3s.Vmid, cfg.Lxc.K3s.Ip)
			if cfg.Lxc.K3s.Ip != nil {
				row.URL = "https://" + config.StripCIDR(*cfg.Lxc.K3s.Ip) + ":6443"
			} else {
				row.URL = "—"
			}
			row.Status = boolStatus(m.K3sLive, "live", "down")
		case "litellm":
			row.Name = "litellm (gateway)"
			if cfg.Litellm.Host != "" {
				row.Where = "kube · " + cfg.Litellm.Host
			} else {
				row.Where = "kube · (no coords — not deployed this run)"
			}
			row.URL = cfg.Litellm.URL
			row.Status = boolStatus(m.LitellmLive, "live", "down")
		case "caddy":
			row.Name = "caddy (TLS edge)"
			if cfg.Caddy.Host != "" {
				row.Where = "kube · " + cfg.Caddy.Host
			} else {
				row.Where = "kube · (no coords — not deployed this run)"
			}
			row.URL = cfg.Caddy.URL
			row.Status = boolStatus(m.CaddyLive, "live", "down")
		default:
			row.Where = "managed"
			row.Status = "—"
		}
		m.Services = append(m.Services, row)
	}
	if len(m.Services) == 0 {
		m.Services = []ServiceRow{{Name: "(none managed)", Status: styleDim.Render("add `managed` entries to config")}}
	}
	// The CP resolver's explicit records (C0 DNS panel). The live probe	// (dns activity step) sets m.DNS — the CP is authoritative. This render
	// stays PURE (no exec): it falls back to the config mirror ONLY when the
	// live probe has never produced a snapshot.
	if len(m.DNS) == 0 {
		for name, ip := range cfg.Dns.Records {
			m.DNS = append(m.DNS, DnsRow{Name: name, IP: ip, Source: "config mirror"})
		}
	}
}

// buildCpServices renders the Services view for EVERY box once it has a CP
// session, from the CP's own /api/world services report — relay + control plane
// + each world service (k3s/litellm/caddy), URL and health all CP-provided.
// No local config row appears; local config is only the offline (no-session)
// fallback. This is the "read the world from the CP" contract.
func (m *Model) buildCpServices(cfg *config.Config) {
	m.Services = nil
	for _, s := range m.cpWorld.Services {
		m.Services = append(m.Services, ServiceRow{
			Name:   serviceRowName(s.Kind),
			Where:  "CP-served · " + s.Kind,
			URL:    s.URL,
			Status: boolStatus(s.Up, "live", "down"),
		})
	}
	if len(m.Services) == 0 {
		m.Services = []ServiceRow{{Name: "(none managed)", Status: styleDim.Render("add `managed` entries to config")}}
	}
}

func serviceRowName(kind string) string {
	switch kind {
	case "relay":
		return "relay"
	case "cp":
		return "control plane"
	case "k3s":
		return "k3s (kube)"
	case "litellm":
		return "litellm (gateway)"
	case "caddy":
		return "caddy (TLS edge)"
	default:
		return kind
	}
}

// buildCerts fills the Certs view from the config's recorded per-host edge certs
// (written by the F3 stage on rebuild). Pure — no exec. Two rows (relay, cp),
// each deriving its status from that slot's expiry.
// relaySlotHost is the Certs view's relay host: the box's resolved public relay
// domain (m.Domain — CP/DNS-derived on a management box, config-derived on a
// deployer), or the config host when m.Domain isn't populated.
func relaySlotHost(m *Model, cfg *config.Config) string {
	if m.Domain != "" {
		return m.Domain
	}
	return cfg.RelayHost()
}

func (m *Model) buildCerts(cfg *config.Config) {
	m.Certs = nil
	// The CP's world facts carry the edge cert metadata (registered at build),
	// so a management/login-only box renders the Certs view from the CP.
	if m.Facts != nil && len(m.Facts.Certs) > 0 {
		for _, c := range m.Facts.Certs {
			status, expiry := "no expiry on record", "—"
			if c.Expiry != "" {
				expiry = c.Expiry
				if t, err := time.Parse(time.RFC3339, c.Expiry); err == nil {
					switch {
					case t.Before(time.Now()):
						status = styleRed.Render("EXPIRED")
					case t.Before(time.Now().Add(30 * 24 * time.Hour)):
						status = styleYellow.Render("expiring <30d")
					default:
						status = styleGreen.Render("valid")
					}
				}
			}
			m.Certs = append(m.Certs, CertRow{
				Domain: c.Domain,
				URL:    "https://" + c.Domain,
				Expiry: expiry,
				Issuer: c.Issuer,
				Status: status,
			})
		}
		if len(m.Certs) == 0 {
			m.Certs = []CertRow{{Domain: "(no cert on record)", Status: styleDim.Render("rebuild stages the wildcard cert for the edge")}}
		}
		return
	}
	// Fallback: the deployer's local config (pre-facts or offline).
	issuer := cfg.Caddy.CertIssuer
	if issuer == "" {
		issuer = "lego (DNS-01)"
	}
	slots := []struct {
		name, host, expiry string
	}{
		// relay host: m.Domain is the public relay host (config-derived on a
		// deployer box; CP-relay_host- or DNS-derived on a management box),
		// falling back to the config host — so a management box shows the
		// domain, not the LAN IP it connected to.
		{"relay", relaySlotHost(m, cfg), cfg.Caddy.RelayCert},
		{"control plane", cfg.CPHost(), cfg.Caddy.CPCert},
	}
	for _, s := range slots {
		status, expiry := "no expiry on record", "—"
		if s.expiry != "" {
			expiry = s.expiry
			if t, err := time.Parse(time.RFC3339, s.expiry); err == nil {
				switch {
				case t.Before(time.Now()):
					status = styleRed.Render("EXPIRED")
				case t.Before(time.Now().Add(30 * 24 * time.Hour)):
					status = styleYellow.Render("expiring <30d")
				default:
					status = styleGreen.Render("valid")
				}
			}
		}
		m.Certs = append(m.Certs, CertRow{
			Domain: s.host,
			URL:    caddyURL(m, cfg, s.name),
			Expiry: expiry,
			Issuer: issuer,
			Status: status,
		})
	}
}

// caddyURL returns the edge URL for a service (relay -> the Caddy URL / relay
// host; control plane -> the cp host), falling back to the bare host. The relay
// fallback uses the resolved public relay domain (m.Domain — CP/DNS-derived on
// a management box), not the LAN IP the box connected to.
func caddyURL(m *Model, cfg *config.Config, name string) string {
	if name == "control plane" {
		if cfg.CPURL != "" {
			return cfg.CPURL
		}
		return "https://" + cfg.CPHost()
	}
	if cfg.Caddy.URL != "" {
		return cfg.Caddy.URL
	}
	return "https://" + relaySlotHost(m, cfg)
}

// dnsRowsLive execs `control-plane dns list` inside the cp LXC through the
// signed runner channel (same pattern as cpLive) and parses the records.
// Failure or empty output = (nil) — the caller falls back to the mirror.
func dnsRowsLive(cfg *config.Config) []DnsRow {
	if cfg.Lxc.Cp.Vmid == nil || cfg.Runner.Addr == "" {
		return nil
	}
	inner := "'/srv/data/cp/bin/control-plane' dns --state-dir '/srv/data/cp/control-plane' list"
	out, err := runSelf("exec", "--addr", cfg.Runner.Addr,
		"--agent-dir", freeholdStateDir()+"/agent-ops",
		"--runner-pubkey", cfg.Runner.Pubkey,
		"--timeout", "15",
		cfg.Runner.Target,
		fmt.Sprintf("pct exec %d -- sh -c %s", *cfg.Lxc.Cp.Vmid, "'"+inner+"'"))
	if err != nil {
		return nil
	}
	return parseDnsList(out)
}

// parseDnsList extracts `name ip` pairs from the RECORDS TABLE of
// `control-plane dns list` output: the indented metadata lines and the
// "--- addn-hosts ---" section (ip-first lines) are both skipped. The
// wildcard apex line is captured as `*.<apex>`. Pure.
func parseDnsList(out string) []DnsRow {
	var rows []DnsRow
	for _, l := range strings.Split(out, "\n") {
		if l == "" || l[0] == ' ' || l[0] == '\t' || strings.HasPrefix(l, "---") ||
			strings.HasPrefix(l, "(no dns") {
			continue
		}
		f := strings.Fields(l)
		if len(f) > 0 && f[0][0] == '-' {
			continue
		}
		if strings.HasPrefix(l, "wildcard") {
			if len(f) >= 3 && f[1] != "(none)" {
				rows = append(rows, DnsRow{Name: f[1], IP: f[2], Source: "wildcard (live)"})
			}
			continue
		}
		if len(f) >= 2 && !strings.Contains(f[0], ".") {
			rows = append(rows, DnsRow{Name: f[0], IP: f[1], Source: "resolver (live)"})
		}
	}
	return rows
}

// guestLocation mirrors the Rust location column: "LXC 100 · 1.2.3.4".
func guestLocation(vmid *uint32, ip *string) string {
	switch {
	case vmid != nil && ip != nil:
		return fmt.Sprintf("LXC %d · %s", *vmid, config.StripCIDR(*ip))
	case vmid != nil:
		return fmt.Sprintf("LXC %d", *vmid)
	case ip != nil:
		return config.StripCIDR(*ip)
	default:
		return "—"
	}
}

// refreshData rebuilds the DATA view. The CP's world facts are the single
// source — identical for EVERY box (the durable-plane layout registered at
// build). No local plane probe: a bootstrap box and a login box render the
// same CP facts, so the boot check never diverges or hangs on a local exec.
func (m *Model) refreshData(cfg *config.Config) {
	if m.Facts == nil || len(m.Facts.Plane.Mounts) == 0 {
		m.DataAt = time.Now()
		m.Storage = []DataRow{{Role: "(no plane mounts)", Source: "CP facts"}}
		return
	}
	m.DataAt = time.Now()
	m.Storage = nil
	for _, mu := range m.Facts.Plane.Mounts {
		size, used, fill := mu.Size, mu.Used, mu.Fill
		if size == "" {
			size = "—"
		}
		if used == "" {
			used = "—"
		}
		if fill == "" {
			fill = "—"
		}
		m.Storage = append(m.Storage, DataRow{
			Role: mu.Tenant, Mount: mu.GuestPath, Size: size, Used: used, Fill: fill,
			Source: mu.Source, Live: "CP facts",
		})
	}
	if len(m.Storage) == 0 {
		m.Storage = []DataRow{{Role: "(no mounts)"}}
	}
	m.Msg = "data layout from the CP"
}

// vmidForRole maps a plane mount role to its LXC vmid (mirror of Rust
// plane_info's role match).
// launchWeb opens the console's portal URL in the browser (mirrors Rust
// launch_web): needs a session (l); xdg-open absence degrades to the
// manual-URL notice (single-use token, 60s).
func (m *Model) launchWeb() {
	if m.console == nil || m.console.client == nil {
		m.Msg = "not logged in — press l first (the web needs a session)"
		return
	}
	url, err := m.console.client.PortalURL()
	if err != nil {
		m.Msg = "web launch failed: " + err.Error()
		return
	}
	cmd := exec.Command("xdg-open", url)
	if err := cmd.Start(); err != nil {
		m.Msg = "no browser launcher (xdg-open: " + err.Error() + ") — open manually: " + url
		return
	}
	m.Msg = "web opened: " + url
}

// refreshRunners fills the Runners view from the console /api/overview (the
// CP's authoritative runner list). There is no local loopback toggle: the
// box-local state.json mirror is deleted (Phase 2) — world ops read the CP.
func (m *Model) refreshRunners(cfg *config.Config) {
	if cfg == nil {
		return
	}
	m.readCpRunners(cfg)
}

// readCpRunners fills the Runners view from the CONSOLE /api/overview (the
// CP's authoritative runner list — NIP-98 session). Not logged in = a hint
// row, not an empty table.
func (m *Model) readCpRunners(cfg *config.Config) {
	if m.console == nil || m.console.client == nil {
		m.Runners = []RunnerRow{{
			Name:   "(not logged into a console)",
			Status: styleDim.Render("press l to log in to see the CP runner list"),
			Addr:   cfg.Runner.Addr,
		}}
		return
	}
	ov, err := m.console.client.Overview()
	if err != nil {
		m.Runners = []RunnerRow{{
			Name:   "(console overview failed)",
			Status: styleRed.Render(clip(err.Error(), 48)),
			Addr:   cfg.Runner.Addr,
		}}
		return
	}
	m.Runners = nil
	for _, r := range ov.Runners {
		addr := "—"
		if r.McpAddr != nil {
			addr = *r.McpAddr
		} else if cfg != nil {
			addr = cfg.Runner.Addr
		}
		grants := "—"
		if len(r.Grants) > 0 {
			grants = fmt.Sprintf("%d grants", len(r.Grants))
		}
		readiness := "—"
		if r.Readiness != nil {
			readiness = fmt.Sprintf("%v", r.Readiness)
		}
		m.Runners = append(m.Runners, RunnerRow{
			Name: r.Name, Status: r.Status, Pubkey: r.NostrPubkey,
			Addr: addr, Grants: grants, Readiness: readiness,
		})
	}
	if len(m.Runners) == 0 {
		m.Runners = []RunnerRow{{Name: "(no runners on the console)", Status: styleDim.Render("provision one with p")}}
	}
}

// buildAgents fills the Agents view from the CP's /api/world — the same
// single-inventory status the /mcp world_status tool shares. The agent registry
// + world facts are served publicly by the console from the toolset's durable
// authoritative state, so ANY logged-in box renders them from the CP with no
// local agent-tools coords. Not logged in = a hint row, never a blank dashboard.
func (m *Model) buildAgents(cfg *config.Config) {
	m.Agents = nil
	// Gated on the live console session: buildAgents is called from load()
	// BEFORE the first frame, so at startup m.console == nil and we take this
	// free hint path. The real /api/world fetch happens after auto-login via
	// refreshLocal().
	if m.console == nil || m.console.client == nil {
		m.Agents = []AgentRow{{Name: "(not logged into a console)", Available: styleDim.Render("press l to log in to see the CP agent roster")}}
		m.Facts = nil
		return
	}
	w, err := m.console.client.World()
	if err != nil {
		m.Agents = []AgentRow{{Name: "(world fetch failed)", Available: styleRed.Render(clip(err.Error(), 48))}}
		m.Facts = nil
		return
	}
	// The world facts ride the same /api/world read — the DATA + Certs views
	// render from them on a management box.
	m.Facts = nil
	if len(w.Facts) > 0 {
		var facts agenttools.WorldFacts
		if err := json.Unmarshal(w.Facts, &facts); err != nil {
			m.Facts = nil
			_ = err
		} else {
			m.Facts = &facts
		}
	}
	for _, a := range w.Agents {
		created := "just now"
		if a.CreatedAt > 0 {
			created = humanize(time.Since(time.Unix(int64(a.CreatedAt), 0)))
		}
		m.Agents = append(m.Agents, AgentRow{Name: a.Name, Pubkey: a.Pubkey, Created: created})
	}
	if len(m.Agents) == 0 {
		m.Agents = []AgentRow{{Name: "(no agents on the console)", Created: styleDim.Render("created via the CP toolset")}}
	}
}

// ---- Update --------------------------------------------------------------

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.KeyMsg:
		// Full-screen activity mode owns every key while it runs (boot
		// check, teardown, rebuild incl. the door gate, bootstrap, deploys).
		if m.activity != nil {
			return m.handleActivityKey(v)
		}
		if m.Flow != nil {
			return m.handleFlow(v)
		}
		switch v.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.nextView()
		case "shift+tab":
			m.prevView()
		case "r":
			if m.Mode == ModeRunning {
				return m, m.startBootActivity("checking the world")
			}
		case "w":
			if m.Mode == ModeRunning {
				m.launchWeb()
			}
		case "l":
			if m.Mode == ModeRunning {
				m.beginPrompt(flowLogin)
			}
		case "p":
			if m.Mode == ModeRunning {
				m.beginPrompt(flowProvision)
			}
		case "x":
			if m.Mode == ModeRunning {
				m.beginPrompt(flowRevoke)
			}
		case "g":
			if m.Mode == ModeRunning {
				m.beginPrompt(flowGrant)
			}
			// Build/bootstrap/teardown/deploy are NOT run from the TUI — the TUI is
			// a status/operating dashboard. Run `freehold build` / `freehold teardown`
			// in a terminal instead (single canonical flow).
		}
	case flowMsg:
		m.Flow = nil
		m.refreshLocal()
		if v.err != nil {
			m.Err = v.err.Error()
		} else {
			m.Msg = v.ok
			// A flow (notably the manual `l` console login, which sets
			// m.console) may have just granted a console session: fill a
			// management box's world pillars from the CP. Idempotent; no-ops
			// unless a session exists and local coords are absent.
			m.applyCPWorldHealth()
		}
	case tickMsg:
		if m.activity == nil {
			m.LastRef = time.Now()
		}
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{} })
	case loginMsg:
		if v.err != nil {
			// Auto-login is best-effort (e.g. the console isn't reachable yet
			// during a boot check): stay unauthenticated, nudge the operator
			// to `l` once the world is up. Not a persistent error.
			m.Msg = "auto-login failed — press l to log in: " + clip(v.err.Error(), 48)
			return m, nil
		}
		if v.auth && v.client != nil {
			m.console = &consoleClient{client: v.client}
			m.consolePK = v.pubkey
			m.refreshLocal()
			m.applyCPWorldHealth()
			m.Msg = "auto-logged into the CP as " + v.pubkey[:12]
		}
		return m, nil
	case activityStartMsg:
		// the world-mutation forms all land here: full-screen streaming.
		m.Flow = nil
		return m.startSubprocessActivity(v.kind, v.title, v.args)
	default:
		if handled, mm, cmd := m.handleActivityMsg(msg); handled {
			return mm, cmd
		}
	}
	return m, nil
}
func (m *Model) nextView() {
	if m.Mode != ModeRunning {
		m.ActiveView = ViewServices
		return
	}
	m.ActiveView = View((int(m.ActiveView) + 1) % 6)
}
func (m *Model) prevView() {
	if m.Mode != ModeRunning {
		m.ActiveView = ViewServices
		return
	}
	m.ActiveView = View((int(m.ActiveView) + 5) % 6)
}

// ---- View ----------------------------------------------------------------

func (m *Model) View() string {
	if m.activity != nil {
		return m.activityView()
	}
	var b strings.Builder
	b.WriteString(styleTitle.Render(" freehold ") + styleDim.Render(m.Domain+" · "+m.Mode.String()) + "\n\n")
	if m.Err != "" {
		b.WriteString(styleRed.Render("! "+m.Err) + "\n\n")
	}
	if m.Mode == ModeBootstrap {
		b.WriteString(styleYellow.Render("no config — world not bootstrapped") + "\n\n")
		b.WriteString("run " + styleYellow.Render("freehold build") + " to bring up the world (bootstraps then reconciles)\n")
	} else if m.Mode == ModeConfigure {
		b.WriteString(styleYellow.Render("config present, world NOT converged") + "\n")
		b.WriteString(renderProbes(m) + "\n\n")
		b.WriteString("run " + styleYellow.Render("freehold build") + " to converge the world (tear + re-create as needed)\n")
	} else {
		b.WriteString(renderProbes(m) + "\n")
		b.WriteString(renderViews(m))
	}
	if m.Flow != nil {
		b.WriteString("\n  " + styleYellow.Render(promptLabel(m.Flow.Kind, m.Flow.Step)) + ": " + fieldValue(m.Flow) + "\n")
	}
	b.WriteString("\n" + m.footer())
	return lipgloss.NewStyle().Render(b.String())
}

func renderProbes(m *Model) string {
	return fmt.Sprintf("  relay %s  cp %s  k3s %s  litellm %s  caddy %s  runner %s",
		boolStatus(m.RelayLive, "green", "red"),
		boolStatus(m.CPLive, "green", "red"),
		boolStatus(m.K3sLive, "green", "red"),
		boolStatus(m.LitellmLive, "green", "red"),
		boolStatus(m.CaddyLive, "green", "red"),
		boolStatus(m.RunnerReach, "green", "red"),
	)
}

func (m *Model) footer() string {
	if m.Mode == ModeRunning {
		op := ""
		if m.consolePK != "" {
			op = " · op " + m.consolePK[:12]
		}
		return styleFooter.Render(fmt.Sprintf(
			"[%s] · Tab/Shift-Tab views · r refresh · q quit · last %s%s",
			m.ActiveView.String(), time.Since(m.LastRef).Round(time.Second), op)) +
			"   " + styleDim.Render("l log in (operator nsec) · p provision · x revoke · g grant · w web · build/teardown run from the shell")
	}
	switch m.Mode {
	case ModeBootstrap, ModeConfigure:
		return styleFooter.Render("q quit · run `freehold build` to bring up / converge the world")
	default:
		return styleFooter.Render("q quit")
	}
}

func renderViews(m *Model) string {
	var b strings.Builder
	for v := ViewServices; v <= ViewCerts; v++ {
		label := " " + v.String() + " "
		if v == m.ActiveView {
			label = "[" + v.String() + "]"
		}
		b.WriteString(label + " ")
	}
	b.WriteString("\n\n")
	if m.Msg != "" {
		b.WriteString(styleGreen.Render("  "+m.Msg) + "\n")
	}
	title := m.ActiveView.String()
	var headers []string
	var rows [][]string
	switch m.ActiveView {
	case ViewServices:
		headers = []string{"name", "where", "status", "url"}
		for _, s := range m.Services {
			rows = append(rows, []string{s.Name, s.Where, s.Status, s.URL})
		}
	case ViewDNS:
		title = "DNS · the CP resolver's explicit records"
		headers = []string{"name", "ip", "source"}
		if len(m.DNS) == 0 {
			rows = append(rows, []string{styleDim.Render("(no records — the resolver forwards everything upstream)")})
		}
		for _, d := range m.DNS {
			rows = append(rows, []string{d.Name, d.IP, d.Source})
		}
	case ViewAgents:
		headers = []string{"name", "pubkey", "available", "created"}
		for _, a := range m.Agents {
			rows = append(rows, []string{a.Name, clip(a.Pubkey, 16), a.Available, a.Created})
		}
	case ViewRunners:
		title = "Runners · CP (console /api/overview)"
		headers = []string{"name", "status", "pubkey", "addr", "grants", "readiness"}
		for _, r := range m.Runners {
			rows = append(rows, []string{r.Name, r.Status, clip(r.Pubkey, 16), r.Addr, r.Grants, r.Readiness})
		}
	case ViewData:
		if !m.DataAt.IsZero() {
			title = fmt.Sprintf("DATA · %s · refreshed %s", m.DataCap, humanize(time.Since(m.DataAt)))
		} else {
			title = "DATA · no plane snapshot yet"
		}
		headers = []string{"service", "mount point", "size", "used", "fill", "source (host)", "live"}
		if len(m.Storage) == 0 {
			rows = append(rows, []string{styleDim.Render("(no durable plane on record — converge first, then r refreshes)")})
		}
		for _, d := range m.Storage {
			rows = append(rows, []string{d.Role, d.Mount, d.Size, d.Used, d.Fill, d.Source, d.Live})
		}
	case ViewCerts:
		title = "Certs · the Caddy edge's wildcard certificate"
		headers = []string{"domain", "edge", "expires", "issuer", "status"}
		if len(m.Certs) == 0 {
			rows = append(rows, []string{styleDim.Render("(no cert on record — rebuild stages the wildcard cert for the edge)")})
		}
		for _, c := range m.Certs {
			rows = append(rows, []string{c.Domain, c.URL, c.Expiry, c.Issuer, c.Status})
		}
	}
	return b.String() + renderTable(title, headers, rows)
}

func renderTable(title string, headers []string, rows [][]string) string {
	var b strings.Builder
	b.WriteString(styleHeader.Render(" "+title+" ") + "\n")
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
		for _, r := range rows {
			if i < len(r) && len(r[i]) > widths[i] {
				widths[i] = len(r[i])
			}
		}
	}
	for i, h := range headers {
		b.WriteString(pad(h, widths[i]+2))
	}
	b.WriteString("\n" + strings.Repeat("─", sum(widths)+2*len(widths)) + "\n")
	for _, r := range rows {
		for i := range headers {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}
			b.WriteString(pad(cell, widths[i]+2))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func pad(s string, w int) string {
	if len(s) >= w {
		return s[:w]
	}
	return s + strings.Repeat(" ", w-len(s))
}

func sum(is []int) int {
	n := 0
	for _, v := range is {
		n += v
	}
	return n
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func boolStatus(v bool, on, off string) string {
	if v {
		return styleGreen.Render("● " + on)
	}
	return styleRed.Render("● " + off)
}

func humanize(d time.Duration) string {
	s := int64(d.Seconds())
	switch {
	case s < 2:
		return "just now"
	case s < 60:
		return fmt.Sprintf("%ds ago", s)
	case s < 3600:
		return fmt.Sprintf("%dm ago", s/60)
	case s < 86400:
		return fmt.Sprintf("%dh ago", s/3600)
	default:
		return fmt.Sprintf("%dd ago", s/86400)
	}
}
