// Package tui is the interactive freehold dashboard — a bubbletea port of
// the Rust ratatui TUI (tui/src). ONE binary, two surfaces: no args -> this
// TUI; a subcommand -> the CLI (handlers in internal/cli).
//
// Modes auto-detected from the config's presence + liveness (installer
// probe_mode): bootstrap / configure / running.
// Running views cycled with Tab/Shift-Tab: Services · Agents · Runners · DATA.
package tui

import (
	"time"

	"github.com/charmbracelet/lipgloss"

	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/cli/login"
)

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleYellow = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	styleRed    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	styleFooter = lipgloss.NewStyle().Foreground(lipgloss.Color("247"))
)

// Mode is the auto-detected TUI mode.
type Mode int

const (
	ModeBootstrap Mode = iota
	ModeConfigure
	ModeRunning
)

func (m Mode) String() string {
	switch m {
	case ModeBootstrap:
		return "bootstrap"
	case ModeConfigure:
		return "configure"
	default:
		return "running"
	}
}


// View is the running dashboard's active view.
type View int

const (
	ViewServices View = iota
	ViewAgents
	ViewRunners
	ViewData
	ViewDNS
	ViewCerts
)

func (v View) String() string {
	switch v {
	case ViewAgents:
		return "Agents"
	case ViewRunners:
		return "Runners"
	case ViewData:
		return "DATA"
	case ViewDNS:
		return "DNS"
	case ViewCerts:
		return "Certs"
	default:
		return "Services"
	}
}

// Model is the bubbletea model for the whole dashboard.
type Model struct {
	Mode        Mode
	Domain      string
	HasConfig   bool
	RelayLive   bool
	CPLive      bool
	K3sLive     bool
	LitellmLive bool
	CaddyLive   bool
	RunnerReach bool
	Converged   bool
	ActiveView  View
	LastRef     time.Time
	DataCap     string
	DataAt      time.Time
	Services    []ServiceRow
	DNS         []DnsRow
	Certs       []CertRow
	Agents      []AgentRow
	Runners     []RunnerRow
	Storage     []DataRow
	// Facts are the deployer-side world facts (plane/certs/domains) the CP
	// serves on world_status — the DATA + Certs views render from these on a
	// management/login-only box that has no local config + host probes.
	Facts *agenttools.WorldFacts
	Err   string
	Msg   string
	Flow  *tuiFlow
	console *consoleClient
	// worldSvc maps a service kind (k3s|litellm|caddy) to the CP-served live
	// health (from /api/world on the console session). Populated by the
	// management-box login hook and used to render a logged-in box's world
	// green even when this box has no local coords of its own.
	worldSvc map[string]bool
	// cpWorld is the last-fetched /api/world summary on the console session.
	// A management box (no local runner/coords) renders its Services view, DNS
	// and header domain from it, mirroring what the deploying box shows from
	// its own config.
	cpWorld *console.WorldSummary
	// consolePK is the operator pubkey of the live console session ("" = not
	// logged in), shown in the footer / views.
	consolePK string
	// last-loaded config — the `s` toggle and post-flow refreshes need it.
	cfg *config.Config
	// activity: while non-nil, the FULL-SCREEN activity view replaces the
	// dashboard entirely (boot check, teardown, rebuild, bootstrap, deploys).
	// The door-gate pause lives inside it too (its args ride a.args).
	activity *activity
	CfgPath  string
}

// ServiceRow is one managed piece of the world.
// DnsRow is one explicit resolver record (C0 DNS panel).
type DnsRow struct {
	Name   string
	IP     string
	Source string
}

// CertRow is one wildcard cert row on the Certs view (written by the F3 lego
// stage, shown read-only; renew happens via rebuild's cert stage).
type CertRow struct {
	Domain string
	URL    string
	Expiry string
	Issuer string
	Status string
}

type ServiceRow struct {
	Name   string
	Where  string
	Status string
	URL    string
}

// AgentRow is a named agent.
type AgentRow struct {
	Name      string
	Pubkey    string
	Created   string
	Available string
}

// RunnerRow is a runner from the console overview or the local list.
type RunnerRow struct {
	Name      string
	Status    string
	Pubkey    string
	Addr      string
	Grants    string
	Readiness string
}

// DataRow is one durable-plane mount line (DATA view).
type DataRow struct {
	Role   string
	Mount  string
	Size   string
	Used   string
	Fill   string
	Source string
	Live   string
}

// New builds the model from the config path (mirrors app.rs::run).
//
// An empty cfgPath means "the default" and triggers tenant negotiation: when
// the box has registered profiles, the operator picks one (its config + state
// become this session's), otherwise the legacy default is used. An explicit
// cfgPath (--config) bypasses negotiation.
func New(cfgPath string) (*Model, error) {
	m := &Model{Mode: ModeBootstrap, LastRef: time.Now()}
	if cfgPath != "" {
		m.CfgPath = cfgPath
	} else {
		m.CfgPath = selectTuiProfile()
	}
	if err := m.load(m.CfgPath); err != nil {
		return nil, err
	}
	return m, nil
}

// selectTuiProfile returns the config path for this TUI session: the picked
// profile's when one is registered, else the legacy default path.
func selectTuiProfile() string {
	if len(config.List()) > 0 {
		if p, err := oplogin.SelectProfile("operate"); err == nil && p != nil {
			return p.ConfigPath
		}
	}
	return config.ConfigPath()
}

// defaultTuiConfigPath mirrors config.ConfigPath (the negotiated profile's
// config, or the legacy ~/.config/freehold/config.toml).
func defaultTuiConfigPath() string {
	return config.ConfigPath()
}
