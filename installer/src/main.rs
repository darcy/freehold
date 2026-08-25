//! freehold-install — the one-shot interactive bring-up wrapper.
//!
//! Composes the EXISTING CLI binaries (`control-plane`, `runner`, `freehold`)
//! into a guided install: collect the few decisions with sane defaults, walk
//! the operator through installing the SSH door, start the runner in the
//! background, verify the door REALLY works, then boot + deploy relay and CP
//! LXCs on the Proxmox host — with a progress line per stage.
//!
//! Designed for the NEW-install path (relay + cp on a Proxmox host as LXC
//! guests). The underlying stages are idempotent where it matters: a re-run
//! reuses an existing runner package and re-verifies the door.
//!
//! How to run (from the repo root — nothing writes outside `./.freehold` and
//! the repo's `target/`):
//!
//!     cargo build --workspace --bins   # once (the wrapper runs the sibling
//!                                      # binaries directly; it cannot invoke
//!                                      # cargo itself while `cargo run`
//!                                      # holds the build lock)
//!     ./target/debug/freehold-install

use anyhow::{Context, Result, bail};
use dialoguer::{Confirm, Input, Select, theme::ColorfulTheme};
use indicatif::{ProgressBar, ProgressStyle};
use std::fs;
use std::net::TcpStream;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{Duration, Instant};

const STATE_DIR: &str = "./.freehold/control-plane";
const OPS_DIR: &str = "./.freehold/control-plane/agent-ops";
const RUNNER_PKGS: &str = "./.freehold/runner";
const SERVE_LOG: &str = "./.freehold/installer/serve.log";

const BANNER: &str = r#"
  ╭──────────────────────────────────────────────────────────────╮
  │                     Welcome to Freehold                      │
  │                                                              │
  │   Reclaim the future we were promised — one command.         │
  │                                                              │
  │   This installer brings up your appliance end to end:        │
  │     • provisions the door into your Proxmox host            │
  │     • starts the runner (your agent's hands on the host)    │
  │     • boots + deploys the Buzz relay (LXC)                  │
  │     • boots + deploys the control plane (its own LXC)       │
  │                                                              │
  │   You'll be asked a handful of questions with defaults.      │
  │   Ctrl-C at any time aborts cleanly; stages are idempotent.  │
  ╰──────────────────────────────────────────────────────────────╯
"#;

struct Answers {
    host: String,        // root@192.168.30.224
    runner: String,      // proxmox-box
    serve: String,       // 127.0.0.1:8787
    domain: String,      // freehold-test.darcydev.net
    relay_vmid: u32,     // 100
    relay_ip: String,    // 192.168.30.238/24
    relay_gw: String,    // 192.168.30.1
    cp_vmid: u32,        // 102
    cp_ip: String,       // 192.168.30.254/24
    rootfs_gb: u32,      // 16
    memory_mb: u32,      // 2048
    operator_pk: String, // 64-hex (npub converted by the CLIs; we keep hex)
    operator_generated: bool,
    operator_dir: PathBuf,
}

fn repo_root() -> &'static Path {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .expect("installer under repo root")
}

fn bin(name: &str) -> PathBuf {
    repo_root().join("target").join("debug").join(name)
}

/// Release build — the deploy-cp stage ships these to the box as base64;
/// debug binaries are ~15x larger and turn the ship into a 10-minute stall.
fn rel_bin(name: &str) -> PathBuf {
    repo_root().join("target").join("release").join(name)
}

/// The wrapper drives the REAL sibling binaries (as the user would by hand).
/// We cannot spawn `cargo` from under `cargo run` (target-dir lock), so the
/// bins must exist; give the exact one-liner when they don't.
fn ensure_bins() -> Result<()> {
    let debug_bins = ["control-plane", "runner", "freehold"];
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
        missing
            .iter()
            .map(|b| format!("target/debug/{b}"))
            .collect::<Vec<_>>()
            .join(", ")
    );
}

fn spinner(msg: impl Into<String>) -> ProgressBar {
    let p = ProgressBar::new_spinner();
    p.set_style(
        ProgressStyle::with_template("{spinner:.green} {msg}")
            .unwrap()
            .tick_strings(&["⣷", "⣯", "⣟", "⡿", "⢿", "⣻", "⣽", "⣾"]),
    );
    p.set_message(msg.into());
    p.enable_steady_tick(Duration::from_millis(120));
    p
}

/// Run a sibling binary, capturing stdout+stderr. Returns (ok, output).
fn run(bin_path: &Path, args: &[&str]) -> Result<(bool, String)> {
    let out = Command::new(bin_path)
        .args(args)
        .output()
        .with_context(|| format!("failed to spawn {}", bin_path.display()))?;
    let mut text = String::from_utf8_lossy(&out.stdout).into_owned();
    text.push_str(&String::from_utf8_lossy(&out.stderr));
    Ok((out.status.success(), text))
}

fn print_tail(text: &str, n: usize) {
    let lines: Vec<&str> = text.lines().filter(|l| !l.trim().is_empty()).collect();
    for l in lines.iter().rev().take(n).rev() {
        eprintln!("    {l}");
    }
}

fn confirm(prompt: &str, default: bool) -> Result<bool> {
    Ok(Confirm::with_theme(&ColorfulTheme::default())
        .with_prompt(prompt)
        .default(default)
        .interact()?)
}

// ---------------------------------------------------------------- prompts

fn ask<T>(prompt: &str, default: T) -> Result<T>
where
    T: Clone + std::fmt::Display + std::str::FromStr + Send + Sync + 'static,
    T::Err: std::fmt::Display,
{
    Ok(Input::with_theme(&ColorfulTheme::default())
        .with_prompt(prompt)
        .default(default)
        .interact_text()?)
}

fn collect() -> Result<Answers> {
    println!("{BANNER}");
    println!("  First, a few details about your world. Defaults are shown in [brackets].");
    println!();

    let host = ask(
        "Proxmox host (address the runner will SSH into)",
        "root@192.168.30.224".to_string(),
    )?;
    let runner = ask("Runner name", "proxmox-box".to_string())?;
    let serve = ask(
        "Runner MCP address (loopback)",
        "127.0.0.1:8787".to_string(),
    )?;
    let domain = ask(
        "Relay domain (must resolve to your host — the identity gate)",
        "freehold-test.darcydev.net".to_string(),
    )?;
    let relay_vmid = ask("Relay LXC vmid", 100u32)?;
    let relay_ip = ask("Relay LXC IP (CIDR)", "192.168.30.238/24".to_string())?;
    let cp_vmid = ask("Control-plane LXC vmid", 102u32)?;
    let cp_ip = ask(
        "Control-plane LXC IP (CIDR)",
        "192.168.30.254/24".to_string(),
    )?;
    let relay_gw = ask("LXC gateway", "192.168.30.1".to_string())?;
    let rootfs_gb = ask("LXC rootfs size (GB)", 16u32)?;
    let memory_mb = ask("LXC memory (MB)", 2048u32)?;

    println!();
    let have_key = Select::with_theme(&ColorfulTheme::default())
        .with_prompt("Operator identity")
        .item("I have a Nostr key already (paste npub or hex)")
        .item("Generate one for me (an identity dir we keep for you)")
        .default(0)
        .interact()?;

    let (operator_pk, operator_generated, operator_dir) = if have_key == 0 {
        let npub: String = Input::with_theme(&ColorfulTheme::default())
            .with_prompt("Your Nostr public key (npub1… or 64 hex)")
            .interact_text()?;
        let pk = freehold_core::identity::parse_pubkey_input(&npub)
            .map_err(|e| anyhow::anyhow!("invalid pubkey: {e}"))?;
        (pk, false, PathBuf::new())
    } else {
        let dir = PathBuf::from("./.freehold/control-plane/operator");
        let id = mint_identity(&dir)?;
        println!();
        println!("  Generated a fresh identity for you:");
        println!("    pubkey: {}", id.nostr_pubkey_hex());
        println!(
            "    stored: {} (0600 — this IS your key, keep it safe)",
            dir.display()
        );
        println!("  You'll log into the console with it (no secrets on screen).");
        (id.nostr_pubkey_hex(), true, dir)
    };

    Ok(Answers {
        host,
        runner,
        serve,
        domain,
        relay_vmid,
        relay_ip,
        relay_gw,
        cp_vmid,
        cp_ip,
        rootfs_gb,
        memory_mb,
        operator_pk,
        operator_generated,
        operator_dir,
    })
}

fn mint_identity(dir: &Path) -> Result<freehold_core::identity::Identity> {
    fs::create_dir_all(dir)?;
    if dir.join("identity.json").exists() {
        return freehold_core::identity::Identity::load(dir).map_err(Into::into);
    }
    let p = spinner("minting operator identity…");
    let (ok, out) = run(
        &bin("runner"),
        &["keys", "init", "--state-dir", dir.to_str().unwrap()],
    )?;
    p.finish_and_clear();
    if !ok {
        bail!("keys init failed:\n{out}");
    }
    freehold_core::identity::Identity::load(dir).map_err(Into::into)
}

// ---------------------------------------------------------------- stages

fn ensure_ops_agent() -> Result<freehold_core::identity::Identity> {
    let dir = PathBuf::from(OPS_DIR);
    if !dir.join("identity.json").exists() {
        fs::create_dir_all(&dir)?;
        let p = spinner("minting the ops-agent identity (drives the runner)…");
        let (ok, out) = run(&bin("runner"), &["keys", "init", "--state-dir", OPS_DIR])?;
        p.finish_and_clear();
        if !ok {
            bail!("ops-agent keys init failed:\n{out}");
        }
    }
    freehold_core::identity::Identity::load(&dir).map_err(Into::into)
}

/// Stage 1 — provision the SSH door into the PVE host. A fresh runner gets
/// its keypair generated in-process; an existing one is reused (the public
/// key was already printed at ITS provisioning; the door is verified live in
/// stage 3 either way).
fn stage_provision(a: &Answers, agent_pk: &str) -> Result<Option<String>> {
    let p = spinner("provisioning the runner…");
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
            STATE_DIR,
            "--grant",
            agent_pk,
        ],
    )?;
    p.finish_and_clear();

    if ok {
        // The public key line the operator must install on the target.
        let pubkey = out
            .lines()
            .find(|l| l.trim_start().starts_with("ssh-ed25519"))
            .map(|l| l.trim().to_string());
        println!(
            "  ✓ runner provisioned — package at ./.freehold/runner/{}",
            a.runner
        );
        Ok(pubkey)
    } else if out.contains("already exists")
        || out.contains("RunnerExists")
        || out.contains("PackageDirInUse")
    {
        println!(
            "  ✓ runner {} already exists — reusing its package (door re-verified below)",
            a.runner
        );
        Ok(None)
    } else {
        bail!("provision failed:\n{out}");
    }
}

/// Wait for the operator to install the key, then keep them honest.
fn door_gate(a: &Answers, pubkey: &str) -> Result<()> {
    loop {
        println!();
        println!("  ─────────────────────────────────────────────────────────");
        println!(
            "  Finish the door: add this line to {}'s ~/.ssh/authorized_keys:",
            a.host
        );
        println!();
        println!("    {pubkey}");
        println!();
        println!(
            "  (on the host: mkdir -p /root/.ssh && echo '<line>' >> /root/.ssh/authorized_keys)"
        );
        println!("  ─────────────────────────────────────────────────────────");
        let answer: String = Input::with_theme(&ColorfulTheme::default())
            .with_prompt("Press ENTER when it's in place, or 'r' to show it again, 'q' to quit")
            .allow_empty(true)
            .interact_text()?;
        return match answer.trim() {
            "" => Ok(()),
            "r" | "R" => continue,
            "q" | "Q" => bail!("aborted by the operator (door not installed)"),
            _ => continue,
        };
    }
}

/// Stage 2 — the runner serves in the background (detached; survives this
/// process). Reuses an already-listening serve on the same address.
fn stage_serve(a: &Answers) -> Result<String> {
    if port_open(&a.serve) {
        println!(
            "  ✓ a runner is already serving on {} — reusing it",
            a.serve
        );
        return Ok("— (reused)".to_string());
    }
    fs::create_dir_all(Path::new("./.freehold/installer"))?;
    let pkg = PathBuf::from(format!("{RUNNER_PKGS}/{}", a.runner));
    if !pkg.join("identity.json").exists() {
        bail!(
            "runner package {} is missing — the provision stage created it, something is off",
            pkg.display()
        );
    }
    let p = spinner("starting the runner in the background…");
    // Detached + nohup'd: survives the installer exiting. Log lives in ./.freehold.
    let sh = format!(
        "nohup '{}' serve --state-dir {} --addr {} > {} 2>&1 & echo $!",
        bin("runner").display(),
        pkg.display(),
        a.serve,
        SERVE_LOG
    );
    // -c with the built-in; the outer sh exits immediately.
    let out = Command::new("sh")
        .arg("-c")
        .arg(&sh)
        .output()
        .with_context(|| "spawn detached serve")?;
    let pid = String::from_utf8_lossy(&out.stdout).trim().to_string();

    let deadline = Instant::now() + Duration::from_secs(20);
    while Instant::now() < deadline {
        if port_open(&a.serve) {
            p.finish_and_clear();
            println!(
                "  ✓ runner serving on {} (pid {pid}, log {SERVE_LOG})",
                a.serve
            );
            return Ok(pid);
        }
        std::thread::sleep(Duration::from_millis(250));
    }
    p.finish_and_clear();
    bail!(
        "the runner didn't come up on {} within 20s — see {SERVE_LOG} for why",
        a.serve
    );
}

fn port_open(addr: &str) -> bool {
    let (host, port) = addr.split_once(':').unwrap_or((addr, "8787"));
    TcpStream::connect((host, port.parse().unwrap_or(8787))).is_ok()
}

/// Stage 3 — the moment of truth: drive a REAL exec through the runner; the
/// runner's SSH key must be accepted by the host. On auth failure, loop until
/// the operator has fixed authorized_keys (they asked for exactly this loop).
/// Repeated failures after several attempts: the runner package itself may
/// hold an unloadable/stale key — a fresh provision is the fix, not more
/// authorized_keys edits.
fn stage_verify_door(a: &Answers) -> Result<()> {
    let mut failures = 0u32;
    loop {
        let p = spinner("testing the SSH door through the runner…");
        let (ok, out) = run(
            &bin("freehold"),
            &[
                "exec",
                "--addr",
                &a.serve,
                "--agent-dir",
                OPS_DIR,
                &a.runner,
                "echo freehold-door-ok",
            ],
        )?;
        p.finish_and_clear();

        if ok && out.contains("freehold-door-ok") {
            println!("  ✓ the door works — {} is reachable", a.host);
            return Ok(());
        }
        let reason = if out.contains("authentication failed") {
            "authentication failed — the runner's key was rejected"
        } else if !ok {
            "the exec call failed"
        } else {
            "the exec returned without our marker"
        };
        println!("  ✗ {reason}");
        failures += 1;
        if failures >= 3 {
            println!();
            println!("  Still failing after {failures} tries. If this runner predates the");
            println!("  ssh-key serialization fix, its PRIVATE key may be unloadable by the");
            println!("  SSH client — authorized_keys edits can't help that.");
            println!("  Fresh start:  rm -rf ./.freehold && ./target/debug/freehold-install");
            println!();
        }
        print_tail(&out, 6);
        println!();
        let answer: String = Input::with_theme(&ColorfulTheme::default())
            .with_prompt("Fix authorized_keys on the host, then press ENTER to retry ('q' to quit)")
            .allow_empty(true)
            .interact_text()?;
        if answer.trim().eq_ignore_ascii_case("q") {
            bail!("aborted at the door check");
        }
    }
}

fn stage(name: &str, args: &[&str], ok_msg: &str) -> Result<()> {
    let p = spinner(name);
    let (ok, out) = run(&bin("freehold"), args)?;
    p.finish_and_clear();
    if ok {
        println!("  ✓ {ok_msg}");
        Ok(())
    } else {
        bail!("{name} failed:\n{out}");
    }
}

fn stage_bootstrap(a: &Answers, role: &str, vmid: u32, ip: &str) -> Result<()> {
    stage(
        &format!("booting the {role} LXC (vmid {vmid}) — domain resolve gate, docker, template…"),
        &[
            "bootstrap",
            "--kind",
            "proxmox-lxc",
            "--role",
            role,
            "--target",
            &a.runner,
            "--domain",
            &a.domain,
            "--vmid",
            &vmid.to_string(),
            "--rootfs-gb",
            &a.rootfs_gb.to_string(),
            "--memory-mb",
            &a.memory_mb.to_string(),
            "--lxc-ip",
            ip,
            "--lxc-gw",
            &a.relay_gw,
            "--operator-pubkey",
            &a.operator_pk,
        ],
        &format!("{role} LXC {vmid} created and ready on the host"),
    )
}

fn stage_deploy_relay(a: &Answers) -> Result<()> {
    stage(
        "deploying the Buzz relay into the LXC (compose bundle, liveness)…",
        &[
            "deploy-relay",
            "--target",
            &a.runner,
            "--lxc",
            &a.relay_vmid.to_string(),
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
    )
}

fn stage_deploy_cp(a: &Answers) -> Result<()> {
    stage(
        "deploying the control plane into its LXC (release ship, console)…",
        &[
            "deploy-cp",
            "--target",
            &a.runner,
            "--lxc",
            &a.cp_vmid.to_string(),
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
    )
}

/// Best-effort: NIP-11 gives us the relay's signing pubkey (the trust anchor
/// for the runner whitelist). Not fatal — the operator can read it from the
/// relay's data dir later.
fn relay_pubkey_nip11(domain: &str) -> Option<String> {
    let p = spinner("reading the relay's signing key (NIP-11)…");
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
        .ok();
    p.finish_and_clear();
    let text = out.and_then(|o| String::from_utf8(o.stdout).ok())?;
    let pk: serde_json::Value = serde_json::from_str(&text).ok()?;
    let pk = pk.get("pubkey")?.as_str()?.to_string();
    if pk.len() == 64 { Some(pk) } else { None }
}

fn summary(a: &Answers, serve_pid: &str) {
    println!();
    println!("  ╭─────────────────────────────────────────────────────────╮");
    println!("  │                    Freehold is up                      │");
    println!("  ╰─────────────────────────────────────────────────────────╯");
    println!();
    println!(
        "  relay:          https://{} (LXC {})",
        a.domain, a.relay_vmid
    );
    println!(
        "  control plane:  https://cp-{} (LXC {})",
        a.domain, a.cp_vmid
    );
    println!(
        "  runner:         serving on {} (pid {})",
        a.serve, serve_pid
    );
    println!("  operator pk:    {}", a.operator_pk);
    if a.operator_generated {
        println!(
            "  your identity:  {}  (your key — keep this directory safe)",
            a.operator_dir.display()
        );
    }
    println!();
    println!("  Log into the console (browser):");
    if a.operator_generated {
        println!(
            "    cargo run -p freehold-orchestrator -- console-login --url https://cp-{} --identity {}",
            a.domain,
            a.operator_dir.display()
        );
    } else {
        println!(
            "    cargo run -p freehold-orchestrator -- console-login --url https://cp-{} --nsec <your-nsec>",
            a.domain
        );
    }
    println!();
    println!("  Your runner is already granted to your ops agent — delegate away.");
}

fn main() -> Result<()> {
    if let Err(e) = ensure_bins() {
        eprintln!("{e:#}");
        std::process::exit(2);
    }
    let answers = collect()?;

    // ——— summary + go ———
    println!();
    println!("  ───────────────── Setting up ─────────────────");
    println!("  host:            {}", answers.host);
    println!("  runner:          {} @ {}", answers.runner, answers.serve);
    println!("  domain:          {}", answers.domain);
    println!(
        "  relay LXC:       {} ({})",
        answers.relay_vmid, answers.relay_ip
    );
    println!(
        "  control-plane:   LXC {} ({})",
        answers.cp_vmid, answers.cp_ip
    );
    println!("  operator pk:     {}", answers.operator_pk);
    println!("  ──────────────────────────────────────────────");
    if !confirm("Proceed?", true)? {
        println!("aborted.");
        return Ok(());
    }

    // ——— the run ———
    let _ops = ensure_ops_agent()?;
    let agent_pk = freehold_core::identity::Identity::load(Path::new(OPS_DIR))?.nostr_pubkey_hex();

    let pubkey = stage_provision(&answers, &agent_pk)?;
    if let Some(pk) = pubkey {
        door_gate(&answers, &pk)?;
    }

    // Belt + suspenders: re-grant the ops agent so the door test passes even
    // on a reused package that predates the default grant.
    let p = spinner("granting the ops agent…");
    let (ok, out) = run(
        &bin("control-plane"),
        &["grant", &answers.runner, "--state-dir", STATE_DIR],
    )?;
    p.finish_and_clear();
    if !ok {
        bail!("grant failed:\n{out}");
    }
    println!("  ✓ ops agent granted on {}", answers.runner);

    let serve_pid = stage_serve(&answers)?;
    stage_verify_door(&answers)?;
    stage_bootstrap(&answers, "relay", answers.relay_vmid, &answers.relay_ip)?;
    stage_bootstrap(&answers, "cp", answers.cp_vmid, &answers.cp_ip)?;
    stage_deploy_relay(&answers)?;
    stage_deploy_cp(&answers)?;

    if let Some(rpk) = relay_pubkey_nip11(&answers.domain) {
        println!("  ✓ relay signing key: {rpk}");
        println!("    (use it as --relay-pubkey when serving runners against the relay)");
    } else {
        println!(
            "  (couldn't read the relay's signing key via NIP-11 — read it from the relay's data dir when you need --relay-pubkey)"
        );
    }

    // Serve PID: not tracked across the detached spawn in this pass; the
    // summary reads it from the log path convention instead.
    summary(&answers, &serve_pid);
    Ok(())
}
