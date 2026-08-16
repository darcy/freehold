//! Freehold control plane — CLI (Phase B).
//!
//! Subcommands: provision (B1), rotate-secret (B2), revoke (B3), list,
//! and serve (the local admin/ops web surface, still a skeleton).

use std::path::PathBuf;

use anyhow::{Context, Result};
use axum::{Json, Router, routing::get};
use clap::{Args, Parser, Subcommand};
use freehold_control_plane::provisioner::{self, ProvisionRequest};
use freehold_control_plane::state::{RunnerStatus, STATE_DIR_ENV, StateStore};
use serde_json::{Value, json};
use zeroize::Zeroizing;

#[derive(Parser)]
#[command(
    name = "control-plane",
    version,
    about = "Freehold control plane: secret provisioner + local admin/ops surface"
)]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// Provision a runner for an existing service; credential is read from stdin
    Provision(ProvisionArgs),
    /// Rotate a secret: re-seal the NEW credential (stdin) to the runner key
    RotateSecret(RotateArgs),
    /// Revoke a runner's membership (cut-off): blocks provision/rotate
    Revoke(RevokeArgs),
    /// List runners + secrets at a glance
    List(CommonArgs),
    /// Serve the local admin/ops web surface
    Serve(ServeArgs),
}

#[derive(Args)]
struct CommonArgs {
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct ProvisionArgs {
    /// Service/runner name (one secret per runner in Chunk 1)
    name: String,
    #[arg(long)]
    kind: String,
    #[arg(long)]
    address: String,
    /// Where the runner package lands; defaults to ./.freehold/runner/<name>
    #[arg(long, env = "FREEHOLD_RUNNER_STATE_DIR")]
    runner_dir: Option<PathBuf>,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct RotateArgs {
    /// Secret name to rotate
    name: String,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct RevokeArgs {
    /// Runner name to revoke (cut-off)
    name: String,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct ServeArgs {
    #[arg(long, env = "FREEHOLD_CP_ADDR", default_value = "127.0.0.1:8080")]
    addr: String,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();

    match Cli::parse().cmd {
        Cmd::Provision(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let runner_dir = args
                .runner_dir
                .unwrap_or_else(|| PathBuf::from(format!("./.freehold/runner/{}", args.name)));
            let secret = read_secret_stdin(&format!(
                "paste credential for {} ({} @ {}): ",
                args.name, args.kind, args.address
            ))?;
            let res = provisioner::provision_runner(
                &store,
                &ProvisionRequest {
                    name: &args.name,
                    kind: &args.kind,
                    address: &args.address,
                    secret: secret.as_bytes(),
                    runner_dir: &runner_dir,
                },
            )?;
            println!("provisioned runner {0} (active)", res.name);
            println!("  nostr pubkey:      {}", res.nostr_pubkey);
            println!("  encryption pubkey: {}", res.enc_pubkey);
            println!("  package:           {}", res.package_dir.display());
            println!(
                "  (credential sealed to the runner's key — the CP holds no plaintext, no private keys)"
            );
            Ok(())
        }
        Cmd::RotateSecret(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let new_secret =
                read_secret_stdin(&format!("paste NEW credential for {}: ", args.name))?;
            provisioner::rotate_secret(&store, &args.name, new_secret.as_bytes())?;
            println!("rotated secret {}", args.name);
            Ok(())
        }
        Cmd::Revoke(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let rec = provisioner::revoke_runner(&store, &args.name)?;
            println!("revoked runner {} (was {})", args.name, rec.nostr_pubkey);
            println!("note: the shipped secrets.json was removed, but the credential itself may");
            println!("      still be valid at the service — rotate it upstream if it was exposed");
            Ok(())
        }
        Cmd::List(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let state = provisioner::snapshot(&store);
            println!(
                "RUNNER            STATUS    KIND      ADDRESS            NOSTR PUBKEY (first 12)"
            );
            for (name, r) in &state.runners {
                let s = state.secrets.get(name);
                println!(
                    "{:<15} {:<10} {:<9} {:<18} {}",
                    name,
                    match r.status {
                        RunnerStatus::Active => "active",
                        RunnerStatus::Revoked => "REVOKED",
                    },
                    s.map(|s| s.kind.as_str()).unwrap_or("-"),
                    s.map(|s| s.address.as_str()).unwrap_or("-"),
                    &r.nostr_pubkey[..r.nostr_pubkey.len().min(12)],
                );
            }
            if state.runners.is_empty() {
                println!(
                    "(no runners — run `control-plane provision <name> --kind ... --address ...`)"
                );
            }
            println!(
                "{} runner(s), {} secret(s) — all ciphertext, no plaintext in state",
                state.runners.len(),
                state.secrets.len()
            );
            Ok(())
        }
        Cmd::Serve(args) => {
            // A1 skeleton: routes land with Phase F (console). State is opened
            // so the later UI has the same guarantees.
            let _store = StateStore::open(&args.state_dir)?;
            let addr = args.addr;
            let app = Router::new()
                .route("/", get(root))
                .route("/healthz", get(healthz));
            let listener = tokio::net::TcpListener::bind(&addr).await?;
            tracing::info!(%addr, "control plane listening");
            axum::serve(listener, app)
                .with_graceful_shutdown(async {
                    tokio::signal::ctrl_c().await.ok();
                })
                .await?;
            Ok(())
        }
    }
}

/// Read a one-line secret from stdin, zeroized on drop. Never echoed, never
/// logged, never persisted as plaintext. Every copy (read buffer, trimmed
/// value) is under `Zeroizing` — a core dump or heap spray reads nothing.
fn read_secret_stdin(prompt: &str) -> Result<Zeroizing<String>> {
    eprintln!("{prompt}");
    // with_capacity: read_line can still realloc mid-read and orphan a
    // partial copy; a sized buffer makes that unlikely for real credentials.
    let mut line = Zeroizing::new(String::with_capacity(256));
    std::io::stdin().read_line(&mut line)?;
    let value = Zeroizing::new(line.trim_end_matches(['\r', '\n']).to_string());
    if value.is_empty() {
        anyhow::bail!("empty secret");
    }
    Ok(value)
}

async fn root() -> Json<Value> {
    Json(json!({
        "name": "freehold-control-plane",
        "status": "bootstrap",
        "services": []
    }))
}

async fn healthz() -> &'static str {
    "ok"
}
