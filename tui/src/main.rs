//! freehold — the TUI.
//!
//! `freehold` (no args) opens a terminal UI over the freehold world:
//!
//!   - no `~/.config/freehold/config.toml`  → bootstrap mode (collect inputs,
//!     provision the SSH door, start the runner, write the config)
//!   - config present, world not converged  → configure mode (idempotent
//!     pipeline: relay/cp LXCs, deploy relay + cp)
//!   - config present, everything reachable → running mode ("Good to go!")
//!
//! The web console and this TUI are siblings over the same CP API (phase 2);
//! today the TUI drives the same stage binaries the CLI does.

mod app;
mod modes;

use anyhow::Result;
use std::path::PathBuf;

fn usage() {
    eprintln!(
        "freehold — the freehold appliance TUI\n\n\
         USAGE:\n    freehold [--config <path>]\n\n\
         Modes (auto-detected):\n    bootstrap   no config at ~/.config/freehold/config.toml\n    configure   config present, world not converged\n    running     config present, everything reachable\n\
         Keys: q / Esc / Ctrl-C quit · ↑/↓ navigate · Enter confirm"
    );
}

fn parse_args() -> Result<Option<PathBuf>> {
    let mut args = std::env::args().skip(1);
    let mut cfg_path = None;
    while let Some(a) = args.next() {
        match a.as_str() {
            "-h" | "--help" => {
                usage();
                std::process::exit(0);
            }
            "--config" => {
                cfg_path = Some(PathBuf::from(args.next().context("--config needs a path")?));
            }
            other => {
                anyhow::bail!("unknown argument {other:?} — see --help");
            }
        }
    }
    Ok(cfg_path)
}

use anyhow::Context;

fn main() -> Result<()> {
    let cfg_path = parse_args()?.unwrap_or_else(freehold_installer::config::Config::default_path);

    let mut terminal = ratatui::init();
    let res = app::run(&mut terminal, cfg_path);
    ratatui::restore();
    res
}
