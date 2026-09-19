// Command freehold is ONE binary, two surfaces (the Go port of the Rust
// freehold TUI/CLI):
//
//   - no args                -> the interactive TUI dashboard
//   - --config <path>        -> the TUI with an explicit config
//   - freehold <subcommand>  -> the CLI (the operator surface)
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"freehold/contract/config"
	"freehold/freehold-cli/cli"
	"freehold/freehold-cli/cli/login"
	"freehold/freehold-cli/cli/tui"
)

func usage() {
	fmt.Fprintln(os.Stderr, `freehold — the freehold appliance (one binary, two surfaces)

TUI (no args) — a STATUS dashboard for operating a deployed world:
    freehold [--config <path>]
    views (Tab / Shift-Tab): Services · Agents · Runners · Data · DNS · Certs
    build / teardown are NOT run from here — use the CLI commands below.

Modes (auto-detected):
    bootstrap   no config for the chosen tenant at ~/.config/freehold/
    configure   config present, world not converged
    running     config present, everything reachable

CLI:
    freehold login     add a TENANT profile (CP address + pubkey + nsec) and end —
                       root-free. Prompts a profile name (default: the CP host; an
                       existing box auto-resolves to the legacy "default" profile).
                       Afterwards just run freehold and pick the profile.
    freehold profiles  list the registered tenant profiles (each has its own login
                       + config file + scoped state dir)
    freehold logout    clear the login ledger for the chosen profile/local box
                       (CP/world untouched)
    freehold build     bring the world up for the chosen profile (fresh bootstrap
                       OR rebuild — the same reconciling pipeline; a profile
                       picker runs when more than one is registered)
    freehold teardown  destroy the managed world for the chosen profile (confirm first)
    freehold exec <target> "<cmd>"   run a command through the runner
    ... (see `+"`freehold <subcommand> --help`"+`)`)
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		runTUI("")
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
	case "login", "--login":
		if err := oplogin.Interactive(); err != nil {
			fmt.Fprintln(os.Stderr, "login:", err)
			os.Exit(1)
		}
		return
	case "logout", "--logout":
		if len(config.List()) > 0 {
			if _, err := oplogin.SelectProfile("log out"); err != nil {
				fmt.Fprintln(os.Stderr, "logout:", err)
				os.Exit(1)
			}
		}
		if err := oplogin.Logout(); err != nil {
			fmt.Fprintln(os.Stderr, "logout:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "logged out — local login ledger cleared")
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
