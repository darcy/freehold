// Package tui is the interactive freehold dashboard — a bubbletea port of
// the Rust ratatui TUI (tui/src). ONE binary, two surfaces: no args -> this
// TUI; a subcommand -> the CLI (handlers in internal/cli).
//
// Modes auto-detected from the config's presence + liveness (installer
// probe_mode): bootstrap / configure / running.
// Running views cycled with Tab/Shift-Tab: Services · Agents · Runners · DATA.
package tui

import (
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/lipgloss"

	"freehold/contract/config"
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

// Runner sources for the Runners view: the console API (CP — the default)
// or the local loopback state.json. Toggle with `s`.
const (
	RunnerSourceCP    = "cp"
	RunnerSourceLocal = "local"
)

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
	Err         string
	Msg         string
	Flow        *tuiFlow
	console     *consoleClient
	// consolePK is the operator pubkey of the live console session ("" = not
	// logged in), shown in the footer / views.
	consolePK string
	// Runners view source: RunnerSourceCP (default) | RunnerSourceLocal.
	RunnerSource string
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
func New(cfgPath string) (*Model, error) {
	m := &Model{Mode: ModeBootstrap, LastRef: time.Now(), CfgPath: cfgPath}
	if m.CfgPath == "" {
		m.CfgPath = defaultTuiConfigPath()
	}
	if err := m.load(m.CfgPath); err != nil {
		return nil, err
	}
	return m, nil
}

// defaultTuiConfigPath mirrors config.DefaultPath (~/.config/freehold/config.toml).
func defaultTuiConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "freehold", "config.toml")
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	return filepath.Join(home, ".config", "freehold", "config.toml")
}
