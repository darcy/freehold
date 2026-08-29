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
)

func (v View) String() string {
	switch v {
	case ViewAgents:
		return "Agents"
	case ViewRunners:
		return "Runners"
	case ViewData:
		return "DATA"
	default:
		return "Services"
	}
}

// Model is the bubbletea model for the whole dashboard.
type Model struct {
	Mode        Mode
	Domain      string
	HasConfig   bool
	RelayReach  bool
	CPReach     bool
	RunnerReach bool
	Converged   bool
	ActiveView  View
	LastRef     time.Time
	Services    []ServiceRow
	Agents      []AgentRow
	Runners     []RunnerRow
	Storage     []DataRow
	Err         string
	Msg         string
	Flow        *tuiFlow
	console     *consoleClient
}

// ServiceRow is one managed piece of the world.
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
	Role     string
	Source   string
	Capacity string
	Used     string
	Live     string
}

// New builds the model from the config path (mirrors app.rs::run).
func New(cfgPath string) (*Model, error) {
	m := &Model{Mode: ModeBootstrap, LastRef: time.Now()}
	if err := m.load(cfgPath); err != nil {
		return nil, err
	}
	return m, nil
}
