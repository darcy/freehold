package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"freehold/orchestrator/internal/config"
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

	// Liveness probes (installer probes_ok split: relay + CP + runner).
	m.RelayReach = config.RelayLive(cfg)
	m.RunnerReach = config.URLReachable("http://" + cfg.Runner.Addr)
	m.CPReach = config.URLReachable(cfg.CPURL)
	m.Converged = m.RelayReach && m.CPReach && m.RunnerReach
	if !m.Converged {
		m.Mode = ModeConfigure
	} else {
		m.Mode = ModeRunning
	}

	m.buildServices(cfg)
	m.readLocalRunners(cfg)
	m.buildAgents(cfg)
	return nil
}

// buildServices fills the Services view from the config's managed pieces.
func (m *Model) buildServices(cfg *config.Config) {
	m.Services = nil
	for _, piece := range cfg.Managed {
		row := ServiceRow{Name: piece}
		switch piece {
		case "relay":
			row.Where = "LXC relay"
			row.URL = cfg.RelayURL
			row.Status = boolStatus(m.RelayReach, "reachable", "down")
		case "cp":
			row.Where = "LXC cp"
			row.URL = cfg.CPURL
			row.Status = boolStatus(m.CPReach, "up", "down")
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
		switch v.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.nextView()
		case "shift+tab":
			m.prevView()
		case "r":
			if m.Mode == ModeRunning {
				m.Msg = "data refresh requested"
			}
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
	if m.Mode == ModeBootstrap {
		b.WriteString(styleYellow.Render("no config at ~/.config/freehold/config.toml — world not bootstrapped") + "\n\n")
		b.WriteString("run the CLI to bootstrap:  freehold bootstrap --kind proxmox-lxc --role relay --domain <domain>\n")
	} else if m.Mode == ModeConfigure {
		b.WriteString(styleYellow.Render("config present, world NOT converged") + "\n")
		b.WriteString(renderProbes(m) + "\n\n")
		b.WriteString("run `freehold bootstrap` / `freehold deploy-relay` / `freehold deploy-cp` to converge, or check the runner door.\n")
	} else {
		b.WriteString(renderProbes(m) + "\n")
		b.WriteString(renderViews(m))
	}
	b.WriteString("\n" + m.footer())
	return lipgloss.NewStyle().Render(b.String())
}

func renderProbes(m *Model) string {
	return fmt.Sprintf("  relay %s  cp %s  runner %s",
		boolStatus(m.RelayReach, "green", "red"),
		boolStatus(m.CPReach, "green", "red"),
		boolStatus(m.RunnerReach, "green", "red"),
	)
}

func (m *Model) footer() string {
	if m.Mode == ModeRunning {
		return styleFooter.Render(fmt.Sprintf(
			"[%s] · Tab/Shift-Tab views · r refresh · q quit · last %s",
			m.ActiveView.String(), time.Since(m.LastRef).Round(time.Second))) +
			"   " + styleDim.Render("l login · p provision · x revoke · g grant · w web")
	}
	return styleFooter.Render("q quit")
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
		headers = []string{"role", "source", "capacity", "used", "live"}
		for _, d := range m.Storage {
			rows = append(rows, []string{d.Role, d.Source, d.Capacity, d.Used, d.Live})
		}
	}
	return b.String() + renderTable(m.ActiveView.String(), headers, rows)
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
