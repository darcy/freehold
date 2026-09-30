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
    /// Enroll this host's runner: ensure the identity (mint on-guest if
    /// missing, never re-key) and print the pubkeys + the provision_runner
    /// arguments for the control-plane enroll call.
    Enroll(EnrollArgs),
    /// Start the MCP tool server
    Serve(ServeArgs),
}

#[derive(Args)]
struct EnrollArgs {
    /// State dir for identity files (the serve unit's --state-dir)
    #[arg(long, env = "FREEHOLD_STATE_DIR", default_value = "./.freehold")]
    state_dir: PathBuf,
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
    /// Address to bind the MCP server (non-loopback requires --allow-remote)
    #[arg(long, env = "FREEHOLD_RUNNER_ADDR", default_value = "127.0.0.1:8787")]
    addr: String,
    /// The relay this runner belongs to (Chunk 2.6.1: the whitelist is read
    /// LIVE from the runner's own NIP-29 channel roster; omit for
    /// package-only grants — local/loopback).
    #[arg(long, env = "FREEHOLD_RELAY_URL")]
    relay_url: Option<String>,
    /// The RELAY's nostr pubkey (64-hex) — the trust anchor that SIGNS
    /// membership rosters (kind 39002). REQUIRED with --relay-url (fail-fast:
    /// a whitelist accepted from any other author is a self-admission hole).
    #[arg(long, env = "FREEHOLD_RELAY_PUBKEY")]
    relay_pubkey: Option<String>,
    /// The relay's CANONICAL URL for NIP-98 signing (public https://<domain>)
    /// when --relay-url is a LAN dial (http://<domain>:3000). Omit to sign the
    /// dial URL (a LAN-only relay with no public domain).
    #[arg(long, env = "FREEHOLD_RELAY_AUTH_URL")]
    relay_auth_url: Option<String>,
    /// Allow a non-loopback bind. Every privileged call is signed (audience +
    /// grant + 60s window), so a LAN bind is safe for runners agents reach over
    /// the network; off by default so a runner is never exposed by accident.
    /// Boolish env values (1/yes/on) are accepted — the env-file convention.
    #[arg(
        long,
        env = "FREEHOLD_RUNNER_ALLOW_REMOTE",
        default_value_t = false,
        value_parser = clap::builder::BoolishValueParser::new()
    )]
    allow_remote: bool,
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

/// The enroll printout: whether the identity was kept or minted, the pubkeys
/// (the ONLY thing that ever crosses the host boundary), and the exact
/// provision_runner arguments to come back with. Pure + testable.
fn enroll_report(id: &Identity, kept: bool) -> String {
    let mut out = String::new();
    if kept {
        out.push_str(
            "identity already present — kept (enroll never re-keys; `runner keys init --force` \
             replaces it and orphans every grant on it)\n",
        );
    } else {
        out.push_str("identity minted on this host — the private keys never leave it\n");
    }
    out.push_str(&format!(
        "nostr pubkey (hex):      {}\n",
        id.nostr_pubkey_hex()
    ));
    out.push_str(&format!(
        "encryption pubkey (hex): {}\n",
        id.enc_pubkey_hex()
    ));
    out.push_str("\nEnroll with the control plane (the freehold CP toolset, provision_runner):\n");
    out.push_str("  name: <target>-local-<identity>   (e.g. freehold-dev-local-lxcadmin)\n");
    out.push_str(
        "  kind: local, hosted: self, host: <the PINNED NAME from Compute's report — not a raw IP>, address: <user>@<host>\n",
    );
    out.push_str(&format!(
        "  pubkey: {}, enc_pubkey: {}\n",
        id.nostr_pubkey_hex(),
        id.enc_pubkey_hex()
    ));
    out.push_str("  grant_to: [the agent that works this box]\n");
    out
}

/// The enroll identity leg: keep + load the existing identity, or mint a
/// fresh one — and when a file EXISTS but will not load, fail loudly instead
/// of minting over it (a re-key orphans every grant + sealed credential on
/// the guest). Returns the identity and whether it was kept.
fn enroll_identity(
    state_dir: &Path,
    env_nsec: Option<String>,
    env_enc: Option<String>,
) -> anyhow::Result<(Identity, bool)> {
    // Serve gives the env identity priority — so enroll prints THAT (with no
    // file present, an env identity still runs; no file is minted over it).
    if let Ok(id) = Identity::load_with(state_dir, env_nsec.clone(), env_enc.clone()) {
        return Ok((id, true));
    }
    // An identity file that exists but will not load: loud failure — enroll
    // NEVER re-keys a guest (that orphans every grant + sealed credential).
    let identity_path = state_dir.join(identity::IDENTITY_FILE);
    if identity_path.exists() {
        return Err(anyhow::anyhow!(
            "identity at {} exists but is unreadable — enroll never re-keys; \
             repair or remove it by hand, or start from a fresh guest",
            identity_path.display()
        ));
    }
    let id = Identity::generate();
    let written = id.write_to_dir(state_dir)?;
    println!("wrote identity to {}", written.display());
    Ok((id, false))
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
        Cmd::Enroll(args) => {
            // Same env priority as serve: with FREEHOLD_RUNNER_NSEC/ENC set,
            // the ENV identity is what will run, so it is what enroll prints.
            let env_nsec = std::env::var(identity::NSEC_ENV).ok();
            let env_enc = std::env::var(identity::ENC_ENV).ok();
            let (id, kept) = enroll_identity(&args.state_dir, env_nsec.clone(), env_enc.clone())?;
            println!("{}", enroll_report(&id, kept));
            if let Some(note) = env_shadow_note(env_nsec, env_enc, &args.state_dir) {
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
                    relay_pubkey: args.relay_pubkey.clone(),
                    relay_auth_url: args.relay_auth_url.clone(),
                },
                args.allow_remote,
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
    fn enroll_report_minted_names_both_pubkeys() {
        let id = Identity::generate();
        let r = enroll_report(&id, false);
        assert!(r.contains("minted on this host"), "got: {r}");
        assert!(r.contains(&id.nostr_pubkey_hex()), "got: {r}");
        assert!(r.contains(&id.enc_pubkey_hex()), "got: {r}");
        assert!(r.contains("hosted: self"), "must name the enroll mode: {r}");
    }

    #[test]
    fn enroll_report_kept_never_rekeys() {
        let id = Identity::generate();
        let r = enroll_report(&id, true);
        assert!(r.contains("kept"), "got: {r}");
        assert!(!r.contains("minted"), "got: {r}");
        assert!(r.contains(&id.nostr_pubkey_hex()), "got: {r}");
    }

    #[test]
    fn enroll_identity_mints_then_keeps_never_rekeys() {
        let dir = std::env::temp_dir().join(format!("fh-enroll-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        // Missing: mints.
        let (id, kept) = enroll_identity(&dir, None, None).expect("mints when absent");
        assert!(!kept);
        // Present + valid: KEPT (the same identity — never re-keyed).
        let (again, kept2) = enroll_identity(&dir, None, None).expect("keeps when present");
        assert!(kept2);
        assert_eq!(again.nostr_pubkey_hex(), id.nostr_pubkey_hex());
        // Present but CORRUPT: loud failure, never a mint-over.
        std::fs::write(dir.join(identity::IDENTITY_FILE), "{ not json").unwrap();
        let err = enroll_identity(&dir, None, None).unwrap_err();
        assert!(
            err.to_string().contains("never re-keys"),
            "corrupt identity must fail loudly, got: {err}"
        );
        let _ = std::fs::remove_dir_all(&dir);
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
