//! freehold-orchestrator — thin entry: the CLI surface lives in
//! `freehold_orchestrator::cli` (shared with the `freehold` TUI bin).

fn main() {
    if let Err(e) = freehold_orchestrator::cli::dispatch() {
        eprintln!("{e:#}");
        std::process::exit(1);
    }
}
