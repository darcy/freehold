package tui

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"freehold/orchestrator/internal/config"
	"freehold/orchestrator/internal/drive"
	"freehold/orchestrator/internal/flows"
	"freehold/orchestrator/internal/planebase"
	"freehold/orchestrator/internal/state"
)

// ---- bubbletea lifecycle -------------------------------------------------

func (m *Model) Init() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{} })
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

// load detects the mode from the config's presence + liveness and populates
// the dashboard views (mirrors app.rs::run + installer probe_mode).
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
	m.Domain = cfg.Domain

	// Liveness probes — the world must be ALIVE, not merely reachable behind
	// the operator's proxy (Rust probes_ok): the relay's OWN /_liveness 2xx,
	// the CP's /healthz answered 200 from INSIDE its LXC (through the
	// provisioning runner), and the runner MCP port open. The k3s API is NOT
	// part of the convergence gate (Rust parity) — it shows in the strip.
	m.RelayLive = config.RelayLive(cfg)
	m.RunnerReach = config.URLReachable("http://" + cfg.Runner.Addr)
	m.CPLive = cpLive(cfg)
	m.K3sLive = config.K3sLive(cfg)
	m.Converged = m.RelayLive && m.CPLive && m.RunnerReach
	if !m.Converged {
		m.Mode = ModeConfigure
	} else {
		m.Mode = ModeRunning
	}

	m.buildServices(cfg)
	m.readLocalRunners(cfg)
	m.buildAgents(cfg)
	if m.Mode == ModeRunning {
		m.refreshData(cfg)
	}
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

// buildServices fills the Services view from the config's managed pieces
// (mirrors Rust build_services, incl. the k3s row).
func (m *Model) buildServices(cfg *config.Config) {
	m.Services = nil
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
		default:
			row.Where = "managed"
			row.Status = "—"
		}
		m.Services = append(m.Services, row)
	}
	if len(m.Services) == 0 {
		m.Services = []ServiceRow{{Name: "(none managed)", Status: styleDim.Render("add `managed` entries to config")}}
	}
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

// refreshData rebuilds the DATA view: the live durable-plane snapshot read
// through the SAME signed runner channel the orchestrator uses (ops agent
// identity — granted at bootstrap). Mirrors Rust plane_info: a missing
// plane/runner keeps the last good snapshot; a probe failure degrades to
// the notice in m.Msg.
func (m *Model) refreshData(cfg *config.Config) {
	if cfg == nil || cfg.Plane.Backend == nil || cfg.Plane.BackendKind == nil {
		return
	}
	var kind planebase.BackendKind
	switch *cfg.Plane.BackendKind {
	case string(planebase.KindZfs):
		kind = planebase.KindZfs
	case string(planebase.KindLvmThin):
		kind = planebase.KindLvmThin
	default:
		return
	}
	var mounts []drive.MountArg
	for role, specs := range cfg.Plane.Mounts {
		vmid := vmidForRole(cfg, role)
		for _, s := range specs {
			mounts = append(mounts, drive.MountArg{Role: role, Source: s.Source, Guest: s.GuestPath, VMID: vmid})
		}
	}
	if len(mounts) == 0 {
		return
	}
	sort.Slice(mounts, func(i, j int) bool {
		if mounts[i].Role != mounts[j].Role {
			return mounts[i].Role < mounts[j].Role
		}
		return mounts[i].Guest < mounts[j].Guest
	})
	c, err := flows.Connect(cfg.Runner.Addr, freeholdStateDir()+"/agent-ops", cfg.Runner.Pubkey)
	if err != nil {
		m.Msg = "data: runner connect failed — " + err.Error()
		return
	}
	info, err := drive.ProbeStorage(c, cfg.Runner.Target, kind, *cfg.Plane.Backend, mounts)
	if err != nil {
		m.Msg = "data: plane probe failed — " + err.Error()
		return
	}
	m.DataCap = info.Capacity
	m.DataAt = time.Now()
	m.Storage = nil
	for _, mu := range info.Mounts {
		size, used, fill := "—", "—", "—"
		if mu.Size != nil {
			size = drive.HumanBytes(*mu.Size)
		}
		if mu.Used != nil {
			used = drive.HumanBytes(*mu.Used)
		}
		if mu.Used != nil && mu.Size != nil && *mu.Size > 0 {
			pct := (*mu.Used*100 + *mu.Size - 1) / (*mu.Size) // div_ceil
			fill = fillStyle(pct).Render(fmt.Sprintf("%d%%", pct))
		}
		live := styleDim.Render("—")
		if mu.GuestMounted != nil {
			live = boolStatus(*mu.GuestMounted, "mounted", "down")
		}
		m.Storage = append(m.Storage, DataRow{
			Role: mu.Role, Mount: mu.Guest, Size: size, Used: used,
			Fill: fill, Source: mu.Source, Live: live,
		})
	}
	if len(m.Storage) == 0 {
		m.Storage = []DataRow{{Role: "(no mounts)"}}
	}
	m.Msg = fmt.Sprintf("data refreshed %s", time.Now().Format("15:04:05"))
}

// vmidForRole maps a plane mount role to its LXC vmid (mirror of Rust
// plane_info's role match).
func vmidForRole(cfg *config.Config, role string) *uint32 {
	switch role {
	case "relay":
		return cfg.Lxc.Relay.Vmid
	case "cp":
		return cfg.Lxc.Cp.Vmid
	case "k3s":
		return cfg.Lxc.K3s.Vmid
	default:
		return nil
	}
}

// fillStyle is the DATA fill-ratio traffic light (Rust fill_color):
// green < 70%, yellow < 90%, red at/above.
func fillStyle(pct uint64) lipgloss.Style {
	switch {
	case pct >= 90:
		return styleRed
	case pct >= 70:
		return styleYellow
	default:
		return styleGreen
	}
}

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

// readLocalRunners fills the Runners view from the local CP state.json (the
// loopback authn path — mirror of running.rs::read_local).
func (m *Model) readLocalRunners(cfg *config.Config) {
	st, err := state.Open(freeholdStateDir())
	if err != nil {
		m.Runners = nil
		return
	}
	m.Runners = nil
	for name, rec := range st.Snapshot().Runners {
		addr := "—"
		if rec.McpAddr != nil {
			addr = *rec.McpAddr
		} else if cfg != nil {
			addr = cfg.Runner.Addr
		}
		m.Runners = append(m.Runners, RunnerRow{
			Name: name, Status: string(rec.Status), Pubkey: rec.NostrPubkey,
			Addr: addr, Readiness: "—",
		})
	}
	if len(m.Runners) == 0 {
		m.Runners = []RunnerRow{{Name: "(no runners)", Status: styleDim.Render("provision one in the CLI")}}
	}
}

// buildAgents fills the Agents view from the local state registry.
func (m *Model) buildAgents(cfg *config.Config) {
	st, err := state.Open(freeholdStateDir())
	if err != nil {
		m.Agents = nil
		return
	}
	m.Agents = nil
	for name, rec := range st.Snapshot().Agents {
		created := "just now"
		if rec.CreatedAt > 0 {
			created = humanize(time.Since(time.Unix(int64(rec.CreatedAt), 0)))
		}
		m.Agents = append(m.Agents, AgentRow{Name: name, Pubkey: rec.Pubkey, Available: "—", Created: created})
	}
	if len(m.Agents) == 0 {
		m.Agents = []AgentRow{{Name: "(no agents)", Created: styleDim.Render("nothing stood up yet")}}
	}
}

// ---- Update --------------------------------------------------------------

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.KeyMsg:
		// The Rust door flow: a rebuild paused at the door gate keeps the
		// operator INSIDE the gate — install the key, press ENTER, the
		// SAME rebuild re-runs in place (stages are idempotent: provision
		// reuses the key, verify re-probes the door and the pipeline
		// continues). Never back to the form.
		if m.rebuildArgs != nil {
			switch v.Type {
			case tea.KeyEnter:
				args := m.rebuildArgs
				m.Wait, m.Err, m.Msg = "", "", "re-testing the door and resuming rebuild…"
				m.rebuildArgs = nil
				m.Flow = nil
				return m, func() tea.Msg { return rebuildRun(args) }
			case tea.KeyEsc:
				m.rebuildArgs, m.Wait, m.Msg = nil, "", "rebuild cancelled at the door — the key is still printed above"
				return m, nil
			case tea.KeyCtrlC:
				return m, tea.Quit
			}
			if v.String() == "q" {
				return m, tea.Quit
			}
			return m, nil // the gate swallows everything else until ENTER/esc
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
				_ = m.load(m.CfgPath)
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
		case "b":
			if m.Mode == ModeBootstrap {
				m.beginPrompt(flowBootstrap)
			}
		case "d":
			if m.Mode == ModeConfigure {
				m.beginPrompt(flowDeployRelay)
			}
		case "c":
			if m.Mode == ModeConfigure {
				m.beginPrompt(flowDeployCp)
			}
		case "t":
			if m.Mode == ModeRunning {
				m.beginPrompt(flowTeardown)
			}
		case "B":
			if m.Mode == ModeBootstrap || m.Mode == ModeConfigure {
				m.beginPrompt(flowRebuild)
			}
		}
	case flowMsg:
		m.Flow = nil
		m.refreshLocal()
		if v.reload {
			_ = m.load(m.CfgPath)
		}
		if v.err != nil {
			m.Err, m.Wait = v.err.Error(), ""
			m.rebuildArgs = nil
		} else if v.wait != "" {
			m.Wait = v.wait
			// the paused rebuild keeps its args → ENTER re-runs it in place.
			m.rebuildArgs = v.rebuildArgs
		} else {
			m.Msg, m.Wait = v.ok, ""
			m.rebuildArgs = nil
		}
	case tickMsg:
		m.LastRef = time.Now()
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{} })
	}
	return m, nil
}

func (m *Model) nextView() {
	if m.Mode != ModeRunning {
		m.ActiveView = ViewServices
		return
	}
	m.ActiveView = View((int(m.ActiveView) + 1) % 4)
}
func (m *Model) prevView() {
	if m.Mode != ModeRunning {
		m.ActiveView = ViewServices
		return
	}
	m.ActiveView = View((int(m.ActiveView) + 3) % 4)
}

// ---- View ----------------------------------------------------------------

func (m *Model) View() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render(" freehold ") + styleDim.Render(m.Domain+" · "+m.Mode.String()) + "\n\n")
	if m.Err != "" {
		b.WriteString(styleRed.Render("! "+m.Err) + "\n\n")
	}
	if m.Wait != "" {
		b.WriteString(styleYellow.Render("waiting for the operator:\n"+m.Wait) + "\n")
		if m.rebuildArgs != nil {
			b.WriteString(styleYellow.Render("install the key, then press ENTER to re-test the door and resume — esc to cancel") + "\n")
		}
		b.WriteString("\n")
	}
	if m.Mode == ModeBootstrap {
		b.WriteString(styleYellow.Render("no config — world not bootstrapped") + "\n\n")
		b.WriteString("press " + styleYellow.Render("b") + " to bootstrap a target (kind · domain · operator pubkey)\n")
		b.WriteString("press " + styleYellow.Render("B") + " to rebuild the whole world (door · plane · LXCs · deploys)\n")
	} else if m.Mode == ModeConfigure {
		b.WriteString(styleYellow.Render("config present, world NOT converged") + "\n")
		b.WriteString(renderProbes(m) + "\n\n")
		b.WriteString("press " + styleYellow.Render("d") + " to deploy the relay, " + styleYellow.Render("c") + " to deploy the control plane\n")
		b.WriteString("press " + styleYellow.Render("B") + " to rebuild the whole world (tear + re-create everything)\n")
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
	return fmt.Sprintf("  relay %s  cp %s  k3s %s  runner %s",
		boolStatus(m.RelayLive, "green", "red"),
		boolStatus(m.CPLive, "green", "red"),
		boolStatus(m.K3sLive, "green", "red"),
		boolStatus(m.RunnerReach, "green", "red"),
	)
}

func (m *Model) footer() string {
	if m.Mode == ModeRunning {
		return styleFooter.Render(fmt.Sprintf(
			"[%s] · Tab/Shift-Tab views · r refresh · q quit · last %s",
			m.ActiveView.String(), time.Since(m.LastRef).Round(time.Second))) +
			"   " + styleDim.Render("l login · p provision · x revoke · g grant · w web · t teardown")
	}
	switch m.Mode {
	case ModeBootstrap:
		return styleFooter.Render("q quit · b bootstrap · B rebuild")
	case ModeConfigure:
		return styleFooter.Render("q quit · d deploy-relay · c deploy-cp · B rebuild")
	default:
		return styleFooter.Render("q quit")
	}
}

func renderViews(m *Model) string {
	var b strings.Builder
	for v := ViewServices; v <= ViewData; v++ {
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
	case ViewAgents:
		headers = []string{"name", "pubkey", "available", "created"}
		for _, a := range m.Agents {
			rows = append(rows, []string{a.Name, clip(a.Pubkey, 16), a.Available, a.Created})
		}
	case ViewRunners:
		headers = []string{"name", "status", "pubkey", "addr", "grants"}
		for _, r := range m.Runners {
			rows = append(rows, []string{r.Name, r.Status, clip(r.Pubkey, 16), r.Addr, r.Grants})
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
