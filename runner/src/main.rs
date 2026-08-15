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
    #[arg(long, env = "FREEFOLD_STATE_DIR", default_value = "./.freehold")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct ServeArgs {
    /// Dir holding the runner identity
    #[arg(long, env = "FREEFOLD_STATE_DIR", default_value = "./.freehold")]
    state_dir: PathBuf,
    /// Address to bind the MCP server (localhost only)
    #[arg(long, env = "FREEFOLD_RUNNER_ADDR", default_value = "127.0.0.1:8787")]
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
            let id = Identity::generate();
            let path = id.write_to_dir(&args.state_dir)?;
            println!("wrote identity to {}", path.display());
            println!("nostr pubkey (hex):      {}", id.nostr_pubkey_hex());
            println!("encryption pubkey (hex): {}", id.enc_pubkey_hex());
            println!(
                "private keys live in {}/{} (0600) — do not commit",
                args.state_dir.display(),
                identity::IDENTITY_FILE
            );
            Ok(())
        }
        Cmd::Serve(args) => {
            let id = Identity::load(&args.state_dir).with_context(|| {
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
            let (bound, server) = freehold_runner::mcp::serve(&args.addr).await?;
            tracing::info!(addr = %bound, "runner MCP server listening");
            server.await?;
            Ok(())
        }
    }
}
