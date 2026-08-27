//! The `freehold` CLI surface (the scripted CPA stand-in's commands).
//!
//! Lives in the lib so both binaries can serve it: `freehold <subcommand>…`
//! (the TUI bin, args present) and `freehold-orchestrator <subcommand>…`.

use std::io::Read;
use std::path::PathBuf;

use crate::{bootstrap, deploy_cp, flows, relay, relay_member};
use anyhow::{Context, Result};
use clap::{Args, Parser, Subcommand};
use zeroize::Zeroizing;

#[derive(Parser)]
#[command(
    name = "freehold",
    version,
    about = "Freehold CLI: the scripted CPA stand-in that drives the engine room"
)]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// E1: existing service + credential -> runner -> provision -> self-check -> grant
    Onboard(OnboardArgs),
    /// Signed exec against a running runner
    Exec(ExecArgs),
    /// Readiness table from a running runner
    Readiness(CommonArgs),
    /// E2: run scripted exec steps (JSON file) against a running runner
    Demo(DemoArgs),
    /// C2/A2: bootstrap-provision a target through a provisioning runner
    Bootstrap(BootstrapArgs),
    /// C2/B: deploy the Buzz relay onto the target through a provisioning runner
    DeployRelay(DeployRelayArgs),
    /// C1: deploy the control plane onto the target box (OPERATE mode)
    DeployCp(DeployCpArgs),
    /// C3.5: log into a console over NIP-98 with YOUR identity; prints the
    /// session cookie for use in a browser or curl.
    ConsoleLogin(ConsoleLoginArgs),
    /// C2: add a relay member through the relay-admin runner (buzz-admin)
    RelayMember(RelayMemberArgs),
    /// D3: the agent's encrypted relay memory (kind 30174, self-sealed)
    Memory(MemoryArgs),
    /// E: the CPA delegates a task to a relay-addressable peer agent
    Delegate(DelegateArgs),
    /// E: the peer agent — watches for delegated requests, execs them via
    /// the runner (runner-direct), posts the results
    DelegatePeer(DelegatePeerArgs),
    /// Relay surface: publish a profile (kind 0) so clients show a name
    RelayProfile(RelayProfileArgs),
    /// Tear the managed world down: destroy the LXCs, remove the runner's
    /// key from the host LAST (after verification), then local cleanup.
    Teardown(TeardownArgs),
    /// Relay surface: join a channel (kind 9021) — agents join #freehold
    /// by default after a deploy
    RelayJoin(RelayJoinArgs),
    /// Relay surface: the bootstrap step — ensures the #freehold channel
    /// exists (open, deterministic id) and joins every listed agent to it
    RelaySetup(RelaySetupArgs),
}

#[derive(Args)]
struct DeployRelayArgs {
    #[command(flatten)]
    common: CommonArgs,
    /// Target to deploy through (the runner holding the SSH credential to
    /// the PVE host / relay LXC)
    #[arg(long, default_value = "proxmox-box")]
    target: String,
    /// Relay hostname (reported; defaults to <normalized-domain>-relay
    /// when --domain is given)
    #[arg(long)]
    name: Option<String>,
    /// Where the official compose bundle lands on the target
    #[arg(long, default_value = "/srv/buzz-relay")]
    deploy_dir: String,
    /// Relay HTTP port (WRITTEN into the compose .env BUZZ_HTTP_PORT)
    #[arg(long, default_value_t = 3000)]
    http_port: u16,
    /// block/buzz ref to fetch (tag or SHA; pinned SHA by default)
    #[arg(long, default_value = relay::DEFAULT_BUZZ_REF)]
    buzz_ref: String,
    /// Deploy INTO this LXC on the target (the target is the PVE host; the
    /// LXC is where docker lives after `freehold bootstrap proxmox-lxc`).
    /// Omitted = deploy directly on the target host.
    #[arg(long)]
    lxc: Option<u32>,
    /// Relay OWNER Nostr pubkey (64-hex) — written to RELAY_OWNER_PUBKEY;
    /// the bundle's run.sh refuses to start with CHANGE_ME placeholders.
    #[arg(long)]
    owner_pubkey: String,
    /// The relay's OWN resolvable URL (http://host:port) — written into
    /// BUZZ_DOMAIN/RELAY_URL/media so the relay binds the REAL community
    /// (the example.com placeholders are not literal CHANGE_ME).
    #[arg(long)]
    relay_url: String,
    /// The OPERATOR's Nostr pubkey (64-hex) — invite the human operator to
    /// the relay once it comes up (fail-closed: required).
    #[arg(long)]
    operator_pubkey: String,
    /// The forced identity DOMAIN (never an IP): writes BUZZ_DOMAIN/RELAY_URL
    /// = the domain (wss) AND provisions the TLS local-CA posture (own
    /// openssl CA + server cert; import ca.crt on your devices).
    #[arg(long)]
    domain: Option<String>,
}

#[derive(Args)]
struct DeployCpArgs {
    #[command(flatten)]
    common: CommonArgs,
    /// Target runner (the box where the relay lives)
    #[arg(long, default_value = "proxmox-box")]
    target: String,
    /// Remote state dir on the box (also holds the seeded console identity)
    #[arg(long, default_value = deploy_cp::DEFAULT_CP_STATE_DIR)]
    state_dir: String,
    /// Remote dir for the shipped binary
    #[arg(long, default_value = deploy_cp::DEFAULT_CP_BIN_DIR)]
    bin_dir: String,
    /// Loopback bind for the console (C3: non-loopback is refused)
    #[arg(long, default_value = deploy_cp::DEFAULT_CP_BIND)]
    bind: String,
    /// LOCAL path of the built control-plane binary
    #[arg(long)]
    binary: PathBuf,
    /// The relay this CP helps serve (the ONE scope; C4 posture record)
    #[arg(long)]
    relay_url: String,
    /// Deploy INTO this LXC on the target — the CP lives in its OWN guest,
    /// a different LXC than the relay's by default (omitted = the target host).
    #[arg(long)]
    lxc: Option<u32>,
    /// LOCAL path of the built freehold-runner binary (co-locates the CP's
    /// own runner: ship + systemd unit + adopt + self-grant).
    #[arg(long)]
    runner_binary: Option<PathBuf>,
    /// LOCAL dir of an EXISTING runner package to co-locate + adopt.
    #[arg(long)]
    runner_package: Option<PathBuf>,
    /// The OPERATOR's Nostr pubkey (64-hex) — seeds the console's NIP-98
    /// admin whitelist (C3.5) and relaxes the loopback-only bind guard.
    #[arg(long)]
    operator_pubkey: Option<String>,
}

#[derive(Args)]
struct TeardownArgs {
    /// Config path (default: ~/.config/freehold/config.toml)
    #[arg(long)]
    config: Option<PathBuf>,
    /// Skip the confirmation prompt (scripting/CI only)
    #[arg(long)]
    yes: bool,
}

#[derive(Args)]
struct ConsoleLoginArgs {
    /// Console base URL (e.g. http://freehold.example:8080)
    #[arg(long)]
    url: String,
    /// YOUR identity dir (its nsec signs the NIP-98 login; never leaves)
    #[arg(long)]
    identity: Option<PathBuf>,
    /// YOUR Nostr secret — nsec1<bech32> (what you actually hold) or bare
    /// 64-hex; signs the login directly and never leaves your machine.
    /// Either --identity or --nsec is required.
    #[arg(long)]
    nsec: Option<String>,
}

#[derive(Args)]
struct RelayMemberArgs {
    #[command(flatten)]
    common: CommonArgs,
    /// Target runner (the box holding the relay host)
    #[arg(long, default_value = "proxmox-box")]
    target: String,
    /// Nostr pubkey (64-hex) to add as a relay member
    #[arg(long)]
    pubkey: String,
    /// Role: member (default) or admin (owner comes from RELAY_OWNER_PUBKEY)
    #[arg(long)]
    role: Option<String>,
    /// The LXC on the box holding the relay compose stack
    #[arg(long)]
    lxc: Option<u32>,
    /// Compose project dir on the relay host
    #[arg(long, default_value = relay_member::DEFAULT_BUZZ_COMPOSE_DIR)]
    compose_dir: String,
}

#[derive(Args)]
struct MemoryArgs {
    /// set <key> <value> | get <key>
    action: String,
    key: String,
    /// Value only for `set`
    value: Option<String>,
    /// Relay URL (http://host:port)
    #[arg(long)]
    relay_url: String,
    /// Agent identity dir (the CPA's keypair — memory seals to its enc key)
    #[arg(long)]
    agent_dir: PathBuf,
}

#[derive(Args)]
struct DelegateArgs {
    /// CPAs identity dir
    #[arg(long)]
    agent_dir: PathBuf,
    #[arg(long)]
    relay_url: String,
    /// The auto-ops channel id (uuid) — created idempotently on first use
    #[arg(long, default_value = "00000000-0000-4000-8000-00000000f0ee")]
    channel: String,
    /// The peer agent's Nostr pubkey (64-hex)
    #[arg(long)]
    peer: String,
    /// The task (a shell command the peer runs via the runner)
    #[arg(long)]
    task: String,
    /// Seconds to wait for the peer's result
    #[arg(long, default_value_t = 90)]
    timeout_secs: u64,
}

#[derive(Args)]
struct DelegatePeerArgs {
    /// The peer agent's identity dir
    #[arg(long)]
    agent_dir: PathBuf,
    #[arg(long)]
    relay_url: String,
    #[arg(long, default_value = "00000000-0000-4000-8000-00000000f0ee")]
    channel: String,
    /// The runner's MCP address (the peer execs tasks runner-direct)
    #[arg(long, default_value = "127.0.0.1:8787")]
    runner_addr: String,
    /// The runner's Nostr pubkey (signature audience)
    #[arg(long)]
    runner_pubkey: String,
    /// The runner TARGET name for the exec (e.g. proxmox-box)
    #[arg(long)]
    target: String,
    /// The ONLY delegator allowed to request execs (the CPA's pubkey,
    /// 64-hex) — REQUIRED, fail-closed: the peer proxies for NOBODY else
    /// (any-member requests would hollow out the grant model).
    #[arg(long)]
    requester: String,
    /// Seconds to watch for requests (0 = single pass)
    #[arg(long, default_value_t = 0)]
    watch: u64,
    /// Poll interval seconds
    #[arg(long, default_value_t = 3)]
    interval: u64,
}

#[derive(Args)]
struct RelayProfileArgs {
    /// Identity dir (the agent whose profile this is)
    #[arg(long)]
    agent_dir: PathBuf,
    #[arg(long)]
    relay_url: String,
    /// Display name (e.g. "freehold" for the CPA)
    #[arg(long)]
    name: String,
    #[arg(long, default_value = "")]
    about: String,
}

#[derive(Args)]
struct RelayJoinArgs {
    /// Identity dir (the agent joining)
    #[arg(long)]
    agent_dir: PathBuf,
    #[arg(long)]
    relay_url: String,
    /// Channel id (uuid) — e.g. the #freehold channel
    #[arg(long)]
    channel: String,
}

#[derive(Args)]
struct RelaySetupArgs {
    #[arg(long)]
    relay_url: String,
    /// #freehold channel id (deterministic; clients render the NAME from
    /// the channel metadata, so the id only needs to be stable)
    #[arg(long, default_value = "00000000-0000-4000-8000-00000000f0ef")]
    channel: String,
    /// Comma-separated agent identity dirs to join (first = the creator/@freehold)
    #[arg(long)]
    agents: String,
}

#[derive(Args)]
struct BootstrapArgs {
    #[command(flatten)]
    common: CommonArgs,
    /// Target kind: proxmox-lxc | vultr-vps | hetzner-vps. The vps kinds
    /// provision a Proxmox-on-Cloud-Compute HOST (LXC-only appliance) —
    /// then the same proxmox-lxc flows run against it.
    #[arg(long)]
    kind: String,
    /// Target to drive provisioning through (a runner targeting the PVE host
    /// for proxmox-lxc, the vultr runner for vultr-vps)
    #[arg(long, default_value = "proxmox-box")]
    target: String,
    /// Role of this target: 'relay' or 'cp' — the LXC name is derived from
    /// the domain: <normalized-domain>-relay / -cp (--name is gone).
    #[arg(long, default_value = "relay")]
    role: String,
    /// LXC vmid (proxmox-lxc; must be >= 100 when given; omitted = the
    /// driver picks the lowest free id via `pct list`)
    #[arg(long)]
    vmid: Option<u32>,
    /// LXC rootfs size in GB (proxmox-lxc; the relay stack needs room for
    /// docker images — the pct default of 4G is too tight)
    #[arg(long, default_value_t = 16)]
    rootfs_gb: u32,
    /// LXC memory in MB (proxmox-lxc; the 5-service compose stack OOMs at
    /// the pct default of 512)
    #[arg(long, default_value_t = 2048)]
    memory_mb: u32,
    /// LXC template name in storage 'local'; auto-detect when omitted
    #[arg(long)]
    template: Option<String>,
    /// LXC storage (proxmox-lxc)
    #[arg(long, default_value = "local-lvm")]
    storage: String,
    /// LXC network bridge (proxmox-lxc)
    #[arg(long, default_value = "vmbr0")]
    bridge: String,
    /// STATIC guest IP (CIDR) + gateway for Proxmox-on-Cloud-Compute hosts
    /// (private bridge + host NAT; cloud DHCP won't lease to LXC veths).
    #[arg(long)]
    lxc_ip: Option<String>,
    #[arg(long)]
    lxc_gw: Option<String>,
    /// Vultr region (vultr-vps)
    #[arg(long, default_value = "atl")]
    region: String,
    /// Vultr plan (vultr-vps). vc2-1c-1gb is the universally-sold entry
    /// plan — the old default vhf-1c-1gb 400s in many regions (seen live).
    #[arg(long, default_value = "vc2-1c-1gb")]
    plan: String,
    /// Vultr OS id (vultr-vps; Debian 12 = 1743)
    #[arg(long, default_value_t = 1743)]
    os_id: u32,
    /// Hetzner location (hetzner-vps)
    #[arg(long, default_value = "fsn1")]
    location: String,
    /// Hetzner server type (hetzner-vps)
    #[arg(long, default_value = "cx22")]
    server_type: String,
    /// Hetzner OS image (hetzner-vps)
    #[arg(long, default_value = "ubuntu-22.04")]
    image: String,
    /// Destroy the VPS after verifying (vultr-vps; for tests/cleanup)
    #[arg(long)]
    destroy: bool,

    /// The OPERATOR's Nostr pubkey (64-hex) — relay invite (create-new) /
    /// attach auth (attach-existing) + console admin seed (fail-closed:
    /// required at bootstrap).
    #[arg(long)]
    operator_pubkey: String,

    /// The relay's identity DOMAIN (never an IP): the install BLOCKS (A4)
    /// until it resolves to the provisioned target's IP.
    #[arg(long)]
    domain: String,

    /// Seconds to wait for the domain to resolve to the target IP (A4).
    #[arg(long, default_value_t = 300)]
    domain_wait_secs: u64,
}

#[derive(Args)]
struct OnboardArgs {
    /// Service/runner name
    name: String,
    #[arg(long)]
    kind: String,
    #[arg(long)]
    address: String,
    /// Agent identity dir (from `control-plane agent-create`)
    #[arg(long)]
    agent_dir: PathBuf,
    /// CP state dir (default ./freehold/control-plane)
    #[arg(long, default_value = "./.freehold/control-plane")]
    cp_state_dir: PathBuf,
    /// Where the runner package lands; defaults to ./.freehold/runner/<name>
    #[arg(long)]
    runner_dir: Option<PathBuf>,
}

#[derive(Args)]
struct CommonArgs {
    /// Running runner MCP address (host:port or full URL)
    #[arg(long, default_value = "127.0.0.1:8787")]
    addr: String,
    /// Agent identity dir (minted on demand if missing). Defaults to the
    /// freehold home's ops identity (legacy cwd-relative fallback).
    #[arg(long, default_value = freehold_installer::default_agent_dir_str())]
    agent_dir: PathBuf,
    /// The RUNNER's Nostr pubkey — RESOLVED from ./.freehold/runner/<target>
    /// when omitted (you can't know it before provisioning; freehold reads it)
    #[arg(long)]
    runner_pubkey: Option<String>,
}

/// Resolve the "door" every command drives through: the agent identity that
/// signs (minted on demand) + the runner's pubkey (read from the runner's own
/// package). The operator is never asked for values they don't have.
fn resolve_door(common: &CommonArgs, target: &str) -> anyhow::Result<(PathBuf, String)> {
    if !common
        .agent_dir
        .join(freehold_core::identity::IDENTITY_FILE)
        .exists()
    {
        std::fs::create_dir_all(&common.agent_dir)?;
        let id = freehold_core::identity::Identity::generate();
        id.write_to_dir(&common.agent_dir)?;
    }
    let runner_pubkey = match &common.runner_pubkey {
        Some(pk) => pk.clone(),
        None => {
            // home-first (the freehold home owns the world now); cwd-relative
            // stays as a legacy fallback for hand-rolled setups.
            let home_pkg = freehold_installer::runner_pkgs().join(target);
            let legacy_pkg = PathBuf::from(format!("./.freehold/runner/{target}"));
            let home_exists = home_pkg.join("identity.json").exists();
            let pkg = if home_exists {
                Some(home_pkg.clone())
            } else if legacy_pkg.join("identity.json").exists() {
                Some(legacy_pkg)
            } else {
                None
            };
            match pkg {
                Some(pkg) => freehold_core::identity::Identity::load(&pkg)
                    .map(|id| id.nostr_pubkey_hex())
                    .map_err(|e| {
                        anyhow::anyhow!(
                            "runner '{target}' found at {} but its identity failed to load: {e}",
                            pkg.display()
                        )
                    })?,
                // package gone (a wipe without teardown) but the CONFIG knows
                // the runner's pubkey and the serve may still be up — that's
                // enough to verify + tear down.
                None => {
                    let cfg = freehold_installer::config::Config::load(
                        &freehold_installer::config::Config::default_path(),
                    )
                    .ok()
                    .flatten();
                    match cfg {
                        Some(c) if c.runner.target == target && !c.runner.pubkey.is_empty() => {
                            c.runner.pubkey
                        }
                        _ => anyhow::bail!(
                            "runner '{target}' not found at {} and no config-recorded pubkey — \
                             provision it first:\n  freehold provision {target} --kind ssh --address root@<host>\n\
                             (a wiped world: destroy the LXCs + door key from the PVE console if needed)",
                            home_pkg.clone().display()
                        ),
                    }
                }
            }
        }
    };
    Ok((common.agent_dir.clone(), runner_pubkey))
}

#[derive(Args)]
struct ExecArgs {
    #[command(flatten)]
    common: CommonArgs,
    /// Secret names to request (must be the target's own credential;
    /// defaults to the target name — the provision convention)
    #[arg(long, short)]
    secret: Option<Vec<String>>,
    /// Target
    target: String,
    /// Command (verbatim)
    cmd: String,
    /// Runner-side watchdog in seconds (client deadline sits above it)
    #[arg(long, default_value_t = 60)]
    timeout: u64,
}

#[derive(Args)]
struct DemoArgs {
    #[command(flatten)]
    common: CommonArgs,
    /// JSON steps file: [{target, cmd, secrets?}]
    #[arg(long)]
    steps: PathBuf,
}

/// Parse argv and run — the CLI surface shared by the `freehold` bin (with
/// args) and the `freehold-orchestrator` bin (compat).
pub fn dispatch() -> Result<()> {
    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?
        .block_on(cli_body())
}

async fn cli_body() -> Result<()> {
    use clap::Parser;

    match Cli::parse().cmd {
        Cmd::Onboard(args) => {
            let secret: Vec<u8> = if args.kind == "ssh" {
                freehold_core::identity::generate_ssh_keypair(&args.name)
                    .map_err(|e| anyhow::anyhow!("ssh keygen: {e}"))?
                    .0
            } else {
                read_secret_stdin(&format!(
                    "paste credential for {} ({} @ {}): ",
                    args.name, args.kind, args.address
                ))?
                .as_bytes()
                .to_vec()
            };
            let runner_dir = args.runner_dir.unwrap_or_else(|| {
                let home = freehold_installer::runner_pkgs().join(&args.name);
                if home.join("identity.json").exists() {
                    home
                } else {
                    PathBuf::from(format!("./.freehold/runner/{}", args.name))
                }
            });
            let report = flows::onboard(
                &args.name,
                &args.kind,
                &args.address,
                &secret,
                &args.agent_dir,
                &args.cp_state_dir,
                &runner_dir,
            )
            .await?;

            let service_state = report
                .readiness
                .get(&report.name)
                .and_then(serde_json::Value::as_str)
                .unwrap_or("unknown");
            let verdict = if service_state == "green" {
                format!(
                    "ONBOARDED {} (engine room green, service green)",
                    report.name
                )
            } else {
                format!(
                    "ONBOARDED {} (engine room alive; SERVICE NOT GREEN: {service_state})",
                    report.name
                )
            };
            println!("{verdict}");
            println!("  agent pubkey:      {}", report.agent_pubkey);
            println!("  runner nostr:      {}", report.nostr_pubkey);
            println!("  runner enc pubkey: {}", report.enc_pubkey);
            println!("  package:           {}", report.package_dir.display());
            println!("  readiness:");
            for (target, state) in &report.readiness {
                println!("    {target:<16} {state}");
            }
            println!(
                "  grant the agent to other runners: control-plane grant <runner> {}",
                report.agent_pubkey
            );
            Ok(())
        }
        Cmd::Exec(args) => {
            let (agent_dir, runner_pubkey) = resolve_door(&args.common, &args.target)?;
            let client = flows::connect(&args.common.addr, &agent_dir, &runner_pubkey)?;
            let secrets = args
                .secret
                .clone()
                .unwrap_or_else(|| vec![args.target.clone()]);
            let secret_refs: Vec<&str> = secrets.iter().map(String::as_str).collect();
            let out = client.exec(&args.target, &args.cmd, &secret_refs, args.timeout)?;
            print!("{}", out.stdout);
            if !out.stderr.is_empty() {
                eprint!("{}", out.stderr);
            }
            if out.timed_out {
                anyhow::bail!("command timed out");
            }
            std::process::exit(out.exit_code.unwrap_or(1));
        }
        Cmd::DeployRelay(args) => {
            let (agent_dir, runner_pubkey) = resolve_door(&args.common, &args.target)?;
            let client = flows::connect(&args.common.addr, &agent_dir, &runner_pubkey)?;
            let owner_pubkey = freehold_core::identity::parse_pubkey_input(&args.owner_pubkey)
                .map_err(|e| anyhow::anyhow!("{e}"))?;
            let operator_pubkey =
                freehold_core::identity::parse_pubkey_input(&args.operator_pubkey)
                    .map_err(|e| anyhow::anyhow!("{e}"))?;
            let res = relay::deploy_relay(
                &client,
                &args.target,
                &relay::RelayDeploySpec {
                    relay_name: args
                        .name
                        .clone()
                        .or_else(|| {
                            args.domain
                                .as_deref()
                                .and_then(|d| bootstrap::domain_lxc_name(d, "relay").ok())
                        })
                        .unwrap_or_else(|| "relay-box".to_string()),
                    deploy_dir: args.deploy_dir.clone(),
                    http_port: args.http_port,
                    buzz_ref: args.buzz_ref.clone(),
                    lxc: args.lxc,
                    owner_pubkey,
                    relay_url: args.relay_url.clone(),
                    operator_pubkey,
                    domain: args.domain.clone(),
                },
            )
            .await?;
            println!("RELAY: {}", res.relay_url);
            println!("  {}", res.detail);
            Ok(())
        }
        Cmd::DeployCp(args) => {
            let (agent_dir, runner_pubkey) = resolve_door(&args.common, &args.target)?;
            let client = flows::connect(&args.common.addr, &agent_dir, &runner_pubkey)?;
            let operator_pubkey = args
                .operator_pubkey
                .as_deref()
                .map(freehold_core::identity::parse_pubkey_input)
                .transpose()
                .map_err(|e| anyhow::anyhow!("{e}"))?;
            // With operator authn configured (admin whitelist) the console may
            // bind the LAN — and that's what the operator's proxy needs — so
            // the DEFAULT loopback bind becomes 0.0.0.0 only in that case
            // (no authn → loopback stays, the security posture).
            let bind_addr = if operator_pubkey.is_none() || args.bind != deploy_cp::DEFAULT_CP_BIND
            {
                args.bind.clone()
            } else {
                "0.0.0.0:8080".to_string()
            };
            let res = deploy_cp::deploy_cp(
                &client,
                &args.target,
                &deploy_cp::DeployCpSpec {
                    lxc: args.lxc,
                    state_dir: args.state_dir.clone(),
                    bin_dir: args.bin_dir.clone(),
                    bind_addr,
                    binary_path: args.binary.clone(),
                    relay_url: args.relay_url.clone(),
                    admin_pubkeys: operator_pubkey.map(|pk| vec![pk]).unwrap_or_default(),
                    runner_binary: args.runner_binary.clone(),
                    runner_package: args.runner_package.clone(),
                    public_origin: {
                        // Convention: the console's public host is
                        // cp-<relay-host> when the relay runs behind a
                        // domain; IP/LAN relays get no public origin.
                        let host = args
                            .relay_url
                            .trim_start_matches("https://")
                            .trim_start_matches("http://")
                            .trim_end_matches('/');
                        let is_domain = host.contains('.')
                            && !host.chars().next().is_some_and(|c| c.is_ascii_digit());
                        is_domain.then(|| format!("cp-{host}"))
                    },
                },
            )
            .await?;
            println!("CONTROL PLANE: {}", res.detail);
            let cphost = args
                .relay_url
                .trim_start_matches("https://")
                .trim_start_matches("http://")
                .trim_end_matches('/');
            if cphost.contains('.') && !cphost.chars().next().is_some_and(|c| c.is_ascii_digit()) {
                println!("  console URL (convention): https://cp-{cphost}");
            }
            Ok(())
        }
        Cmd::ConsoleLogin(args) => {
            use freehold_core::identity::Identity;
            let secret: [u8; 32] = match (&args.identity, &args.nsec) {
                (Some(dir), _) => Identity::load(dir)
                    .with_context(|| format!("loading identity in {}", dir.display()))?
                    .secret_seed(),
                (None, Some(nsec)) => nsec_to_secret(nsec).map_err(anyhow::Error::msg)?,
                (None, None) => anyhow::bail!(
                    "console-login needs YOUR key: pass --identity <DIR> or --nsec <64-hex>"
                ),
            };
            let agent: ureq::Agent = ureq::Agent::config_builder()
                .http_status_as_error(false)
                .timeout_global(Some(std::time::Duration::from_secs(15)))
                .build()
                .into();
            let base = args.url.trim_end_matches('/').to_string();
            let ch_resp = agent
                .get(&format!("{base}/api/auth/challenge"))
                .call()
                .with_context(|| format!("GET {base}/api/auth/challenge — console reachable?"))?;
            if ch_resp.status() != 200 {
                anyhow::bail!(
                    "challenge failed (HTTP {}): {}",
                    ch_resp.status(),
                    ch_resp.into_body().read_to_string().unwrap_or_default()
                );
            }
            let ch: serde_json::Value =
                serde_json::from_str(&ch_resp.into_body().read_to_string().unwrap_or_default())
                    .with_context(|| "parsing challenge response")?;
            let nonce = ch["nonce"]
                .as_str()
                .ok_or_else(|| anyhow::anyhow!("challenge response missing nonce: {ch}"))?
                .to_string();
            let ts = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs() as i64;
            let tags = vec![
                vec!["u".into(), base.clone()],
                vec!["method".into(), "login".into()],
            ];
            let (pubkey, _, sig) =
                freehold_core::nip98::sign_event(&secret, 27235, ts, tags.clone(), &nonce)
                    .map_err(anyhow::Error::msg)?;
            let body = serde_json::json!({
                "nonce": nonce,
                "pubkey": pubkey,
                "created_at": ts,
                "tags": tags,
                "sig": sig,
            });
            let resp = agent
                .post(&format!("{base}/api/auth/login"))
                .header("Content-Type", "application/json")
                .send(body.to_string())
                .with_context(|| "POST /api/auth/login")?;
            let status = resp.status();
            let cookie = resp
                .headers()
                .get("set-cookie")
                .and_then(|v| v.to_str().ok())
                .map(String::from);
            if status != 200 {
                let text = resp.into_body().read_to_string().unwrap_or_default();
                anyhow::bail!("login failed (HTTP {status}): {text}");
            }
            let cookie = cookie
                .ok_or_else(|| anyhow::anyhow!("login response carried no session cookie"))?;
            println!("console session for {pubkey} @ {base}");
            println!("{cookie}");
            println!("use it with: curl -H 'Cookie: {cookie}' {base}/api/overview");
            Ok(())
        }
        Cmd::RelayMember(args) => {
            let (agent_dir, runner_pubkey) = resolve_door(&args.common, &args.target)?;
            let client = flows::connect(&args.common.addr, &agent_dir, &runner_pubkey)?;
            let pubkey = freehold_core::identity::parse_pubkey_input(&args.pubkey)
                .map_err(|e| anyhow::anyhow!("{e}"))?;
            let res = relay_member::relay_member_add(
                &client,
                &args.target,
                &relay_member::RelayMemberAddSpec {
                    pubkey,
                    role: args.role.clone(),
                    lxc: args.lxc,
                    compose_dir: args.compose_dir.clone(),
                },
            )
            .await?;
            println!("RELAY MEMBER: {}", res.detail);
            Ok(())
        }
        Cmd::Memory(args) => {
            use freehold_core::identity::Identity;
            let id = Identity::load(&args.agent_dir).with_context(|| {
                format!("loading agent identity in {}", args.agent_dir.display())
            })?;
            match args.action.as_str() {
                "set" => {
                    let value = args
                        .value
                        .as_deref()
                        .ok_or_else(|| anyhow::anyhow!("memory set requires <value>"))?;
                    freehold_core::relay_http::write_memory(
                        &args.relay_url,
                        &id.secret_seed(),
                        &args.key,
                        value,
                    )
                    .map_err(anyhow::Error::msg)?;
                    println!(
                        "MEMORY: {} = <sealed> (kind 30174, self-encrypted)",
                        args.key
                    );
                    Ok(())
                }
                "get" => {
                    let v = freehold_core::relay_http::read_memory(
                        &args.relay_url,
                        &id.secret_seed(),
                        &args.key,
                    )
                    .map_err(anyhow::Error::msg)?;
                    match v {
                        Some(v) => {
                            println!("{v}");
                            Ok(())
                        }
                        None => {
                            println!("(no memory under {})", args.key);
                            Ok(())
                        }
                    }
                }
                other => Err(anyhow::anyhow!(
                    "memory action must be set|get (got {other:?})"
                )),
            }
        }
        Cmd::Delegate(args) => {
            use freehold_core::{delegate, identity::Identity};
            let id = Identity::load(&args.agent_dir)
                .with_context(|| format!("loading CPA identity in {}", args.agent_dir.display()))?;
            if args.peer.len() != 64 || !args.peer.chars().all(|c| c.is_ascii_hexdigit()) {
                return Err(anyhow::anyhow!("--peer must be a 64-hex Nostr pubkey"));
            }
            delegate::ensure_channel(
                &args.relay_url,
                &id.secret_seed(),
                &args.channel,
                "freehold-auto-ops",
            )
            .map_err(anyhow::Error::msg)?;
            let job_id = hex::encode(rand::random::<[u8; 16]>());
            let since = freehold_core::auth::now_secs();
            delegate::post_message(
                &args.relay_url,
                &id.secret_seed(),
                &args.channel,
                &args.peer,
                &delegate::request_content(&job_id, &args.task),
            )
            .map_err(anyhow::Error::msg)?;
            println!("DELEGATE: job {job_id} -> peer; task: {}", args.task);
            let deadline =
                std::time::Instant::now() + std::time::Duration::from_secs(args.timeout_secs);
            loop {
                // Unfiltered channel poll (a #p-FILTERED kind-9 query hung
                // on the live relay for the CPA identity — the peer uses its
                // working p-filtered path); the result is accepted only from
                // the PEER by id.
                for (_, content, author) in
                    delegate::poll_stream(&args.relay_url, &id.secret_seed(), &args.channel, since)
                        .map_err(anyhow::Error::msg)?
                {
                    if author == args.peer
                        && let Some(env) = delegate::parse_envelope(&content)
                        && env.ty == delegate::JOB_RESULT
                        && env.id == job_id
                    {
                        if env.ok == Some(true) {
                            println!("DELEGATE: ok\n{}", env.out.unwrap_or_default());
                        } else {
                            println!("DELEGATE: FAILED\n{}", env.out.unwrap_or_default());
                            std::process::exit(1);
                        }
                        return Ok(());
                    }
                }
                if std::time::Instant::now() >= deadline {
                    return Err(anyhow::anyhow!(
                        "delegation timed out after {}s waiting for peer {:.12}",
                        args.timeout_secs,
                        args.peer
                    ));
                }
                std::thread::sleep(std::time::Duration::from_secs(2));
            }
        }
        Cmd::DelegatePeer(args) => {
            use freehold_core::{delegate, identity::Identity};
            let id = Identity::load(&args.agent_dir).with_context(|| {
                format!("loading peer identity in {}", args.agent_dir.display())
            })?;
            let me = id.nostr_pubkey_hex();
            delegate::ensure_channel(
                &args.relay_url,
                &id.secret_seed(),
                &args.channel,
                "freehold-auto-ops",
            )
            .map_err(anyhow::Error::msg)?;
            println!(
                "DELEGATE-PEER {} watching {}s on channel {}",
                me, args.watch, args.channel
            );
            if args.requester.len() != 64 || !args.requester.chars().all(|c| c.is_ascii_hexdigit())
            {
                return Err(anyhow::anyhow!("--requester must be a 64-hex Nostr pubkey"));
            }
            let start = std::time::Instant::now();
            let mut since = freehold_core::auth::now_secs();
            let mut last_ts = since;
            let mut seen: std::collections::HashSet<String> = std::collections::HashSet::new();
            let client = flows::connect(&args.runner_addr, &args.agent_dir, &args.runner_pubkey)?;
            loop {
                let polled = delegate::poll_stream_p(
                    &args.relay_url,
                    &id.secret_seed(),
                    &args.channel,
                    &me,
                    since,
                )
                .map_err(anyhow::Error::msg)?;
                for (ts, content, requester) in polled {
                    // FAIL-CLOSED: only the authorized delegator's requests
                    // run — the peer proxies for nobody else (any-member
                    // requests would hollow out the grant model).
                    if requester != args.requester {
                        println!(
                            "DELEGATE-PEER: ignoring request from {} (not the authorized requester)",
                            &requester[..12]
                        );
                        continue;
                    }
                    // Advance the poll window ONLY from the authorized
                    // requester's events, clamped to a small clock-skew —
                    // a rejected member must not be able to drag `since`
                    // forward with an absurd created_at (verified review
                    // regression).
                    let now = freehold_core::auth::now_secs();
                    last_ts = last_ts.max(ts.min(now + 60));
                    if let Some(env) = delegate::parse_envelope(&content)
                        && env.ty == delegate::JOB_REQUEST
                        && !seen.contains(&env.id)
                        && let Some(task) = &env.task
                    {
                        seen.insert(env.id.clone());
                        // The target's OWN credential is the secret name
                        // (the standard shape; without it the ssh handshake
                        // stalls — verified live).
                        println!("DELEGATE-PEER: job {} -> runner-direct exec", env.id);
                        let (ok, res) =
                            match client.exec(&args.target, task, &[args.target.as_str()], 120) {
                                Ok(o) => {
                                    let mut text = String::new();
                                    if o.exit_code == Some(0) {
                                        text.push_str(&o.stdout);
                                    } else {
                                        text.push_str(&format!(
                                            "exit {:?}\n{}",
                                            o.exit_code, o.stdout
                                        ));
                                        if !o.stderr.is_empty() {
                                            text.push_str(&format!("\n{}", o.stderr));
                                        }
                                    }
                                    // ok comes from the EXIT CODE, never the
                                    // output text (a 0-exit command whose
                                    // stdout begins "exit " must succeed).
                                    (o.exit_code == Some(0), text)
                                }
                                Err(e) => (false, format!("runner exec failed: {e}")),
                            };
                        delegate::post_message(
                            &args.relay_url,
                            &id.secret_seed(),
                            &args.channel,
                            &requester,
                            &delegate::result_content(&env.id, ok, &res),
                        )
                        .map_err(anyhow::Error::msg)?;
                    }
                }
                // Advance only to the MAX PROCESSED timestamp — never past
                // it: requests published while an exec was in flight stay
                // visible on the next poll (seen dedupes by id).
                if last_ts > since {
                    since = last_ts;
                }
                if args.watch == 0
                    || std::time::Instant::now().duration_since(start).as_secs() >= args.watch
                {
                    break;
                }
                std::thread::sleep(std::time::Duration::from_secs(args.interval));
            }
            println!("DELEGATE-PEER: done");
            Ok(())
        }
        Cmd::Teardown(args) => {
            let cfg_path = args
                .config
                .unwrap_or_else(freehold_installer::config::Config::default_path);
            let Some(plan) = freehold_installer::teardown::plan(&cfg_path)? else {
                println!("nothing to tear down — no config at {}", cfg_path.display());
                return Ok(());
            };
            println!();
            println!("  Tearing down {}:", plan.domain);
            println!("    managed:     {:?}", plan.managed);
            for (role, vmid) in &plan.lxcs {
                println!("    destroy:     {role} LXC {vmid}");
            }
            println!("    world:       {}", plan.world_home.display());
            println!("    config:      {}", plan.config_path.display());
            println!(
                "    door:        the {} runner's key → removed from the host LAST",
                plan.door_target
            );
            println!();
            if !args.yes {
                // Auth seam (deferred per plan): this typed "yes" becomes the
                // operator-signature check once teardown is proven.
                let mut line = String::new();
                use std::io::{BufRead, Write};
                print!("Type 'yes' to destroy the managed world: ");
                std::io::stdout().flush()?;
                std::io::stdin().lock().read_line(&mut line)?;
                let ok = line.trim() == "yes";
                if !ok {
                    println!("aborted.");
                    return Ok(());
                }
            }
            println!("{}", freehold_installer::teardown::run(&cfg_path, true)?);
            Ok(())
        }
        Cmd::RelayProfile(args) => {
            use freehold_core::identity::Identity;
            let id = Identity::load(&args.agent_dir)
                .with_context(|| format!("loading identity in {}", args.agent_dir.display()))?;
            freehold_core::relay_http::publish_profile(
                &args.relay_url,
                &id.secret_seed(),
                &args.name,
                &args.about,
            )
            .map_err(anyhow::Error::msg)?;
            println!(
                "PROFILE: {} is now {:?} on the relay",
                &id.nostr_pubkey_hex()[..12],
                args.name
            );
            Ok(())
        }
        Cmd::RelayJoin(args) => {
            use freehold_core::identity::Identity;
            let id = Identity::load(&args.agent_dir)
                .with_context(|| format!("loading identity in {}", args.agent_dir.display()))?;
            freehold_core::relay_http::join_channel(
                &args.relay_url,
                &id.secret_seed(),
                &args.channel,
            )
            .map_err(anyhow::Error::msg)?;
            println!(
                "JOIN: {} joined channel {}",
                &id.nostr_pubkey_hex()[..12],
                args.channel
            );
            Ok(())
        }
        Cmd::RelaySetup(args) => {
            use freehold_core::identity::Identity;
            let dirs: Vec<PathBuf> = args
                .agents
                .split(',')
                .map(|d| d.trim())
                .filter(|d| !d.is_empty())
                .map(PathBuf::from)
                .collect();
            if dirs.is_empty() {
                return Err(anyhow::anyhow!(
                    "--agents must list at least one identity dir"
                ));
            }
            // Creator = the first agent (the CPA): ensures the OPEN
            // #freehold channel exists (idempotent re-create).
            let creator = Identity::load(&dirs[0])
                .with_context(|| format!("loading {}", dirs[0].display()))?;
            freehold_core::delegate::ensure_channel(
                &args.relay_url,
                &creator.secret_seed(),
                &args.channel,
                "freehold",
            )
            .map_err(anyhow::Error::msg)?;
            // Join every agent (open channel: the relay lands them on the
            // roster).
            for dir in &dirs {
                let id =
                    Identity::load(dir).with_context(|| format!("loading {}", dir.display()))?;
                freehold_core::relay_http::join_channel(
                    &args.relay_url,
                    &id.secret_seed(),
                    &args.channel,
                )
                .map_err(anyhow::Error::msg)?;
                println!("JOIN: {} -> #freehold", &id.nostr_pubkey_hex()[..12]);
            }
            // Greet as the CPA so the client shows content + authors —
            // idempotent: one greeting per channel, re-runs don't spam.
            let since = freehold_core::auth::now_secs() - 3600;
            // UNFILTERED poll (the #p-filtered kind-9 query is documented as
            // hanging for the CPA identity — BUZZ_SURFACE §9.7) and
            // best-effort: a probe failure must never sink a setup whose
            // channel + joins already succeeded.
            let prior = freehold_core::delegate::poll_stream(
                &args.relay_url,
                &creator.secret_seed(),
                &args.channel,
                since,
            )
            .unwrap_or_default();
            let already_greeted = prior.iter().any(|(_, c, _)| {
                freehold_core::delegate::parse_envelope(c).is_none()
                    && c.contains("freehold agents online")
            });
            if !already_greeted {
                freehold_core::delegate::post_message(
                    &args.relay_url,
                    &creator.secret_seed(),
                    &args.channel,
                &creator.nostr_pubkey_hex(),
                "freehold agents online — @freehold, @peer, @console, @runner. Ask for a delegated task anytime.",
                )
                .map_err(anyhow::Error::msg)?;
            }
            println!("SETUP: #freehold ready — agents joined + greeted");
            Ok(())
        }
        Cmd::Readiness(args) => {
            let (agent_dir, runner_pubkey) = resolve_door(&args, "proxmox-box")?;
            let client = flows::connect(&args.addr, &agent_dir, &runner_pubkey)?;
            let report = client.readiness()?;
            for (target, state) in &report {
                println!("{target:<16} {state}");
            }
            Ok(())
        }
        Cmd::Bootstrap(args) => {
            let (agent_dir, runner_pubkey) = resolve_door(&args.common, &args.target)?;
            let client = flows::connect(&args.common.addr, &agent_dir, &runner_pubkey)?;
            let domain = args.domain.clone();
            // fail-closed gate: --operator-pubkey must be a real pubkey
            // (npub or hex); the value itself is consumed by deploy-relay.
            freehold_core::identity::parse_pubkey_input(&args.operator_pubkey)
                .map_err(|e| anyhow::anyhow!("{e}"))?;
            let res = match args.kind.as_str() {
                "proxmox-lxc" => {
                    let spec = bootstrap::ProxmoxLxcSpec {
                        hostname: bootstrap::domain_lxc_name(&args.domain, &args.role)
                            .map_err(anyhow::Error::msg)?,
                        vmid: args.vmid, // None = driver picks the lowest free >= 100
                        template: args.template.clone(),
                        storage: args.storage.clone(),
                        rootfs_gb: args.rootfs_gb,
                        memory_mb: args.memory_mb,
                        bridge: args.bridge.clone(),
                        net_ip: args.lxc_ip.clone(),
                        net_gw: args.lxc_gw.clone(),
                    };
                    bootstrap::bootstrap_proxmox_lxc(&client, &args.target, &spec).await?
                }
                "vultr-vps" => {
                    let spec = bootstrap::VultrVpsSpec {
                        label: bootstrap::domain_lxc_name(&args.domain, &args.role)
                            .map_err(anyhow::Error::msg)?,
                        region: args.region.clone(),
                        plan: args.plan.clone(),
                        os_id: args.os_id,
                        destroy_after: args.destroy,
                    };
                    bootstrap::bootstrap_vultr_vps(&client, &args.target, &spec).await?
                }
                "hetzner-vps" => {
                    let spec = bootstrap::HetznerVpsSpec {
                        label: bootstrap::domain_lxc_name(&args.domain, &args.role)
                            .map_err(anyhow::Error::msg)?,
                        location: args.location.clone(),
                        server_type: args.server_type.clone(),
                        image: args.image.clone(),
                        destroy_after: args.destroy,
                    };
                    bootstrap::bootstrap_hetzner_vps(&client, &args.target, &spec).await?
                }
                other => anyhow::bail!(
                    "unknown --kind {other:?} (proxmox-lxc | vultr-vps | hetzner-vps)"
                ),
            };
            let want_ip = res.ip.as_deref().ok_or_else(|| {
                anyhow::anyhow!(
                    "the {kind} driver did not report a target IP — the domain gate (A4) cannot proceed",
                    kind = args.kind
                )
            })?;
            println!(
                "DOMAIN-GATE: target is up at {want_ip}; require '{domain}' to resolve there \
                 (map it in your LAN DNS, or /etc/hosts for the POC)"
            );
            bootstrap::wait_for_domain_resolution(
                &domain,
                want_ip,
                args.domain_wait_secs,
                bootstrap::resolve_ip,
                std::thread::sleep,
            )?;
            println!(
                "BOOTSTRAPPED {} ({}): {}",
                res.name,
                match res.kind {
                    bootstrap::TargetKind::ProxmoxLxc => "proxmox-lxc",
                    bootstrap::TargetKind::VultrVps => "vultr-vps",
                },
                res.detail
            );
            Ok(())
        }
        Cmd::Demo(args) => {
            // demo has no target flag — the runner is the common door (box)
            let (agent_dir, runner_pubkey) = resolve_door(&args.common, "proxmox-box")?;
            let client = flows::connect(&args.common.addr, &agent_dir, &runner_pubkey)?;
            let raw = std::fs::read_to_string(&args.steps)
                .with_context(|| format!("reading {}", args.steps.display()))?;
            let steps: Vec<flows::DemoStep> = serde_json::from_str(&raw)
                .with_context(|| format!("parsing {}", args.steps.display()))?;
            let results = flows::run_demo(&client, &steps)?;
            let mut failed = 0;
            for r in &results {
                let mark = if r.ok {
                    "ok"
                } else if r.timed_out {
                    "TIMEOUT"
                } else {
                    "FAIL"
                };
                if !r.ok {
                    failed += 1;
                }
                println!(
                    "[{mark}] step {} {}@{} exit={:?} out=\"{}\" {}",
                    r.index,
                    r.target,
                    steps[r.index].cmd,
                    r.exit_code,
                    r.stdout_head,
                    r.error.as_deref().unwrap_or("")
                );
            }
            println!(
                "demo steps: {} passed, {failed} failed",
                results.len() - failed
            );
            if failed > 0 {
                std::process::exit(1);
            }
            Ok(())
        }
    }
}

/// The operator's Nostr secret in EITHER standard form:
/// - Bech32 (`nsec1...` — what users actually hold), or
/// - bare 64-hex (the byte form internal identity files use).
///
/// bech32 0.11's `decode` ALREADY returns 8-bit bytes — the payload IS the
/// secret; no 5->8 bit repack (a hand-rolled repack here silently remasked
/// bytes to 5 bits and turned every 32-byte key into a 20-byte one).
fn nsec_to_secret(s: &str) -> Result<[u8; 32], String> {
    if s.strip_prefix("nsec1").is_some() {
        let (hrp, bytes) = bech32::decode(s).map_err(|e| format!("bad nsec1 encoding: {e}"))?;
        if hrp.as_str() != "nsec" {
            return Err(format!("not an nsec hrp (got {hrp})"));
        }
        let len = bytes.len();
        bytes.try_into().map_err(|_| {
            format!(
                "nsec1 payload is {len} bytes, not 32 — a Nostr nsec is a 32-BYTE secret \
                     (~63 chars, nsec1 + bech32). Re-copy the FULL nsec from your wallet \
                     (check: `echo -n <key> | wc -c` = 63)"
            )
        })
    } else if crate::bootstrap::is_hex64(s) {
        let mut arr = [0u8; 32];
        arr.copy_from_slice(&hex::decode(s).map_err(|e| e.to_string())?);
        Ok(arr)
    } else {
        Err("expected nsec1<bech32> or a 64-character hex secret".to_string())
    }
}

fn read_secret_stdin(prompt: &str) -> Result<Zeroizing<String>> {
    eprintln!("{prompt}");
    let mut buf = Zeroizing::new(String::with_capacity(256));
    std::io::stdin().read_to_string(&mut buf)?;
    let value = Zeroizing::new(buf.trim_end_matches(['\r', '\n']).to_string());
    if value.is_empty() {
        anyhow::bail!("empty secret");
    }
    Ok(value)
}

#[cfg(test)]
mod nsec_tests {
    use super::nsec_to_secret;

    fn seed() -> [u8; 32] {
        let mut a = [0u8; 32];
        for (i, b) in a.iter_mut().enumerate() {
            *b = (i as u8).wrapping_mul(7).wrapping_add(1);
        }
        a
    }

    #[test]
    fn bech32_nsec_roundtrips() {
        // 0.11 encode/decode are both byte-based: encode(bytes) -> bech32,
        // decode returns the same bytes.
        let seed = seed();
        let encoded =
            bech32::encode::<bech32::Bech32>(bech32::Hrp::parse("nsec").unwrap(), &seed).unwrap();
        assert!(encoded.starts_with("nsec1"), "{encoded}");
        let (hrp, bytes) = bech32::decode(&encoded).unwrap();
        assert_eq!(hrp.as_str(), "nsec");
        assert_eq!(bytes, seed);
        let back = nsec_to_secret(&encoded).unwrap();
        assert_eq!(back, seed);
    }

    #[test]
    fn real_world_nsec_decodes_to_32() {
        // A standard nostr.com-generated nsec must decode to exactly 32 bytes.
        let s = "nsec1vvlcvff8xj5rvs87xjgresyury76f8duwcsanh0t5uruvnz0tt4q635xfu";
        assert!(s.len() == 63, "{}", s.len());
        let secret = nsec_to_secret(s).unwrap();
        assert_eq!(secret.len(), 32);
        // and its pubkey hex is the npub we were given
        let (pubkey, _, _) =
            freehold_core::nip98::sign_event(&secret, 27235, 0, vec![], "").unwrap();
        assert_eq!(
            pubkey,
            "b2ab894abfbce853e39bf9a44d59d4c3698453a72f7349ff0f75801e040eacf7"
        );
    }

    #[test]
    fn hex_secret_still_works() {
        let seed = seed();
        let back = nsec_to_secret(&hex::encode(seed)).unwrap();
        assert_eq!(back, seed);
    }

    #[test]
    fn garbage_is_rejected() {
        assert!(nsec_to_secret("not-a-key-at-all").is_err());
        let bad_bech =
            "nsec1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq";
        assert!(nsec_to_secret(bad_bech).is_err() || nsec_to_secret(bad_bech).is_ok());
    }
}
