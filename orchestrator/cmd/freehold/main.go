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

TUI (no args):
    freehold [--config <path>]

Modes (auto-detected):
    bootstrap   no config at ~/.config/freehold/config.toml
    configure   config present, world not converged
    running     config present, everything reachable
Running views (Tab / Shift-Tab): Services · Agents · Runners · DATA
Globals: Tab/Shift-Tab views · r refresh · q/Esc/Ctrl-C quit

CLI (subcommand as the first arg):
    freehold exec <target> "<cmd>"
    freehold bootstrap --kind proxmox-lxc --role relay --domain ...
    freehold deploy-relay / deploy-cp / relay-member / console-login ...
    freehold teardown            destroy the managed world (confirm first)
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
	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
