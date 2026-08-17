use std::path::{Path, PathBuf};

use anyhow::Context;
use clap::{Args, Parser, Subcommand};
use freehold_runner::identity::{self, Identity};

#[derive(Parser)]
#[command(
    name = "runner",
    version,
    about = "Freehold runner: privileged MCP tool server"
)]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// Manage runner identity keypairs (Nostr + X25519 encryption)
    Keys(KeysCmd),
    /// Start the MCP tool server
    Serve(ServeArgs),
}

#[derive(Parser)]
struct KeysCmd {
    #[command(subcommand)]
    cmd: KeysAction,
}

#[derive(Subcommand)]
enum KeysAction {
    /// Generate keypairs and write them to the state dir (mode 0600, never committed)
    Init(InitArgs),
}

#[derive(Args)]
struct InitArgs {
    /// State dir for identity files
    #[arg(long, env = "FREEHOLD_STATE_DIR", default_value = "./.freehold")]
    state_dir: PathBuf,
    /// Replace an existing identity (irreversible — orphaning every grant on it)
    #[arg(long)]
    force: bool,
}

#[derive(Args)]
struct ServeArgs {
    /// Dir holding the runner identity
    #[arg(long, env = "FREEHOLD_STATE_DIR", default_value = "./.freehold")]
    state_dir: PathBuf,
    /// Loopback address to bind the MCP server (non-loopback binds are rejected)
    #[arg(long, env = "FREEHOLD_RUNNER_ADDR", default_value = "127.0.0.1:8787")]
    addr: String,
    /// The relay this runner belongs to (Phase D: grants read LIVE from it
    /// as kind-30180 events; omit for package-only grants — local/loopback).
    #[arg(long, env = "FREEHOLD_RELAY_URL")]
    relay_url: Option<String>,
}

/// Operator-facing warning printed after `keys init`: env vars override the
/// file in `serve`, so if they're set the printed pubkey is not the one that
/// will run. Pure + testable; fires ONLY when at least one env var is present
/// (`load_with(None, None)` falls back to the file and prints nothing).
fn env_shadow_note(nsec: Option<String>, enc: Option<String>, state_dir: &Path) -> Option<String> {
    match (nsec, enc) {
        (Some(n), Some(e)) => match Identity::load_with(state_dir, Some(n), Some(e)) {
            Ok(env_id) => Some(format!(
                "note: {} and {} are set — `runner serve` will use the ENV \
                 identity (nostr pubkey {}), not the file above",
                identity::NSEC_ENV,
                identity::ENC_ENV,
                env_id.nostr_pubkey_hex()
            )),
            Err(e) => Some(format!(
                "warning: {} and {} are set but unusable ({e}) — `runner serve` will fail",
                identity::NSEC_ENV,
                identity::ENC_ENV
            )),
        },
        (Some(_), None) | (None, Some(_)) => Some(format!(
            "warning: only one of {}/{} is set — `runner serve` will fail with PartialEnv",
            identity::NSEC_ENV,
            identity::ENC_ENV
        )),
        (None, None) => None,
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();

    match Cli::parse().cmd {
        Cmd::Keys(KeysCmd {
            cmd: KeysAction::Init(args),
        }) => {
            let path = args.state_dir.join(identity::IDENTITY_FILE);
            if path.exists() && !args.force {
                anyhow::bail!(
                    "identity already exists at {} — pass --force to overwrite (irreversible)",
                    path.display()
                );
            }
            if args.force && path.exists() {
                let target = Identity::backup_identity(&args.state_dir)?;
                println!("backed up old identity to {}", target.display());
            }
            let id = Identity::generate();
            let written = id.write_to_dir(&args.state_dir)?;
            println!("wrote identity to {}", written.display());
            println!("nostr pubkey (hex):      {}", id.nostr_pubkey_hex());
            println!("encryption pubkey (hex): {}", id.enc_pubkey_hex());
            println!(
                "private keys live in {} (0600) — do not commit",
                written.display()
            );
            // load_with gives env vars priority over the file — if they're set,
            // `runner serve` will NOT use the identity we just wrote. Say so
            // now, or the printed pubkey looks authoritative and is wrong.
            // Gated on actual env presence: load_with(None, None) falls back to
            // the file and must not be read as "env is set".
            if let Some(note) = env_shadow_note(
                std::env::var(identity::NSEC_ENV).ok(),
                std::env::var(identity::ENC_ENV).ok(),
                &args.state_dir,
            ) {
                println!("{note}");
            }
            Ok(())
        }
        Cmd::Serve(args) => {
            let id = Identity::load_with(
                &args.state_dir,
                std::env::var(identity::NSEC_ENV).ok(),
                std::env::var(identity::ENC_ENV).ok(),
            )
            .with_context(|| {
                format!(
                    "no identity found in {} — run `runner keys init`",
                    args.state_dir.display()
                )
            })?;
            tracing::info!(
                addr = %args.addr,
                nostr_pubkey = %id.nostr_pubkey_hex(),
                enc_pubkey = %id.enc_pubkey_hex(),
                "runner starting"
            );
            // The secret package (ciphertext only) ships next to the identity;
            // exec resolves secret NAMES from it, en route to A4's plumbing.
            let package = match freehold_runner::secrets::SecretPackage::load(&args.state_dir) {
                Ok(p) => p,
                Err(e) => {
                    tracing::warn!(error = %e, "no secrets package found — exec runs without secrets");
                    freehold_runner::secrets::SecretPackage::default()
                }
            };
            // Log NAMES only — never values.
            if !package.secrets.is_empty() {
                tracing::info!(
                    secrets = ?package.secrets.keys().cloned().collect::<Vec<_>>(),
                    "runner holds ciphertext for {} secret(s)",
                    package.secrets.len()
                );
            }
            let (bound, server) = freehold_runner::mcp::serve(
                &args.addr,
                freehold_runner::mcp::RunnerContext {
                    identity: id,
                    package,
                    state_dir: args.state_dir.clone(),
                    relay_url: args.relay_url.clone(),
                },
            )
            .await?;
            tracing::info!(addr = %bound, "runner MCP server listening");
            // A4: replace with an axum graceful-shutdown future once exec has
            // in-flight work to drain — signal-to-exit is not a drain.
            tokio::select! {
                _ = tokio::signal::ctrl_c() => tracing::info!("shutting down"),
                _ = server => {}
            }
            Ok(())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // (Some, Some) takes the from_hex path — the dir is never touched.
    fn note(nsec: Option<&str>, enc: Option<&str>) -> Option<String> {
        env_shadow_note(
            nsec.map(String::from),
            enc.map(String::from),
            &PathBuf::from("/nonexistent"),
        )
    }

    #[test]
    fn no_env_prints_nothing() {
        assert!(note(None, None).is_none(), "default path must stay silent");
    }

    #[test]
    fn partial_env_warns_not_silent() {
        // "00".repeat(32) is a 32-byte ALL-ZERO scalar: valid length, invalid
        // secp256k1 key. It would surface as "unusable" if validation ran
        // before the partial-env arm — asserting "PartialEnv" proves ordering.
        let n = note(Some(&"00".repeat(32)), None).expect("partial env warns");
        assert!(n.contains("only one of"), "got: {n}");
        assert!(n.contains("PartialEnv"), "got: {n}");
        assert!(
            !n.contains("unusable"),
            "partial check must precede validation: {n}"
        );
    }

    #[test]
    fn both_env_valid_names_the_env_pubkey() {
        let id = Identity::generate();
        let n = note(Some(&id.nostr_secret_hex()), Some(&id.enc_secret_hex()))
            .expect("both env vars set");
        assert!(n.contains("ENV identity"), "got: {n}");
        assert!(
            n.contains(&id.nostr_pubkey_hex()),
            "must name the env pubkey"
        );
    }

    #[test]
    fn both_env_invalid_warns() {
        let id = Identity::generate();
        // "00".repeat(32): 32-byte all-zero scalar — VALID length so it gets
        // past BadLength and must fail on the secp256k1 scalar check. This is
        // the only coverage of the Err arm; keep it on the scalar path so a
        // regression dropping validation doesn't stay green.
        let n =
            note(Some(&"00".repeat(32)), Some(&id.enc_secret_hex())).expect("both env vars set");
        assert!(n.contains("unusable"), "got: {n}");
        assert!(
            n.contains("secp256k1"),
            "must be the scalar check, got: {n}"
        );
    }
}
