// Command freehold is ONE binary, two surfaces (the Go port of the Rust
// freehold TUI/CLI):
//
//   - no args                -> the interactive TUI dashboard
//   - --config <path>        -> the TUI with an explicit config
//   - freehold <subcommand>  -> the CLI (the freehold-orchestrator surface)
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"freehold/orchestrator/internal/cli"
	"freehold/orchestrator/internal/config"
	"freehold/orchestrator/internal/tui"
)

func usage() {
	fmt.Fprintln(os.Stderr, `freehold — the freehold appliance (one binary, two surfaces)

TUI (no args) — a STATUS dashboard for operating a deployed world:
    freehold [--config <path>]
    views (Tab / Shift-Tab): Services · Agents · Runners · Data · DNS · Certs
    build / teardown are NOT run from here — use the CLI commands below.

Modes (auto-detected):
    bootstrap   no config at ~/.config/freehold/config.toml
    configure   config present, world not converged
    running     config present, everything reachable

CLI:
    freehold build     bring the world up (fresh bootstrap OR rebuild — the same
                       reconciling pipeline; reads the recorded config, asks only
                       what's missing, picks the DNS provider from lego's list)
    freehold teardown  destroy the managed world (confirm first)
    freehold exec <target> "<cmd>"   run a command through the runner
    ... (see `+"`freehold <subcommand> --help`"+`)`)
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		runTUI(config.DefaultPath())
		return
	}
	switch args[0] {
	case "-h", "--help":
		usage()
		return
	case "--config":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "--config needs a path")
			os.Exit(2)
		}
		runTUI(args[1])
		return
	default:
		os.Exit(cli.Run(args))
	}
}

// runTUI starts the interactive bubbletea dashboard.
func runTUI(cfgPath string) {
	m, err := tui.New(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
