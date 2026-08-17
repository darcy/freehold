//! Freehold orchestrator — the scripted CPA stand-in (Phase E).

use std::io::Read;
use std::path::PathBuf;

use anyhow::{Context, Result};
use clap::{Args, Parser, Subcommand};
use freehold_orchestrator::{bootstrap, flows};
use zeroize::Zeroizing;

#[derive(Parser)]
#[command(
    name = "orchestrator",
    version,
    about = "Freehold orchestrator: scripted CPA stand-in that drives the engine room"
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
    /// LXC vmid (proxmox-lxc; must be >= 100)
    #[arg(long)]
    vmid: Option<u32>,
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
                    let vmid = args
                        .vmid
                        .ok_or_else(|| anyhow::anyhow!("--vmid is required for proxmox-lxc"))?;
                    let spec = bootstrap::ProxmoxLxcSpec {
                        hostname: args.name.clone(),
                        vmid,
                        template: args.template.clone(),
                        storage: args.storage.clone(),
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
