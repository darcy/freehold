//! Freehold orchestrator — the scripted CPA stand-in (Phase E).

use std::io::Read;
use std::path::PathBuf;

use anyhow::{Context, Result};
use clap::{Args, Parser, Subcommand};
use freehold_orchestrator::{bootstrap, deploy_cp, flows, relay, relay_member};
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
    /// C2: add a relay member through the relay-admin runner (buzz-admin)
    RelayMember(RelayMemberArgs),
    /// D3: the agent's encrypted relay memory (kind 30174, self-sealed)
    Memory(MemoryArgs),
    /// E: the CPA delegates a task to a relay-addressable peer agent
    Delegate(DelegateArgs),
    /// E: the peer agent — watches for delegated requests, execs them via
    /// the runner (runner-direct), posts the results
    DelegatePeer(DelegatePeerArgs),
}

#[derive(Args)]
struct DeployRelayArgs {
    #[command(flatten)]
    common: CommonArgs,
    /// Target to deploy through (the runner holding the SSH credential to
    /// the PVE host / relay LXC)
    #[arg(long, default_value = "proxmox-box")]
    target: String,
    /// Relay hostname (reported; the operator maps it to the box)
    #[arg(long, default_value = "relay-box")]
    name: String,
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
struct BootstrapArgs {
    #[command(flatten)]
    common: CommonArgs,
    /// Target kind: proxmox-lxc | vultr-vps
    #[arg(long)]
    kind: String,
    /// Target to drive provisioning through (a runner targeting the PVE host
    /// for proxmox-lxc, the vultr runner for vultr-vps)
    #[arg(long, default_value = "proxmox-box")]
    target: String,
    /// LXC hostname (proxmox-lxc) / instance label (vultr-vps)
    #[arg(long)]
    name: String,
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
    /// Vultr region (vultr-vps)
    #[arg(long, default_value = "atl")]
    region: String,
    /// Vultr plan (vultr-vps)
    #[arg(long, default_value = "vhf-1c-1gb")]
    plan: String,
    /// Vultr OS id (vultr-vps; Debian 12 = 1743)
    #[arg(long, default_value_t = 1743)]
    os_id: u32,
    /// Destroy the VPS after verifying (vultr-vps; for tests/cleanup)
    #[arg(long)]
    destroy: bool,
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
    /// Agent identity dir (from `control-plane agent-create`)
    #[arg(long)]
    agent_dir: PathBuf,
    /// The RUNNER's Nostr pubkey (signature audience)
    #[arg(long)]
    runner_pubkey: String,
}

#[derive(Args)]
struct ExecArgs {
    #[command(flatten)]
    common: CommonArgs,
    /// Secret names to request (must be the target's own credential)
    #[arg(long, short)]
    secret: Vec<String>,
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

#[tokio::main]
async fn main() -> Result<()> {
    match Cli::parse().cmd {
        Cmd::Onboard(args) => {
            let secret = read_secret_stdin(&format!(
                "paste credential for {} ({} @ {}): ",
                args.name, args.kind, args.address
            ))?;
            let runner_dir = args
                .runner_dir
                .unwrap_or_else(|| PathBuf::from(format!("./.freehold/runner/{}", args.name)));
            let report = flows::onboard(
                &args.name,
                &args.kind,
                &args.address,
                secret.as_bytes(),
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
            let client = flows::connect(
                &args.common.addr,
                &args.common.agent_dir,
                &args.common.runner_pubkey,
            )?;
            let secret_refs: Vec<&str> = args.secret.iter().map(String::as_str).collect();
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
            let client = flows::connect(
                &args.common.addr,
                &args.common.agent_dir,
                &args.common.runner_pubkey,
            )?;
            let res = relay::deploy_relay(
                &client,
                &args.target,
                &relay::RelayDeploySpec {
                    relay_name: args.name.clone(),
                    deploy_dir: args.deploy_dir.clone(),
                    http_port: args.http_port,
                    buzz_ref: args.buzz_ref.clone(),
                    lxc: args.lxc,
                    owner_pubkey: args.owner_pubkey.clone(),
                    relay_url: args.relay_url.clone(),
                },
            )
            .await?;
            println!("RELAY: {}", res.relay_url);
            println!("  {}", res.detail);
            Ok(())
        }
        Cmd::DeployCp(args) => {
            let client = flows::connect(
                &args.common.addr,
                &args.common.agent_dir,
                &args.common.runner_pubkey,
            )?;
            let res = deploy_cp::deploy_cp(
                &client,
                &args.target,
                &deploy_cp::DeployCpSpec {
                    state_dir: args.state_dir.clone(),
                    bin_dir: args.bin_dir.clone(),
                    bind_addr: args.bind.clone(),
                    binary_path: args.binary.clone(),
                    relay_url: args.relay_url.clone(),
                },
            )
            .await?;
            println!("CONTROL PLANE: {}", res.detail);
            Ok(())
        }
        Cmd::RelayMember(args) => {
            let client = flows::connect(
                &args.common.addr,
                &args.common.agent_dir,
                &args.common.runner_pubkey,
            )?;
            let res = relay_member::relay_member_add(
                &client,
                &args.target,
                &relay_member::RelayMemberAddSpec {
                    pubkey: args.pubkey.clone(),
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
                    last_ts = last_ts.max(ts);
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
        Cmd::Readiness(args) => {
            let client = flows::connect(&args.addr, &args.agent_dir, &args.runner_pubkey)?;
            let report = client.readiness()?;
            for (target, state) in &report {
                println!("{target:<16} {state}");
            }
            Ok(())
        }
        Cmd::Bootstrap(args) => {
            let client = flows::connect(
                &args.common.addr,
                &args.common.agent_dir,
                &args.common.runner_pubkey,
            )?;
            let res = match args.kind.as_str() {
                "proxmox-lxc" => {
                    let spec = bootstrap::ProxmoxLxcSpec {
                        hostname: args.name.clone(),
                        vmid: args.vmid, // None = driver picks the lowest free >= 100
                        template: args.template.clone(),
                        storage: args.storage.clone(),
                        rootfs_gb: args.rootfs_gb,
                        memory_mb: args.memory_mb,
                        bridge: args.bridge.clone(),
                    };
                    bootstrap::bootstrap_proxmox_lxc(&client, &args.target, &spec).await?
                }
                "vultr-vps" => {
                    let spec = bootstrap::VultrVpsSpec {
                        label: args.name.clone(),
                        region: args.region.clone(),
                        plan: args.plan.clone(),
                        os_id: args.os_id,
                        destroy_after: args.destroy,
                    };
                    bootstrap::bootstrap_vultr_vps(&client, &args.target, &spec).await?
                }
                other => anyhow::bail!("unknown --kind {other:?} (proxmox-lxc | vultr-vps)"),
            };
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
            let client = flows::connect(
                &args.common.addr,
                &args.common.agent_dir,
                &args.common.runner_pubkey,
            )?;
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
