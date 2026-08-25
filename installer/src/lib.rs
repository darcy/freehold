//! freehold-installer core — the bring-up STAGES as a library.
//!
//! Both front-ends drive the same choreography:
//!   - the standalone script (`freehold-install`, src/main.rs — dialoguer)
//!   - the `freehold` TUI bootstrap mode (crate `tui`)
//!
//! The stages compose the REAL sibling binaries (`control-plane`, `runner`,
//! `freehold-orchestrator`) exactly as a human would by hand — no duplicated
//! logic, no parallel codepaths. Everything here is non-interactive: prompts
//! and waiting belong to the front-ends.

use anyhow::{Context, Result, bail};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{Duration, Instant};

// World paths are anchored to the REPO ROOT (never cwd-relative): the TUI,
// the installer, and tests run from different working directories, and a
// relative path silently mints a fresh identity instead of reusing the grant.
pub fn freehold_home() -> std::path::PathBuf {
    if let Some(h) = std::env::var_os("FREEHOLD_HOME") {
        return PathBuf::from(h);
    }
    PathBuf::from(std::env::var_os("HOME").unwrap_or_else(|| "/root".into())).join(".freehold")
}
pub fn state_dir() -> std::path::PathBuf {
    freehold_home().join("control-plane")
}
pub fn ops_dir() -> std::path::PathBuf {
    freehold_home().join("control-plane").join("agent-ops")
}
pub fn runner_pkgs() -> std::path::PathBuf {
    freehold_home().join("runner")
}
pub fn serve_log() -> std::path::PathBuf {
    freehold_home().join("installer").join("serve.log")
}
/// Where a minted operator identity lands (the operator keeps this dir).
pub fn operator_dir() -> std::path::PathBuf {
    freehold_home().join("control-plane").join("operator")
}

/// The RUNNER's own Nostr pubkey, read from its package identity (public —
/// this is the key agents SIGN TO, and the trust anchor for exec). Empty
/// string when the package doesn't exist yet (pre-provision).
pub fn resolve_runner_pubkey(name: &str) -> String {
    let pkg = runner_pkgs().join(name);
    freehold_core::identity::Identity::load(&pkg)
        .map(|id| id.nostr_pubkey_hex())
        .unwrap_or_default()
}

pub mod config;
pub mod teardown;

/// Everything a bring-up needs to know about the world, once collected.
#[derive(Debug, Clone)]
pub struct Answers {
    /// Proxmox host the runner SSH's into (root@192.168.30.224)
    pub host: String,
    /// Runner name (the package + grant name)
    pub runner: String,
    /// Runner MCP address (loopback)
    pub serve: String,
    /// Relay identity domain (A4 gate)
    pub domain: String,
    /// Auto-picked by the driver when None (stored in the config after boot).
    pub relay_vmid: Option<u32>,
    pub relay_ip: Option<String>,
    pub cp_vmid: Option<u32>,
    pub cp_ip: Option<String>,
    pub rootfs_gb: u32,
    pub memory_mb: u32,
    /// Operator Nostr pubkey (64-hex)
    pub operator_pk: String,
    /// True when we minted the operator identity (vs. the user's own key)
    pub operator_generated: bool,
    /// The operator's identity dir (when generated/pointed at one)
    pub operator_dir: PathBuf,
}

impl Answers {
    /// Rebuild from a saved config — what the configure pipeline runs on.
    pub fn from_config(cfg: &config::Config) -> Answers {
        Answers {
            host: String::new(), // only used by fresh provisioning
            runner: cfg.runner.target.clone(),
            serve: cfg.runner.addr.clone(),
            domain: cfg.domain.clone(),
            relay_vmid: cfg.lxc.relay.vmid,
            relay_ip: cfg.lxc.relay.ip.clone(),
            cp_vmid: cfg.lxc.cp.vmid,
            cp_ip: cfg.lxc.cp.ip.clone(),
            rootfs_gb: 16, // bootstrap-time only
            memory_mb: 2048,
            operator_pk: cfg.operator_pubkey.clone(),
            operator_generated: cfg.operator_identity.is_some(),
            operator_dir: cfg.operator_identity.clone().unwrap_or_default(),
        }
    }

    /// The sane defaults — the user overrides what differs.
    pub fn defaults() -> Self {
        Self {
            host: "root@192.168.30.224".into(),
            runner: "proxmox-box".into(),
            serve: "127.0.0.1:8787".into(),
            domain: "freehold-test.darcydev.net".into(),
            relay_vmid: None,
            relay_ip: None,
            cp_vmid: None,
            cp_ip: None,
            rootfs_gb: 16,
            memory_mb: 2048,
            operator_pk: String::new(),
            operator_generated: false,
            operator_dir: PathBuf::new(),
        }
    }
}

pub fn repo_root() -> &'static Path {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .expect("installer under repo root")
}

pub fn bin(name: &str) -> PathBuf {
    repo_root().join("target").join("debug").join(name)
}

/// Release build — the deploy-cp stage ships these to the box as base64;
/// debug binaries are ~15x larger and turn the ship into a 10-minute stall.
pub fn rel_bin(name: &str) -> PathBuf {
    repo_root().join("target").join("release").join(name)
}

/// The stages drive the REAL sibling binaries. We cannot spawn `cargo` from
/// under `cargo run` (target-dir lock), so the bins must exist; give the
/// exact one-liner when they don't.
pub fn ensure_bins() -> Result<()> {
    let debug_bins = ["control-plane", "runner", "freehold-orchestrator"];
    let release_bins = ["control-plane", "runner"];
    let missing: Vec<_> = debug_bins
        .iter()
        .filter(|b| !bin(b).exists())
        .map(|b| format!("target/debug/{b}"))
        .chain(
            release_bins
                .iter()
                .filter(|b| !rel_bin(b).exists())
                .map(|b| format!("target/release/{b}")),
        )
        .collect();
    if missing.is_empty() {
        return Ok(());
    }
    bail!(
        "sibling binaries missing: {}\n  build them once, then run the installer directly:\n    cargo build --workspace --bins && cargo build --release --bin control-plane --bin runner\n    ./target/debug/freehold-install",
        missing.join(", ")
    );
}

/// Run a sibling binary, capturing stdout+stderr. Returns (ok, output).
pub fn run(bin_path: &Path, args: &[&str]) -> Result<(bool, String)> {
    let out = Command::new(bin_path)
        .args(args)
        .output()
        .with_context(|| format!("failed to spawn {}", bin_path.display()))?;
    let mut text = String::from_utf8_lossy(&out.stdout).into_owned();
    text.push_str(&String::from_utf8_lossy(&out.stderr));
    Ok((out.status.success(), text))
}

pub fn print_tail(text: &str, n: usize) {
    let lines: Vec<&str> = text.lines().filter(|l| !l.trim().is_empty()).collect();
    for l in lines.iter().rev().take(n).rev() {
        eprintln!("    {l}");
    }
}

pub fn mint_identity(dir: &Path) -> Result<freehold_core::identity::Identity> {
    std::fs::create_dir_all(dir)?;
    if dir.join("identity.json").exists() {
        return freehold_core::identity::Identity::load(dir).map_err(Into::into);
    }
    let (ok, out) = run(
        &bin("runner"),
        &["keys", "init", "--state-dir", dir.to_str().unwrap()],
    )?;
    if !ok {
        bail!("keys init failed:\n{out}");
    }
    freehold_core::identity::Identity::load(dir).map_err(Into::into)
}

pub fn ensure_ops_agent() -> Result<freehold_core::identity::Identity> {
    let dir = ops_dir();
    if !dir.join("identity.json").exists() {
        std::fs::create_dir_all(&dir)?;
        let (ok, out) = run(
            &bin("runner"),
            &["keys", "init", "--state-dir", dir.to_str().unwrap()],
        )?;
        if !ok {
            bail!("ops-agent keys init failed:\n{out}");
        }
    }
    freehold_core::identity::Identity::load(&dir).map_err(Into::into)
}

/// The ops agent's pubkey (the identity the orchestrator drives with).
pub fn ops_pubkey() -> Result<String> {
    Ok(ensure_ops_agent()?.nostr_pubkey_hex())
}

/// Stage 1 — provision the SSH door into the PVE host. A fresh runner gets
/// its keypair generated in-process; an existing one is reused (the public
/// key was already printed at ITS provisioning; the door is verified live in
/// stage 3 either way).
pub fn stage_provision(a: &Answers, agent_pk: &str) -> Result<Option<String>> {
    let (ok, out) = run(
        &bin("control-plane"),
        &[
            "provision",
            &a.runner,
            "--kind",
            "ssh",
            "--address",
            &a.host,
            "--state-dir",
            state_dir().to_str().unwrap(),
            "--grant",
            agent_pk,
        ],
    )?;
    if ok {
        // The public key line the operator must install on the target.
        let pubkey = out
            .lines()
            .find(|l| l.trim_start().starts_with("ssh-ed25519"))
            .map(|l| l.trim().to_string());
        Ok(pubkey)
    } else if out.contains("already exists")
        || out.contains("RunnerExists")
        || out.contains("PackageDirInUse")
    {
        Ok(None)
    } else {
        bail!("provision failed:\n{out}");
    }
}

/// Re-grant the ops agent (belt + suspenders for a reused package).
pub fn stage_grant(a: &Answers) -> Result<()> {
    let (ok, out) = run(
        &bin("control-plane"),
        &[
            "grant",
            &a.runner,
            "--state-dir",
            state_dir().to_str().unwrap(),
        ],
    )?;
    if !ok {
        bail!("grant failed:\n{out}");
    }
    Ok(())
}

/// Stage 2 — the runner serves in the background (detached; survives this
/// process). Reuses an already-listening serve on the same address.
/// Returns the pid (or "— (reused)").
pub fn stage_serve(a: &Answers) -> Result<String> {
    if port_open(&a.serve) {
        return Ok("— (reused)".to_string());
    }
    std::fs::create_dir_all(serve_log().parent().unwrap())?;
    let pkg = runner_pkgs().join(&a.runner);
    if !pkg.join("identity.json").exists() {
        bail!(
            "runner package {} is missing — the provision stage created it, something is off",
            pkg.display()
        );
    }
    // Detached + nohup'd: survives the installer exiting. Log in ./.freehold.
    let sh = format!(
        "nohup '{}' serve --state-dir {} --addr {} > {} 2>&1 & echo $!",
        bin("runner").display(),
        pkg.display(),
        a.serve,
        serve_log().display()
    );
    let out = Command::new("sh")
        .arg("-c")
        .arg(&sh)
        .output()
        .with_context(|| "spawn detached serve")?;
    let pid = String::from_utf8_lossy(&out.stdout).trim().to_string();

    let deadline = Instant::now() + Duration::from_secs(20);
    while Instant::now() < deadline {
        if port_open(&a.serve) {
            return Ok(pid);
        }
        std::thread::sleep(Duration::from_millis(250));
    }
    bail!(
        "the runner didn't come up on {} within 20s — see {} for why",
        a.serve,
        serve_log().display()
    );
}

pub fn port_open(addr: &str) -> bool {
    let (host, port) = addr.split_once(':').unwrap_or((addr, "8787"));
    std::net::TcpStream::connect((host, port.parse().unwrap_or(8787))).is_ok()
}

/// Stage 3 — one real exec through the runner: the SSH key must be accepted
/// by the host. The INTERACTIVE retry loop belongs to the front-end: it
/// probes, reports, and lets the operator fix authorized_keys.
#[derive(Debug, Clone)]
pub enum DoorProbe {
    Ok,
    AuthFailed(String),
    Failed(String),
}

/// Find the vmid of the role's container on the host (`pct list` name match
/// on the `-<role>` suffix) — used to write the ACTUAL vmid back into the
/// config after an auto-picked boot.
pub fn find_lxc_vmid(a: &Answers, role: &str) -> Result<u32> {
    let (ok, out) = run(
        &bin("freehold-orchestrator"),
        &[
            "exec",
            "--addr",
            &a.serve,
            "--agent-dir",
            ops_dir().to_str().unwrap(),
            &a.runner,
            "pct list",
        ],
    )?;
    if !ok {
        bail!("pct list unreadable through the runner:\n{out}");
    }
    let suffix = format!("-{role}");
    for line in out.lines().skip(1) {
        let cols: Vec<&str> = line.split_whitespace().collect();
        if let (Some(name), Some(vmid)) = (cols.last(), cols.first())
            && name.ends_with(&suffix)
        {
            return vmid
                .parse()
                .map_err(|_| anyhow::anyhow!("unparseable vmid {vmid:?} in {line:?}"));
        }
    }
    bail!("no container named *{suffix} found on the host:\n{out}")
}

/// The guest's current IPv4 (CIDR) — read back after a DHCP boot.
pub fn read_lxc_ip(a: &Answers, vmid: u32) -> Result<String> {
    let (ok, out) = run(
        &bin("freehold-orchestrator"),
        &[
            "exec",
            "--addr",
            &a.serve,
            "--agent-dir",
            ops_dir().to_str().unwrap(),
            &a.runner,
            &format!("pct exec {vmid} -- ip -4 -o addr show eth0"),
        ],
    )?;
    if !ok {
        bail!("ip readback failed on LXC {vmid}:\n{out}");
    }
    out.split_whitespace()
        .find(|t| t.contains('/'))
        .map(|t| t.to_string())
        .filter(|t| t != "127.0.0.1/8")
        .ok_or_else(|| anyhow::anyhow!("no ipv4 on LXC {vmid} eth0:\n{out}"))
}

/// Persist the real post-boot coordinates into the config.
pub fn write_back_lxc(a: &Answers, cfg: &mut config::Config, role: &str) -> Result<()> {
    let vmid = find_lxc_vmid(a, role)?;
    let ip = read_lxc_ip(a, vmid)?;
    let guest = if role == "relay" {
        &mut cfg.lxc.relay
    } else {
        &mut cfg.lxc.cp
    };
    guest.vmid = Some(vmid);
    guest.ip = Some(ip);
    Ok(())
}

/// Is the LXC with this vmid present on the target host (via the runner)?
pub fn probe_lxc(a: &Answers, vmid: Option<u32>) -> Result<bool> {
    let Some(vmid) = vmid else { return Ok(false) };
    let (ok, out) = run(
        &bin("freehold-orchestrator"),
        &[
            "exec",
            "--addr",
            &a.serve,
            "--agent-dir",
            ops_dir().to_str().unwrap(),
            &a.runner,
            &format!("pct status {vmid}"),
        ],
    )?;
    if ok {
        Ok(true)
    } else if out.contains("does not exist") {
        Ok(false)
    } else {
        bail!("pct status {vmid} unreadable through the runner:\n{out}")
    }
}

pub fn verify_door_once(a: &Answers) -> Result<DoorProbe> {
    let (ok, out) = run(
        &bin("freehold-orchestrator"),
        &[
            "exec",
            "--addr",
            &a.serve,
            "--agent-dir",
            ops_dir().to_str().unwrap(),
            &a.runner,
            "echo freehold-door-ok",
        ],
    )?;
    if ok && out.contains("freehold-door-ok") {
        Ok(DoorProbe::Ok)
    } else if out.contains("authentication failed") {
        Ok(DoorProbe::AuthFailed(out))
    } else {
        Ok(DoorProbe::Failed(out))
    }
}

/// Run a configured stage of the `freehold-orchestrator` CLI.
pub fn stage_any(name: &str, args: &[&str], ok_msg: &str) -> Result<String> {
    let (ok, out) = run(&bin("freehold-orchestrator"), args)?;
    if ok {
        Ok(ok_msg.to_string())
    } else {
        bail!("{name} failed:\n{out}");
    }
}

/// Boot the role's LXC. The vmid is AUTO-PICKED (lowest free via
/// `pvesh /cluster/nextid`) when None; the guest IP is DHCP-assigned and the
/// A4 domain gate verifies the domain resolves to it. Both are read back and
/// stored in the config by the configure pipeline after the boot.
pub fn stage_bootstrap(a: &Answers, role: &str, vmid: Option<u32>) -> Result<()> {
    let mut args: Vec<String> = vec![
        "bootstrap".into(),
        "--kind".into(),
        "proxmox-lxc".into(),
        "--role".into(),
        role.into(),
        "--target".into(),
        a.runner.clone(),
        "--domain".into(),
        a.domain.clone(),
        "--rootfs-gb".into(),
        a.rootfs_gb.to_string(),
        "--memory-mb".into(),
        a.memory_mb.to_string(),
        "--operator-pubkey".into(),
        a.operator_pk.clone(),
    ];
    let label = match vmid {
        Some(v) => {
            args.push("--vmid".into());
            args.push(v.to_string());
            format!("(vmid {v})")
        }
        None => "(auto vmid, dhcp ip)".into(),
    };
    let arg_refs: Vec<&str> = args.iter().map(String::as_str).collect();
    stage_any(
        &format!("booting the {role} LXC {label}"),
        &arg_refs,
        &format!("{role} LXC booted"),
    )?;
    Ok(())
}

pub fn stage_deploy_relay(a: &Answers) -> Result<()> {
    let relay_vmid = a
        .relay_vmid
        .ok_or_else(|| anyhow::anyhow!("relay LXC not booted yet — no vmid to deploy into"))?;
    stage_any(
        "deploy-relay",
        &[
            "deploy-relay",
            "--target",
            &a.runner,
            "--lxc",
            &relay_vmid.to_string(),
            "--domain",
            &a.domain,
            "--relay-url",
            &format!("https://{}", a.domain),
            "--owner-pubkey",
            &a.operator_pk,
            "--operator-pubkey",
            &a.operator_pk,
        ],
        &format!("relay live at https://{}", a.domain),
    )?;
    Ok(())
}

pub fn stage_deploy_cp(a: &Answers) -> Result<()> {
    let cp_vmid = a
        .cp_vmid
        .ok_or_else(|| anyhow::anyhow!("cp LXC not booted yet — no vmid to deploy into"))?;
    stage_any(
        "deploy-cp",
        &[
            "deploy-cp",
            "--target",
            &a.runner,
            "--lxc",
            &cp_vmid.to_string(),
            "--relay-url",
            &format!("https://{}", a.domain),
            "--binary",
            &rel_bin("control-plane").display().to_string(),
            "--runner-binary",
            &rel_bin("runner").display().to_string(),
            "--operator-pubkey",
            &a.operator_pk,
        ],
        &format!("control plane live at https://cp-{}", a.domain),
    )?;
    Ok(())
}

/// Best-effort: NIP-11 gives us the relay's signing pubkey (the trust anchor
/// for the runner whitelist). Not fatal — the operator can read it from the
/// relay's data dir later.
pub fn relay_pubkey_nip11(domain: &str) -> Option<String> {
    let out = Command::new("curl")
        .args([
            "-sk",
            "--max-time",
            "8",
            "-H",
            "Accept: application/nostr+json",
            &format!("https://{domain}/"),
        ])
        .output()
        .ok()?;
    let text = String::from_utf8(out.stdout).ok()?;
    let pk: serde_json::Value = serde_json::from_str(&text).ok()?;
    let pk = pk.get("pubkey")?.as_str()?.to_string();
    (pk.len() == 64).then_some(pk)
}
