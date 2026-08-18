//! Freehold control plane — CLI (Phase B).
//!
//! Subcommands: provision (B1), rotate-secret (B2), revoke (B3), list,
//! and serve (the local admin/ops web surface, still a skeleton).

use std::io::Read;
use std::path::PathBuf;
use std::sync::Arc;

use anyhow::{Context, Result};
use clap::{Args, Parser, Subcommand};
use freehold_control_plane::console::Console;
use freehold_control_plane::provisioner::{self, ProvisionRequest};
use freehold_control_plane::state::{RunnerStatus, STATE_DIR_ENV, StateStore};
use freehold_control_plane::web;
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
    /// Create an agent identity (private key stays in the CP state dir, 0600)
    AgentCreate(AgentArgs),
    /// Grant another agent pubkey to a runner (re-ships the package)
    Grant(GrantArgs),
    /// Revoke two agent pubkey from to a runner (re-ships + relay replace)
    RevokeGrant(RevokeGrantArgs),
    /// Provision a runner for an existing service; credential is read from stdin
    Provision(ProvisionArgs),
    /// Rotate a secret: re-seal the NEW credential (stdin) to the runner key
    RotateSecret(RotateArgs),
    /// Revoke a runner's membership (cut-off): blocks provision/rotate
    Revoke(RevokeArgs),
    /// List runners + secrets at a glance
    List(CommonArgs),
    /// Print this state dir's console identity PUBKEY (64-hex, pubkey only —
    /// never the secret). Used by deploy-cp to name the box's fresh identity
    /// for relay-member add.
    Identity(CommonArgs),
    /// Serve the local admin/ops web surface
    Serve(ServeArgs),
}

#[derive(Args)]
struct CommonArgs {
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct AgentArgs {
    /// Agent name (identity file: <state-dir>/agent-<name>/identity.json)
    name: String,
    /// Replace an existing agent identity (irreversible — every runner
    /// granted to the old pubkey must be re-granted)
    #[arg(long)]
    force: bool,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct GrantArgs {
    /// Runner to grant
    name: String,
    /// Agent pubkey (Nostr x-only hex) allowed to call the runner
    pubkey: String,
    /// Relay to publish the new grant list to (Phase D; kind 30180)
    #[arg(long, env = "FREEHOLD_RELAY_URL")]
    relay_url: Option<String>,
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
    /// Agent pubkey(s) granted to call this runner (repeatable)
    #[arg(long)]
    grant: Vec<String>,
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
struct RevokeGrantArgs {
    /// Runner to revoke the grant from
    name: String,
    /// Agent pubkey (Nostr x-only hex) to drop
    pubkey: String,
    /// Relay to publish the shrunk list to (Phase D)
    #[arg(long, env = "FREEHOLD_RELAY_URL")]
    relay_url: Option<String>,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct RevokeArgs {
    /// Runner name to revoke (cut-off)
    name: String,
    /// Relay to publish an EMPTY grant list to (cut-off for relay-backed runners)
    #[arg(long, env = "FREEHOLD_RELAY_URL")]
    relay_url: Option<String>,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct ServeArgs {
    #[arg(long, env = "FREEHOLD_CP_ADDR", default_value = "127.0.0.1:8080")]
    addr: String,
    /// Comma-separated operator/admin Nostr pubkeys (64-hex). Non-empty =>
    /// NIP-98 console auth is ON and a non-loopback bind is allowed (C3.5);
    /// empty => the loopback-only posture (C3) holds.
    #[arg(long)]
    admin_pubkeys: Option<String>,
    /// The console's PUBLIC host (e.g. cp-freehold.example) when fronted by
    /// a proxy — the DNS-rebinding guard also allows this origin so the UI
    /// works over the domain.
    #[arg(long)]
    public_origin: Option<String>,
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
        Cmd::AgentCreate(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let agent_dir = args.state_dir.join(format!("agent-{}", args.name));
            let agent_id = agent_dir.join(freehold_core::identity::IDENTITY_FILE);
            if agent_id.exists() && !args.force {
                anyhow::bail!(
                    "agent {} already exists at {} — pass --force to replace (irreversible: \
                     every runner granted to the old pubkey must be re-granted)",
                    args.name,
                    agent_id.display()
                );
            }
            if args.force && agent_id.exists() {
                let target = freehold_core::identity::Identity::backup_identity(&agent_dir)?;
                println!("backed up old agent identity to {}", target.display());
            }
            let id = freehold_core::identity::Identity::generate();
            let written = id.write_to_dir(&agent_dir)?;
            println!("created agent {}", args.name);
            println!("  agent pubkey (hex): {}", id.nostr_pubkey_hex());
            println!("  identity:           {}", written.display());
            let _ = store;
            Ok(())
        }
        Cmd::Grant(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let grants = provisioner::grant_agent(&store, &args.name, &args.pubkey)?;
            if let Some(relay) = &args.relay_url {
                provisioner::publish_grants(&store, relay, &args.name, &grants, &args.state_dir)?;
                println!("published {} to the relay ({relay})", args.name);
            }
            println!("runner {} grants: {}", args.name, grants.len());
            for g in &grants {
                println!("  {}", g);
            }
            Ok(())
        }
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
                    grants: &args.grant,
                },
            )?;
            if args.grant.is_empty() {
                println!(
                    "  note: no agents granted — the runner denies all calls until `control-plane grant {} <pubkey>`",
                    args.name
                );
            }
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
        Cmd::RevokeGrant(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let grants = provisioner::revoke_grant(&store, &args.name, &args.pubkey)?;
            if let Some(relay) = &args.relay_url {
                provisioner::publish_grants(&store, relay, &args.name, &grants, &args.state_dir)?;
                println!("published {} grants to the relay ({relay})", args.name);
            }
            println!("runner {} grants: {}", args.name, grants.len());
            for g in &grants {
                println!("  {}", g);
            }
            Ok(())
        }
        Cmd::Revoke(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let rec = provisioner::revoke_runner(&store, &args.name)?;
            if let Some(relay) = &args.relay_url {
                // cut-off on the relay too: an EMPTY grant list denies every caller
                provisioner::publish_grants(&store, relay, &args.name, &[], &args.state_dir)?;
                println!(
                    "published empty grants for {} to the relay ({relay})",
                    args.name
                );
            }
            println!("revoked runner {} (was {})", args.name, rec.nostr_pubkey);
            println!("note: the shipped secrets.json was removed, but the credential itself may");
            println!("      still be valid at the service — rotate it upstream if it was exposed");
            Ok(())
        }
        Cmd::Identity(args) => {
            // load_or_create: a fresh state dir gets a NEW identity here
            // (the box never receives a pre-made keypair — see deploy-cp).
            let console = Console::load_or_create(&args.state_dir).with_context(|| {
                format!("loading console identity in {}", args.state_dir.display())
            })?;
            println!("{}", console.pubkey());
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
            let store = StateStore::open(&args.state_dir)?;
            // C3.5 — console authentication (NIP-98 operator login). The
            // admin whitelist is seeded at first serve via --admin-pubkeys
            // (the bootstrap's --operator-pubkey); it persists in state.json
            // so a restart keeps it. Auth ON relaxes the bind guard; auth
            // OFF keeps the loopback-only refusal (C3) byte-for-byte.
            let mut admins: Vec<String> = Vec::new();
            if let Some(raw) = &args.admin_pubkeys {
                for pk in raw.split(',').map(str::trim).filter(|s| !s.is_empty()) {
                    if !freehold_control_plane::is_hex64(pk) {
                        anyhow::bail!(
                            "--admin-pubkeys entries must be 64-hex Nostr pubkeys (got {pk:?})"
                        );
                    }
                    admins.push(pk.to_string());
                }
                if !admins.is_empty() {
                    store.set_admins(admins.clone());
                    store.save()?;
                }
            } else {
                admins = store.admins();
            }
            let auth = if admins.is_empty() {
                None
            } else {
                tracing::info!(
                    admins = admins.len(),
                    "console auth enabled (NIP-98) — non-loopback bind allowed"
                );
                Some(std::sync::Arc::new(web::Auth::new(admins)))
            };
            if auth.is_none() {
                // C3: the unauthenticated console stays loopback-only,
                // enforced here AND at deploy time.
                freehold_control_plane::validate_loopback_bind(&args.addr)
                    .map_err(anyhow::Error::msg)?;
            }
            let console = Console::load_or_create(&args.state_dir)?;
            tracing::info!(pubkey = %console.pubkey(), "console agent ready");
            let addr = args.addr;
            let app = web::router(Arc::new(store), console, auth, args.public_origin.clone());
            let listener = tokio::net::TcpListener::bind(&addr).await?;
            tracing::info!(%addr, "control plane console listening");
            axum::serve(listener, app)
                .with_graceful_shutdown(async {
                    tokio::signal::ctrl_c().await.ok();
                })
                .await?;
            Ok(())
        }
    }
}

/// Read the secret from stdin (ALL of it — SSH private keys are multi-line
/// PEM, API keys are single lines), zeroized on drop. Never echoed, never
/// logged, never persisted as plaintext. Every copy (read buffer, trimmed
/// value) is under `Zeroizing` — a core dump or heap spray reads nothing.
fn read_secret_stdin(prompt: &str) -> Result<Zeroizing<String>> {
    eprintln!("{prompt}");
    // with_capacity: reads can still realloc and orphan a partial copy; a
    // sized buffer keeps that unlikely for real credentials.
    let mut buf = Zeroizing::new(String::with_capacity(256));
    std::io::stdin().read_to_string(&mut buf)?;
    let value = Zeroizing::new(buf.trim_end_matches(['\r', '\n']).to_string());
    if value.is_empty() {
        anyhow::bail!("empty secret");
    }
    Ok(value)
}
