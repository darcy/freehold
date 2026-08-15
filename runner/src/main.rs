use std::fs;
use std::path::PathBuf;

use anyhow::Context;
use clap::{Args, Parser, Subcommand};
use freehold_runner::identity::{self, Identity};

#[derive(Parser)]
#[command(name = "runner", version, about = "Freehold runner: privileged MCP tool server")]
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
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info".into()),
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
                // Rotate: the ORIGINAL key is the one --force exists to make
                // survivable — a second --force must not clobber its backup.
                let mut target = args.state_dir.join("identity.json.bak");
                let mut i = 1;
                while target.exists() {
                    target = args.state_dir.join(format!("identity.json.bak.{i}"));
                    i += 1;
                }
                fs::copy(&path, &target)?;
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
            // A4: hand `id` to the server — decrypt secrets for exec, sign audit events.
            let (bound, server) = freehold_runner::mcp::serve(&args.addr).await?;
            tracing::info!(addr = %bound, "runner MCP server listening");
            tokio::select! {
                _ = tokio::signal::ctrl_c() => tracing::info!("shutting down"),
                _ = server => {}
            }
            Ok(())
        }
    }
}
