//! freehold — ONE binary, two surfaces.
//!
//! - no args → the TUI (ratatui: bootstrap/configure/running modes, config at
//!   ~/.config/freehold/config.toml)
//! - `freehold <subcommand> …` → the CLI (exec, bootstrap, deploy-relay,
//!   deploy-cp, relay-member, memory, console-login, grant… — the scripted
//!   CPA surface, shared with the freehold-orchestrator bin)
//!   - `freehold --config <path>`   → the TUI with an explicit config
//!   - `freehold --help`            → this help

mod app;
mod modes;

use anyhow::Result;
use std::path::PathBuf;

fn usage() {
    eprintln!(
        "freehold — the freehold appliance (one binary, two surfaces)\n\n\
         TUI (no args):\n    freehold [--config <path>]\n\n\
         Modes (auto-detected):\n    bootstrap   no config at ~/.config/freehold/config.toml\n    configure   config present, world not converged\n    running     config present, everything reachable\n\
         Keys: q / Esc / Ctrl-C quit · ↑/↓ navigate · Enter confirm\n\n\
         CLI (subcommand as the first arg):\n    freehold exec <target> \"<cmd>\"\n    freehold bootstrap --kind proxmox-lxc --role relay --domain …\n    freehold deploy-relay / deploy-cp / relay-member …\n    … (see `freehold <subcommand> --help`)\n"
    );
}

fn main() -> Result<()> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match args.first().map(String::as_str) {
        None => run_tui(freehold_installer::config::Config::default_path()),
        Some("-h") | Some("--help") => {
            usage();
            Ok(())
        }
        Some("--config") => {
            let path = args
                .get(1)
                .map(PathBuf::from)
                .context("--config needs a path")?;
            run_tui(path)
        }
        Some(_) => freehold_orchestrator::cli::dispatch(),
    }
}

use anyhow::Context;

fn run_tui(cfg_path: PathBuf) -> Result<()> {
    let mut terminal = ratatui::init();
    let res = app::run(&mut terminal, cfg_path);
    ratatui::restore();
    res
}
