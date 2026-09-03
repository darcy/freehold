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
use freehold_control_plane::state::{
    RunnerRecord, RunnerStatus, STATE_DIR_ENV, SecretRecord, StateStore,
};
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
    /// C2/D2: ADOPT an EXISTING runner into the console registry from its
    /// shipped package (no credential re-shipping, no re-sealing).
    Adopt(AdoptArgs),
    /// Grant another agent pubkey to a runner (re-ships the package)
    Grant(GrantArgs),
    /// Revoke two agent pubkey from to a runner (re-ships + relay replace)
    RevokeGrant(RevokeGrantArgs),
    /// Provision a runner for an existing service; credential is read from stdin
    Provision(ProvisionArgs),
    /// Rotate a secret: re-seal the NEW credential (stdin) to the runner key
    RotateSecret(RotateArgs),
    /// Add an EXTRA named secret (stdin) to an existing runner's package —
    /// sealed to the runner's key under aad = the name (C0: litellm carries
    /// master + provider + postgres together).
    AddSecret(AddSecretArgs),
    /// Revoke a runner's membership (cut-off): blocks provision/rotate
    Revoke(RevokeArgs),
    /// C0 DNS records: add/rm/list — the CP resolver's explicit surface.
    /// `add` writes state + re-syncs dnsmasq addn-hosts (core service).
    Dns(DnsArgs),
    /// List runners + secrets at a glance
    List(CommonArgs),
    /// Chunk 2.6.1: REBUILD the CP view from the relay's runner-profile
    /// channel messages (kind 9, t=fh-profile) — a respawned CP folds
    /// instead of carrying state ("disposable CP"). Idempotent: same relay
    /// → same store; a re-run converges. Restored records carry NO
    /// ciphertext/package path (the relay never holds secret material) —
    /// re-run `adopt` per runner to re-arm the package.
    Rebuild(RebuildArgs),
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
    /// Agent pubkey (npub or 64-hex) allowed to call the runner; omitted =
    /// the state dir's own ops-agent identity ({state_dir}/agent-ops) — the
    /// identity the orchestrator drives with, so the common first-run grant
    /// needs no argument
    #[arg(long)]
    pubkey: Option<String>,
    /// Relay to publish the new grant list to (Phase D; kind 30180)
    #[arg(long, env = "FREEHOLD_RELAY_URL")]
    relay_url: Option<String>,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct AdoptArgs {
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
    /// Service/runner name (must match the package's own name)
    name: String,
    #[arg(long)]
    kind: String,
    #[arg(long)]
    address: String,
    /// The runner's existing package dir (identity.json + secrets.json)
    #[arg(long)]
    package_dir: PathBuf,
    /// The runner's MCP listen address (console readiness probing)
    #[arg(long)]
    mcp_addr: Option<String>,
    /// Relay to sync the runner's NIP-29 channel to (Chunk 2.6.1)
    #[arg(long, env = "FREEHOLD_RELAY_URL")]
    relay_url: Option<String>,
}

#[derive(Args)]
struct ProvisionArgs {
    /// Service/runner name (one target credential per runner; extras via
    /// add-secret for C0's litellm master/provider/postgres set)
    name: String,
    /// Read the credential from this ENV VAR instead of stdin (headless
    /// pipelines: the value arrives as an env-injected secret, never argv).
    #[arg(long)]
    secret_env: Option<String>,
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
    /// Runner risk class override (safe|risky-install|risky-host); a
    /// kind-based default applies when unset (POC_CHUNK3 §0.02).
    #[arg(long)]
    risk: Option<String>,
    /// Relay to sync the runner's NIP-29 channel to (Chunk 2.6.1)
    #[arg(long, env = "FREEHOLD_RELAY_URL")]
    relay_url: Option<String>,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct RotateArgs {
    /// Secret name to rotate
    name: String,
    /// Relay to re-sync (rotated_at flip in the channel meta) (Chunk 2.6.1)
    #[arg(long, env = "FREEHOLD_RELAY_URL")]
    relay_url: Option<String>,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct AddSecretArgs {
    /// Runner to add the extra secret to
    runner: String,
    /// Extra secret name (e.g. provider-key) — the agent requests this name
    /// in an exec's `secrets`, and the runner injects + redacts its value.
    name: String,
    /// Read the value from this ENV VAR instead of stdin (never argv).
    #[arg(long)]
    secret_env: Option<String>,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct DnsArgs {
    #[command(subcommand)]
    cmd: DnsSub,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Subcommand)]
enum DnsSub {
    /// Add/upsert a record: NAME IP [source] — e.g. `dns add relay 192.168.30.8 record_lxc`
    Add(DnsAddArgs),
    /// Remove a record (missing = ok)
    Rm { name: String },
    /// List the records table + the rendered addn-hosts
    List,
    /// (Re)write the addn-hosts file + reload dnsmasq without changing state
    Sync,
}

#[derive(Args)]
struct DnsAddArgs {
    name: String,
    ip: String,
    /// World domain suffix (the guests' resolv.conf search base) — when set,
    /// addn-hosts renders bare AND `<name>.<domain>` so search-first lookups
    /// hit the split-horizon answer instead of leaking upstream.
    #[arg(long)]
    domain: Option<String>,
    /// Registration source (record_lxc <role>, litellm-apply, manual)
    #[arg(default_value = "manual")]
    source: String,
}

#[derive(Args)]
struct RevokeGrantArgs {
    /// Runner to revoke the grant from
    name: String,
    /// Agent pubkey (npub or 64-hex) to drop; omitted = the state dir's own
    /// ops-agent identity (the orchestrator's signing identity)
    #[arg(long)]
    pubkey: Option<String>,
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
    /// Relay to revoke the runner channel on (Chunk 2.6.1 cut-off)
    #[arg(long, env = "FREEHOLD_RELAY_URL")]
    relay_url: Option<String>,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct RebuildArgs {
    /// Relay to fold runner channel metadata from (Chunk 2.6.1)
    #[arg(long, env = "FREEHOLD_RELAY_URL")]
    relay_url: String,
    #[arg(long, env = STATE_DIR_ENV, default_value = "./.freehold/control-plane")]
    state_dir: PathBuf,
}

#[derive(Args)]
struct ServeArgs {
    #[arg(long, env = "FREEHOLD_CP_ADDR", default_value = "127.0.0.1:8080")]
    addr: String,
    /// The relay scope this console operates as (Chunk 2.6.1): the web UI
    /// syncs runner channels / membership against it and serves the runner
    /// channel view. Persisted in state.json.
    #[arg(long, env = "FREEHOLD_RELAY_URL")]
    relay_url: Option<String>,
    /// The RELAY's nostr pubkey (Chunk 2.6.1) — the trust anchor that signs
    /// membership rosters; required for the runner channel view. Persisted
    /// in state.json.
    #[arg(long, env = "FREEHOLD_RELAY_PUBKEY")]
    relay_pubkey: Option<String>,
    /// The relay's COMMUNITY host (the domain) — sent as the `Host` header
    /// on relay reads when the scope URL is a LAN address the relay's
    /// strict host map would otherwise refuse.
    #[arg(long)]
    relay_host: Option<String>,
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
            let pubkey = resolve_grant_pubkey(args.pubkey.as_deref(), &args.state_dir)?;
            let grants = provisioner::grant_agent(&store, &args.name, &pubkey)?;
            if let Some(relay) = &args.relay_url {
                // Chunk 2.6.1: grants are channel MEMBERSHIP — the next
                // roster read by the runner (per call) includes the agent.
                provisioner::put_user_membership(
                    &store,
                    relay,
                    &args.name,
                    &pubkey,
                    &args.state_dir,
                )?;
                println!(
                    "added {} to the runner channel on the relay ({relay})",
                    args.name
                );
            }
            println!("runner {} grants: {}", args.name, grants.len());
            for g in &grants {
                println!("  {}", g);
            }
            Ok(())
        }
        Cmd::Adopt(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let runner = provisioner::adopt_runner(
                &store,
                &args.name,
                &args.kind,
                &args.address,
                &args.package_dir,
                args.mcp_addr.clone(),
                None,
            )?;
            println!(
                "adopted runner {} (active) from its shipped package",
                runner.nostr_pubkey
            );
            println!("  package:           {}", runner.package_dir.display());
            println!("  mcp addr:          {:?}", runner.mcp_addr);
            println!("  (credential stays sealed in the package; adopt ships nothing)");
            if let Some(relay) = &args.relay_url {
                provisioner::sync_runner_channel(&store, relay, &args.name, &args.state_dir)?;
                println!("synced runner channel on the relay ({relay})");
            }
            Ok(())
        }
        Cmd::Provision(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let runner_dir = args
                .runner_dir
                .unwrap_or_else(|| PathBuf::from(format!("./.freehold/runner/{}", args.name)));
            // SSH runners: freehold GENERATES the runner's keypair and the
            // operator installs the PUBLIC half on the target's
            // authorized_keys — nobody pastes an existing key. Other kinds:
            // the operator pastes the API credential once.
            let (secret, generated_pubkey): (zeroize::Zeroizing<String>, Option<String>) =
                if let Some(env) = &args.secret_env {
                    let v = std::env::var(env).map_err(|_| {
                        anyhow::anyhow!("--secret-env {env} is not set in this environment")
                    })?;
                    (zeroize::Zeroizing::new(v), None)
                } else if args.kind == "ssh" {
                    let (privk, pubk) = freehold_core::identity::generate_ssh_keypair(&args.name)
                        .map_err(|e| anyhow::anyhow!("ssh keygen: {e}"))?;
                    (
                        zeroize::Zeroizing::new(String::from_utf8_lossy(&privk).into_owned()),
                        Some(pubk),
                    )
                } else {
                    (
                        read_secret_stdin(&format!(
                            "paste credential for {} ({} @ {}): ",
                            args.name, args.kind, args.address
                        ))?,
                        None,
                    )
                };
            let res = provisioner::provision_runner(
                &store,
                &ProvisionRequest {
                    name: &args.name,
                    kind: &args.kind,
                    address: &args.address,
                    secret: secret.as_bytes(),
                    runner_dir: &runner_dir,
                    grants: &args.grant,
                    risk_level: args.risk.as_deref(),
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
            if let Some(pubk) = &generated_pubkey {
                println!(
                    "  PUBLIC KEY — add this line to {}'s ~/.ssh/authorized_keys:",
                    args.address
                );
                println!("  {pubk}");
                println!(
                    "  (the private half is the sealed credential — it never leaves this box)"
                );
            }
            if let Some(relay) = &args.relay_url {
                provisioner::sync_runner_channel(&store, relay, &args.name, &args.state_dir)?;
                println!("synced runner channel on the relay ({relay})");
            }
            Ok(())
        }
        Cmd::AddSecret(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let value: zeroize::Zeroizing<String> = if let Some(env) = &args.secret_env {
                zeroize::Zeroizing::new(std::env::var(env).map_err(|_| {
                    anyhow::anyhow!("--secret-env {env} is not set in this environment")
                })?)
            } else {
                read_secret_stdin(&format!(
                    "paste extra secret {0} for runner {1}: ",
                    args.name, args.runner
                ))?
            };
            provisioner::add_secret(&store, &args.runner, &args.name, value.as_bytes())?;
            println!(
                "added extra secret {0} to runner {1} (sealed to the runner's key)",
                args.name, args.runner
            );
            Ok(())
        }
        Cmd::RotateSecret(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let new_secret =
                read_secret_stdin(&format!("paste NEW credential for {}: ", args.name))?;
            provisioner::rotate_secret(&store, &args.name, new_secret.as_bytes())?;
            if let Some(relay) = &args.relay_url {
                // rotated_at flipped in state by rotate_secret — the meta
                // re-publish carries the fresh stamp (Chunk 2.6.1).
                provisioner::sync_runner_channel(&store, relay, &args.name, &args.state_dir)?;
                println!("synced runner channel on the relay ({relay})");
            }
            println!("rotated secret {}", args.name);
            Ok(())
        }
        Cmd::RevokeGrant(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let pubkey = resolve_grant_pubkey(args.pubkey.as_deref(), &args.state_dir)?;
            let grants = provisioner::revoke_grant(&store, &args.name, &pubkey)?;
            if let Some(relay) = &args.relay_url {
                provisioner::remove_user_membership(
                    &store,
                    relay,
                    &args.name,
                    &pubkey,
                    &args.state_dir,
                )?;
                println!(
                    "removed {} from the runner channel on the relay ({relay})",
                    args.name
                );
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
                // cut-off on the relay too: removing the RUNNER from its own
                // channel denies every caller (its roster read fails closed),
                // and the meta's status flip makes the revoke visible to folds.
                provisioner::revoke_runner_channel(&store, relay, &args.name, &args.state_dir)?;
                println!("revoked {} on the relay ({relay})", args.name);
            }
            println!("revoked runner {} (was {})", args.name, rec.nostr_pubkey);
            println!("note: the shipped secrets.json was removed, but the credential itself may");
            println!("      still be valid at the service — rotate it upstream if it was exposed");
            Ok(())
        }
        Cmd::Dns(args) => {
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            let sync = || {
                dns_sync_resolver(&store, &args.state_dir)
                    .map_err(|e| anyhow::anyhow!("resolver sync failed: {e}"))
            };
            match args.cmd {
                DnsSub::Add(add) => {
                    if add.domain.is_some() {
                        freehold_control_plane::dns::set_resolver_domain(
                            &store,
                            add.domain.as_deref(),
                        )?;
                    }
                    let rec = freehold_control_plane::dns::upsert(
                        &store,
                        &add.name,
                        &add.ip,
                        &add.source,
                    )?;
                    sync()?;
                    println!(
                        "dns record {} -> {} (source: {})",
                        add.name, rec.ip, rec.source
                    );
                }
                DnsSub::Rm { name } => {
                    freehold_control_plane::dns::remove(&store, &name)?;
                    sync()?;
                    println!("dns record {name} removed");
                }
                DnsSub::List => {
                    let snap = store.snapshot();
                    if snap.dns.is_empty() {
                        println!("(no dns records — the resolver forwards everything upstream)");
                    }
                    for (name, rec) in &snap.dns {
                        println!("{name:<32} {}", rec.ip);
                        println!("  source: {} · created: {}", rec.source, rec.created_at);
                    }
                    println!("--- addn-hosts ---");
                    print!(
                        "{}",
                        freehold_control_plane::dns::render_addn_hosts(
                            &snap.dns,
                            snap.resolver_domain.as_deref()
                        )
                    );
                }
                DnsSub::Sync => {
                    sync()?;
                    println!("dnsmasq addn-hosts synced + reloaded");
                }
            }
            Ok(())
        }
        Cmd::Rebuild(args) => {
            use std::collections::BTreeMap;
            let store = StateStore::open(&args.state_dir)
                .with_context(|| format!("opening CP state in {}", args.state_dir.display()))?;
            // load_or_create mirrors Identity: a fresh CP gets its identity
            // at first use, but the FOLD is author-gated — a new console's
            // pubkey isn't the snapshots' author, so it reads nothing until
            // it's re-admitted (the documented re-trust step).
            let console = Console::load_or_create(&args.state_dir).with_context(|| {
                format!("loading console identity in {}", args.state_dir.display())
            })?;
            let profiles = freehold_core::relay_http::query_runner_metas(
                &args.relay_url,
                &console.identity.nostr_pubkey_hex(),
                &console.identity.secret_seed(),
            )
            .map_err(|e| anyhow::anyhow!("meta query failed: {e}"))?;
            let mut runners = BTreeMap::new();
            let mut secrets = BTreeMap::new();
            for p in &profiles {
                let status = if p.status == "revoked" {
                    RunnerStatus::Revoked
                } else {
                    RunnerStatus::Active
                };
                runners.insert(
                    p.name.clone(),
                    RunnerRecord {
                        nostr_pubkey: p.nostr_pubkey.clone(),
                        enc_pubkey: p.enc_pubkey.clone(),
                        status,
                        package_dir: std::path::PathBuf::new(),
                        created_at: p.created_at,
                        mcp_addr: None,
                        risk_level: p.risk.clone(),
                    },
                );
                secrets.insert(
                    p.name.clone(),
                    SecretRecord {
                        runner: p.name.clone(),
                        kind: p.kind.clone(),
                        address: p.address.clone(),
                        ciphertext_hex: String::new(),
                        created_at: p.created_at,
                        rotated_at: p.rotated_at,
                    },
                );
            }
            let restored = runners.len();
            store.rebuild_from(runners, secrets)?;
            println!(
                "rebuilt {} runner record(s) from the relay ({})",
                restored, args.relay_url
            );
            println!(
                "  restored records carry no ciphertext/package path (the relay holds no secret \
                 material) — re-run `adopt` per runner to re-arm the package"
            );
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
            if let Some(url) = &args.relay_url {
                store.set_relay_url(Some(url.clone()))?;
                tracing::info!(relay = %url, "console relay scope set");
            }
            if let Some(host) = &args.relay_host {
                store.set_relay_host(Some(host.clone()))?;
                tracing::info!(host = %host, "console relay community host set");
            }
            if let Some(pk) = &args.relay_pubkey {
                if !freehold_control_plane::is_hex64(pk) {
                    anyhow::bail!("--relay-pubkey must be a 64-hex Nostr pubkey (got {pk:?})");
                }
                store.set_relay_pubkey(Some(pk.clone()))?;
                tracing::info!("console relay pubkey set");
            }
            if (args.relay_url.is_some()) != (args.relay_pubkey.is_some()) {
                anyhow::bail!(
                    "--relay-url and --relay-pubkey must be set TOGETHER (the roster view                      verifies relay-signed snapshots; one without the other is a misconfig)"
                );
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
fn resolve_grant_pubkey(
    explicit: Option<&str>,
    state_dir: &std::path::Path,
) -> anyhow::Result<String> {
    if let Some(pk) = explicit {
        return freehold_core::identity::parse_pubkey_input(pk).map_err(|e| anyhow::anyhow!("{e}"));
    }
    // default: the state dir's own ops-agent identity (the orchestrator's
    // signing identity) — the common first-run grant needs no argument
    let agent_dir = state_dir.join("agent-ops");
    let id = freehold_core::identity::Identity::load(&agent_dir)
        .map_err(|_| {
            anyhow::anyhow!(
                "no agent identity at {} — mint one: `runner keys init --state-dir {}`, or pass --pubkey",
                agent_dir.display(),
                agent_dir.display()
            )
        })?;
    Ok(id.nostr_pubkey_hex())
}

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
/// Write the addn-hosts file + reload dnsmasq — the resolver runs IN this
/// box (the CP LXC), so "local" exec IS the CP's own host. dnsmasq's
/// addn-hosts is watched; a SIGHUP forces an immediate reload. The file path
/// lives under the state dir (durable plane) so a compute teardown of the CP
/// LXC keeps the records rendering on the first boot before any re-sync.
fn dns_sync_resolver(
    store: &StateStore,
    state_dir: &std::path::Path,
) -> Result<(), freehold_control_plane::dns::DnsError> {
    let snap = store.snapshot();
    let path = freehold_control_plane::dns::addn_hosts_path(state_dir);
    freehold_control_plane::dns::sync_resolver(
        state_dir,
        &snap.dns,
        snap.resolver_domain.as_deref(),
        &|p: &std::path::Path, body: &str| -> Result<(), String> {
            std::fs::write(p, body).map_err(|e| e.to_string())
        },
        &|| -> Result<(), String> {
            // dnsmasq may not be installed yet (first boot): ensure it, then
            // point it at the file + reload. Install + restarts are
            // idempotent (apt returns ok on present; /etc/dnsmasq.d re-point
            // is a no-op once set).
            let _ = std::process::Command::new("sh")
                .arg("-c")
                .arg("command -v dnsmasq >/dev/null 2>&1 || (export DEBIAN_FRONTEND=noninteractive; apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq dnsmasq >/dev/null 2>&1); mkdir -p /etc/dnsmasq.d")
                .output()
                .map_err(|e| e.to_string())?;
            let conf = "/etc/dnsmasq.d/freehold-names.conf";
            let conf_body = format!(
                "addn-hosts={}\nno-negcache\n",
                freehold_control_plane::dns::addn_hosts_path(state_dir).display()
            );
            std::fs::write(conf, conf_body).map_err(|e| e.to_string())?;
            let _ = std::process::Command::new("sh")
                .arg("-c")
                .arg("systemctl enable dnsmasq >/dev/null 2>&1; systemctl restart dnsmasq >/dev/null 2>&1 || killall -HUP dnsmasq >/dev/null 2>&1; true")
                .output()
                .map_err(|e| e.to_string())?;
            Ok(())
        },
    )?;
    let _ = path; // path is used inside the closures above
    Ok(())
}
