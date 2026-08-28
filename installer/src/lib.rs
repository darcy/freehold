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

/// clap default_value needs a &'static str — points at the home ops dir when
/// it exists, else the legacy cwd-relative path.
pub fn default_agent_dir_str() -> &'static str {
    if ops_dir().join("identity.json").exists() {
        Box::leak(ops_dir().to_string_lossy().into_owned().into_boxed_str())
    } else {
        "./.freehold/control-plane/agent-ops"
    }
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
    /// Gateway for STATIC ips (empty/unused with DHCP).
    pub relay_gw: String,
    pub cp_vmid: Option<u32>,
    pub cp_ip: Option<String>,
    pub k3s_vmid: Option<u32>,
    pub k3s_ip: Option<String>,
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
            relay_gw: "192.168.30.1".into(), // bootstrap-time only, not config
            cp_vmid: cfg.lxc.cp.vmid,
            cp_ip: cfg.lxc.cp.ip.clone(),
            k3s_vmid: cfg.lxc.k3s.vmid,
            k3s_ip: cfg.lxc.k3s.ip.clone(),
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
            relay_gw: "192.168.30.1".into(),
            cp_vmid: None,
            cp_ip: None,
            k3s_vmid: None,
            k3s_ip: None,
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
    // The runner package lands in the FREEHOLD HOME (absolute) — the CLI's
    // relative default (./.freehold/runner/<name>) would collide with any
    // stale cwd-local state.
    let runner_dir = runner_pkgs().join(&a.runner);
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
            "--runner-dir",
            runner_dir.to_str().unwrap(),
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
        || out.contains("already holds a runner")
    {
        // reuse is only safe when the PACKAGE is actually there — a leftover
        // state record with a deleted package would cascade on every later
        // stage (grant/verify resolve the missing package).
        if !runner_dir.join("identity.json").exists() {
            bail!(
                "a runner '{}' record exists but its package at {} is gone — \
                 wipe the world for a clean re-bootstrap:\n  rm -rf ~/.freehold\n\
                 (or revoke the record: control-plane revoke {} --state-dir {})",
                a.runner,
                runner_dir.display(),
                a.runner,
                state_dir().display()
            );
        }
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
    // ALWAYS a fresh serve for the CURRENT package: a listener on the addr
    // may be a STALE serve (from an earlier world) holding old identities
    // in memory — reusing it makes every signed call fail with
    // "signature does not verify".
    kill_serve_on(&a.serve);
    // the serve log's dir is part of the world — a wiped home lacks it, and
    // the spawn's `> log` redirect FAILS (sh exits before nohup) leaving the
    // port never opened — that was the recurring "serve failed" ghost.
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

/// Kill any runner serve bound to this MCP address (kills stale-world
/// serves; a fresh one is spawned by stage_serve right after) and WAIT for
/// the port to actually close — spawning while the old listener still holds
/// the address makes the fresh runner die with "Address already in use".
pub fn kill_serve_on(addr: &str) {
    let pat = format!("runner serve.*--addr {}", regex_escape(addr));
    let _ = std::process::Command::new("pkill")
        .args(["-f", &pat])
        .output();
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
    while port_open(addr) && std::time::Instant::now() < deadline {
        std::thread::sleep(std::time::Duration::from_millis(100));
    }
}

fn regex_escape(s: &str) -> String {
    s.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() {
                c.to_string()
            } else {
                format!("\\{}", c)
            }
        })
        .collect()
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
/// Match the guest by its FULL `<domain-with-dashes>-<role>` name — the
/// suffix-only match can hit ANOTHER world's container on a multi-world host
/// (bootstrap names guests from the domain precisely so a host can carry
/// several: `find_lxc_vmid`'s `ends_with` is wrong there).
pub fn lxc_name(a: &Answers, role: &str) -> Result<String, anyhow::Error> {
    let normalized: String = a.domain.replace('.', "-");
    Ok(format!("{normalized}-{role}"))
}

pub fn find_lxc_vmid_exact(a: &Answers, role: &str) -> Result<u32> {
    let name = lxc_name(a, role)?;
    let vmid = find_lxc_vmid_find(a, &name)?;
    Ok(vmid)
}

fn find_lxc_vmid_find(a: &Answers, exact: &str) -> Result<u32> {
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
    for line in out.lines().skip(1) {
        let cols: Vec<&str> = line.split_whitespace().collect();
        if let (Some(name), Some(vmid)) = (cols.last(), cols.first())
            && *name == exact
        {
            return vmid
                .parse()
                .map_err(|_| anyhow::anyhow!("unparseable vmid {vmid:?} in {line:?}"));
        }
    }
    bail!("no container named {exact} found on the host:\n{out}")
}

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
    let vmid = find_lxc_vmid_exact(a, role)?;
    let ip = read_lxc_ip(a, vmid)?;
    let guest = if role == "relay" {
        &mut cfg.lxc.relay
    } else if role == "k3s" {
        &mut cfg.lxc.k3s
    } else {
        &mut cfg.lxc.cp
    };
    guest.vmid = Some(vmid);
    guest.ip = Some(ip);
    // a k3s guest means the world OWNS a kube substrate — record the piece
    // here so every write-back (stage + TUI) carries it.
    if role == "k3s" && !cfg.managed.iter().any(|m| m == "k3s") {
        cfg.managed.push("k3s".into());
    }
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
    // --addr/--agent-dir are per-SUBCOMMAND (the orchestrator's flattened
    // CommonArgs) — THEY GO AFTER the subcommand name, not before.
    let agent_dir = ops_dir().to_str().unwrap().to_string();
    let mut full: Vec<String> = vec![args[0].to_string()];
    full.push("--addr".into());
    full.push("127.0.0.1:8787".into());
    full.push("--agent-dir".into());
    full.push(agent_dir);
    full.extend(args[1..].iter().map(|a| a.to_string()));
    let refs: Vec<&str> = full.iter().map(String::as_str).collect();
    let (ok, out) = run(&bin("freehold-orchestrator"), &refs)?;
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
    // a SPECIFIED IP is STATIC (the user owns addressing + the proxy/DNS
    // target); an empty one = DHCP + whatever the bridge assigns (recorded
    // in the config after boot — the UI says this loudly).
    let ip = match role {
        "relay" => a.relay_ip.clone(),
        "cp" => a.cp_ip.clone(),
        _ => a.k3s_ip.clone(),
    };
    let ip = ip.filter(|i| !i.trim().is_empty());
    if let Some(ip) = &ip {
        args.push("--lxc-ip".into());
        args.push(ip.clone());
        args.push("--lxc-gw".into());
        args.push(a.relay_gw.clone());
    }
    let label = match vmid {
        Some(v) => {
            args.push("--vmid".into());
            args.push(v.to_string());
            match &ip {
                Some(ip) => format!("(vmid {v}, static ip {ip})"),
                None => format!("(vmid {v}, dhcp)"),
            }
        }
        None => match &ip {
            Some(ip) => format!("(auto vmid, static ip {ip})"),
            None => "(auto vmid, dhcp ip)".into(),
        },
    };
    // Durable-plane mounts (born-at-create): if the plane is resolved for
    // THIS role, the HOST-resolved mounts recorded by the storage stage are
    // baked into `pct create` as `--mount <host-source>:<guest-path>`. The
    // sources are real host mountpoints (PVE rejects a bare dataset name),
    // and the k3s role reads its own "k3s" key (the tenant is k3s-volumes).
    {
        let cfg_path = config::Config::default_path();
        if let Ok(Some(cfg)) = config::Config::load(&cfg_path)
            && let Some(mounts) = cfg.plane.mounts.get(role)
        {
            for m in mounts {
                args.push("--mount".into());
                args.push(format!("{}:{}", m.source, m.guest_path));
            }
        }
    }
    let arg_refs: Vec<&str> = args.iter().map(String::as_str).collect();
    stage_any(
        &format!("booting the {role} LXC {label}"),
        &arg_refs,
        &format!("{role} LXC booted"),
    )?;
    Ok(())
}

/// The k3s substrate stage (configure): boot the k3s LXC if missing, install
/// k3s inside it (unprivileged-LXC spike posture: the KubeletInUserNamespace
/// feature gate must ride AFTER the subcommand), wait for the API, then
/// record the guest coords + the managed piece.
pub fn stage_k3s(a: &Answers) -> Result<()> {
    // boot if missing: the config may not know the vmid yet (a prior run
    // died before the write-back) — find by NAME first, then probe.
    let existing = match a.k3s_vmid {
        Some(v) => probe_lxc(a, Some(v))?.then_some(v),
        None => find_lxc_vmid_exact(a, "k3s")
            .ok()
            .filter(|v| matches!(probe_lxc(a, Some(*v)), Ok(true))),
    };
    if existing.is_none() {
        stage_bootstrap(a, "k3s", a.k3s_vmid)?;
    }
    let vmid = find_lxc_vmid_exact(a, "k3s")?;
    // install k3s in the guest when absent. The script is single-quote-free
    // (it travels inside a single-quoted bash -c through the runner); the
    // unit heredoc is unquoted-safe (no $ in its content).
    let (installed, out) = run(
        &bin("freehold-orchestrator"),
        &[
            "exec",
            "--addr",
            &a.serve,
            "--agent-dir",
            ops_dir().to_str().unwrap(),
            &a.runner,
            &format!("pct exec {vmid} -- command -v k3s"),
        ],
    )?;
    if (!installed || out.trim().is_empty()) && !out.contains("/usr/local/bin/k3s") {
        let script = r#"set -euo pipefail
export PATH=/usr/local/bin:/root/.cargo/bin:$PATH
DEBIAN_FRONTEND=noninteractive apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl jq
if ! command -v kubectl >/dev/null 2>&1; then
  curl -sfL https://get.k3s.io -o /tmp/k3s-install.sh
  INSTALL_K3S_EXEC="server --kubelet-arg feature-gates=KubeletInUserNamespace=true" sh /tmp/k3s-install.sh
fi
if ! grep -q KubeletInUserNamespace /etc/systemd/system/k3s.service 2>/dev/null; then
cat > /etc/systemd/system/k3s.service <<UNIT
[Unit]
Description=Lightweight Kubernetes
Documentation=https://k3s.io
Wants=network-online.target
After=network-online.target
[Install]
WantedBy=multi-user.target
[Service]
Type=notify
EnvironmentFile=-/etc/default/%N
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/k3s server --kubelet-arg feature-gates=KubeletInUserNamespace=true
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s
UNIT
  systemctl daemon-reload
  systemctl restart k3s
fi
KUBECTL=$(command -v kubectl)
K="$KUBECTL --kubeconfig /etc/rancher/k3s/k3s.yaml"
for i in $(seq 1 30); do
  $K get nodes >/dev/null 2>&1 && break
  sleep 10
done
$K get nodes 2>&1 | tail -2 | head -1
mkdir -p /srv/data/k8s-volumes
"#;
        let (ok, out) = run(
            &bin("freehold-orchestrator"),
            &[
                "exec",
                "--addr",
                &a.serve,
                "--agent-dir",
                ops_dir().to_str().unwrap(),
                "--timeout",
                "900",
                &a.runner,
                &format!("pct exec {vmid} -- bash -c '{}'", script.trim()),
            ],
        )?;
        if !ok {
            bail!(
                "k3s install failed:
{out}"
            );
        }
    }
    // record the guest coords + the managed piece.
    let cfg_path = config::Config::default_path();
    if let Ok(Some(mut cfg)) = config::Config::load(&cfg_path) {
        write_back_lxc(a, &mut cfg, "k3s").ok();
        let _ = cfg.save(&cfg_path);
    }
    Ok(())
}

/// Phase 0.12 — the durable-plane stage. Resolve the storage backend
/// (consent-gated create), ensure each tenant's dataset, and record the
/// tenant→dataset mapping into the config (the two-place rule). Idempotent:
/// re-resolve against an already-resolved target confirms + creates nothing.
///
/// `consent` is the operator's answer to creating a NEW backend (the
/// `--confirm-storage` gate) — the pipeline itself is non-interactive; the
/// front-ends translate the operator's prompt into this bool.
pub fn stage_storage(a: &Answers, consent: bool) -> Result<()> {
    // Resolve the backend (consent-gated create). The `--confirm-storage`
    // flag is appended ONLY when consent is given — never an empty-string
    // argv element (clap rejects "" on a positional-less subcommand).
    let mut resolve_args = vec![
        "storage".to_string(),
        "resolve".to_string(),
        "--addr".to_string(),
        a.serve.clone(),
        "--agent-dir".to_string(),
        ops_dir().to_str().unwrap().to_string(),
        "--target".to_string(),
        a.runner.clone(),
    ];
    if consent {
        resolve_args.push("--confirm-storage".to_string());
    }
    let args: Vec<&str> = resolve_args.iter().map(String::as_str).collect();
    let (ok, out) = run(&bin("freehold-orchestrator"), &args)?;
    if !ok {
        bail!("storage resolution failed:\n{out}");
    }
    // Thread the DETECTED backend identity from resolve into the ensure
    // steps + the config: a zpool named anything but rpool, or a stock PVE
    // LVM host (VG pve), must NOT be driven as 'rpool'. resolve prints
    // `STORAGE-POOL: <name>` (machine-parseable); we use it verbatim.
    let pool = out
        .lines()
        .find_map(|l| l.strip_prefix("STORAGE-POOL: "))
        .map(str::trim)
        .filter(|p| !p.is_empty())
        .unwrap_or("rpool");
    //
    // Ensure + record each durable tenant's HOST-resolved mounts. The LXC
    // ROLE that rides a tenant differs from the tenant's config key:
    //   tenant        role      guest mount(s)
    //   relay  ->     relay     /var/lib/docker + /srv/buzz-relay
    //   cp     ->     cp        /srv/freehold
    //   k3s-volumes -> k3s      /srv/data/k8s-volumes
    let role_for: &[(&str, &str)] = &[("relay", "relay"), ("cp", "cp"), ("k3s-volumes", "k3s")];
    let cfg_path = config::Config::default_path();
    let Some(mut cfg) = config::Config::load(&cfg_path)? else {
        // The storage stage runs IN the configure pipeline — a config to
        // record the mapping into is mandatory, not optional.
        bail!(
            "no config at {} — cannot record the durable-plane mapping",
            cfg_path.display()
        );
    };
    let mut resolved_any = false;
    for (tenant, role) in role_for {
        let mut ensure_args = vec![
            "storage".to_string(),
            "ensure".to_string(),
            "--addr".to_string(),
            a.serve.clone(),
            "--agent-dir".to_string(),
            ops_dir().to_str().unwrap().to_string(),
            "--target".to_string(),
            a.runner.clone(),
            "--tenant".to_string(),
            tenant.to_string(),
            "--domain".to_string(),
            a.domain.clone(),
            "--pool".to_string(),
            pool.to_string(), // the REAL backend identity from resolve
        ];
        // Honor the RECORDED backend kind (once the first tenant has ensured
        // it): on a host with BOTH a zpool and a VG, re-detection would
        // always pick ZFS and drive an LVM-backed tenant the wrong way.
        if let Some(kind) = cfg.plane.backend_kind.as_deref() {
            ensure_args.push("--kind".to_string());
            ensure_args.push(kind.to_string());
        }
        let ensure_refs: Vec<&str> = ensure_args.iter().map(String::as_str).collect();
        let (ok, out) = run(&bin("freehold-orchestrator"), &ensure_refs)?;
        if !ok {
            bail!("storage ensure {tenant} failed:\n{out}");
        }
        // Capture the HOST-resolved mounts the ensure printed.
        let mut mounts = Vec::new();
        for line in out.lines() {
            if let Some(rest) = line.strip_prefix("STORAGE-MOUNT ")
                && let Some((src, guest)) = rest.split_once(':')
            {
                mounts.push(config::PlaneMount {
                    source: src.to_string(),
                    guest_path: guest.to_string(),
                });
            }
        }
        if !mounts.is_empty() {
            cfg.plane.backend = Some(pool.to_string());
            cfg.plane.mounts.insert(role.to_string(), mounts);
            resolved_any = true;
        }
        // Record the discoverable backend KIND printed by ensure ("zfs" or
        // "lvmth") so teardown/dispatch pick the right driver.
        for line in out.lines() {
            if let Some(rest) = line.strip_prefix("STORAGE-BACKEND: ") {
                let kind = rest.split_whitespace().next().unwrap_or("").to_string();
                if !kind.is_empty() {
                    cfg.plane.backend_kind = Some(kind);
                }
                break;
            }
        }
    }
    if resolved_any {
        let _ = cfg.save(&cfg_path);
    } else {
        // A host with no backend and consent withheld bails inside resolve
        // ABOVE; reaching here with no mounts is a real error, not a no-op.
        if consent {
            bail!(
                "storage resolve/ensure recorded no mounts — resolve said create but ensure produced none"
            );
        }
    }
    Ok(())
}

pub fn stage_deploy_relay(a: &Answers) -> Result<()> {
    // the config may predate the boot's write-back — resolve the vmid from
    // the host when it's missing (the check already proved the LXC exists).
    let relay_vmid = match a.relay_vmid {
        Some(v) => v,
        None => find_lxc_vmid_exact(a, "relay")?,
    };
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
    let cp_vmid = match a.cp_vmid {
        Some(v) => v,
        None => find_lxc_vmid_exact(a, "cp")?,
    };
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

#[cfg(test)]
mod live_tests {
    use super::*;

    /// Live-world: boots/verifies the relay stage through the real runner +
    /// host. Idempotent (the driver reuses an existing LXC). Run explicitly.
    #[test]
    #[ignore]
    fn relay_boot_stage_runs_against_live_world() {
        let cfg = config::Config::load(&config::Config::default_path())
            .unwrap()
            .expect("config present");
        let a = Answers::from_config(&cfg);
        stage_bootstrap(&a, "relay", cfg.lxc.relay.vmid).expect("relay boot stage works");
    }
}
