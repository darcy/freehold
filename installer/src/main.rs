//! freehold-install — the standalone (dialoguer) front-end over the shared
//! bring-up stages (freehold_installer::stages). The `freehold` TUI's
//! bootstrap mode drives the same stages with its own widgets.

use anyhow::{Result, bail};
use dialoguer::{Confirm, Input, Password, Select, theme::ColorfulTheme};
use freehold_installer::config;
use freehold_installer::{
    Answers, DoorProbe, ensure_bins, mint_identity, ops_pubkey, print_tail, relay_pubkey_nip11,
    stage_bootstrap, stage_deploy_cp, stage_deploy_relay, stage_grant, stage_provision,
    stage_serve, verify_door_once,
};
use indicatif::{ProgressBar, ProgressStyle};
use std::path::Path;
use std::time::Duration;

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

fn confirm(prompt: &str, default: bool) -> Result<bool> {
    Ok(Confirm::with_theme(&ColorfulTheme::default())
        .with_prompt(prompt)
        .default(default)
        .interact()?)
}

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

    let mut a = Answers::defaults();

    a.host = ask("Proxmox host (address the runner will SSH into)", a.host)?;
    a.runner = ask("Runner name", a.runner)?;
    a.serve = ask("Runner MCP address (loopback)", a.serve)?;
    a.domain = ask(
        "Relay domain (must resolve to your host — the identity gate)",
        a.domain,
    )?;
    // vmids + ips are auto-picked/assigned (stored in the config after boot).
    a.rootfs_gb = ask("LXC rootfs size (GB)", a.rootfs_gb)?;
    a.memory_mb = ask("LXC memory (MB)", a.memory_mb)?;

    println!();
    let have_key = Select::with_theme(&ColorfulTheme::default())
        .with_prompt("Operator identity")
        .item("I have a Nostr key already (paste npub or hex)")
        .item("Generate one for me (an identity dir we keep for you)")
        .default(0)
        .interact()?;

    if have_key == 0 {
        let npub: String = Input::with_theme(&ColorfulTheme::default())
            .with_prompt("Your Nostr public key (npub1… or 64 hex)")
            .interact_text()?;
        a.operator_pk = freehold_core::identity::parse_pubkey_input(&npub)
            .map_err(|e| anyhow::anyhow!("invalid pubkey: {e}"))?;

        // Optionally persist THEIR nsec: encoded into the same 0600 identity
        // file the generated path writes, so every local launch (TUI +
        // console-login) logs into the console automatically. The key never
        // leaves this machine.
        let nsec: String = Password::with_theme(&ColorfulTheme::default())
            .with_prompt(
                "Your nsec (nsec1… — optional: persist your login key here so the TUI/console-login work with no pasting; empty = skip)",
            )
            .allow_empty_password(true)
            .interact()?;
        if !nsec.trim().is_empty() {
            let secret = freehold_core::identity::nsec_to_secret(&nsec)
                .map_err(|e| anyhow::anyhow!("invalid nsec: {e}"))?;
            let id = freehold_core::identity::Identity::from_nostr_secret(secret)
                .map_err(|e| anyhow::anyhow!("invalid nsec: {e}"))?;
            // wipe the dialoguer buffer — a pasted nsec must not linger.
            let mut wiped = nsec;
            zeroize::Zeroize::zeroize(&mut wiped);
            if id.nostr_pubkey_hex() != a.operator_pk {
                bail!(
                    "the nsec's pubkey {} does not match the pubkey you pasted ({}) — fix one",
                    id.nostr_pubkey_hex(),
                    a.operator_pk
                );
            }
            let dir = freehold_installer::operator_dir();
            if dir.join(freehold_core::identity::IDENTITY_FILE).exists() {
                bail!(
                    "an operator identity already exists at {} — remove it or reuse that key",
                    dir.display()
                );
            }
            id.write_to_dir(&dir)?;
            a.operator_generated = true;
            a.operator_dir = dir;
            println!();
            println!("  Persisted your key (0600 — it stays on this machine):");
            println!("    {}", a.operator_dir.display());
            println!("  Every local launch (TUI or console-login) now logs in automatically.");
        }
    } else {
        let dir = freehold_installer::operator_dir();
        let id = mint_identity(&dir)?;
        a.operator_pk = id.nostr_pubkey_hex();
        a.operator_generated = true;
        a.operator_dir = dir;
        println!();
        println!("  Generated a fresh identity for you:");
        println!("    pubkey: {}", a.operator_pk);
        println!(
            "    stored: {} (0600 — this IS your key, keep it safe)",
            a.operator_dir.display()
        );
        println!("  You'll log into the console with it (no secrets on screen).");
    }

    Ok(a)
}

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

fn verify_loop(a: &Answers) -> Result<()> {
    let mut failures = 0u32;
    loop {
        let p = spinner("testing the SSH door through the runner…");
        let probe = verify_door_once(a)?;
        p.finish_and_clear();

        match probe {
            DoorProbe::Ok => {
                println!("  ✓ the door works — {} is reachable", a.host);
                return Ok(());
            }
            DoorProbe::AuthFailed(out) | DoorProbe::Failed(out) => {
                println!("  ✗ the exec failed (auth or otherwise)");
                print_tail(&out, 6);
                failures += 1;
                if failures >= 3 {
                    println!();
                    println!("  Still failing after {failures} tries. If this runner predates the");
                    println!(
                        "  ssh-key serialization fix, its PRIVATE key may be unloadable by the"
                    );
                    println!("  SSH client — authorized_keys edits can't help that.");
                    println!(
                        "  Fresh start:  rm -rf ./.freehold && ./target/debug/freehold-install"
                    );
                    println!();
                }
            }
        }
        let answer: String = Input::with_theme(&ColorfulTheme::default())
            .with_prompt("Fix authorized_keys on the host, then press ENTER to retry ('q' to quit)")
            .allow_empty(true)
            .interact_text()?;
        if answer.trim().eq_ignore_ascii_case("q") {
            bail!("aborted at the door check");
        }
    }
}

fn run_stage<T>(name: &str, f: impl FnOnce() -> Result<T>) -> Result<T> {
    let p = spinner(name);
    let res = f();
    p.finish_and_clear();
    res
}

fn summary(a: &Answers, serve_pid: &str, cfg_path: &Path) {
    println!();
    println!("  ╭─────────────────────────────────────────────────────────╮");
    println!("  │                    Freehold is up                      │");
    println!("  ╰─────────────────────────────────────────────────────────╯");
    println!();
    println!(
        "  relay:          https://{} (LXC {})",
        a.domain,
        a.relay_vmid.map(|v| v.to_string()).unwrap_or_default()
    );
    println!(
        "  control plane:  https://cp-{} (LXC {})",
        a.domain,
        a.cp_vmid.map(|v| v.to_string()).unwrap_or_default()
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
    println!("  config:         {}", cfg_path.display());
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

    println!();
    println!("  ───────────────── Setting up ─────────────────");
    println!("  host:            {}", answers.host);
    println!("  runner:          {} @ {}", answers.runner, answers.serve);
    println!("  domain:          {}", answers.domain);
    println!("  relay LXC:       auto vmid (dhcp ip)",);
    println!("  control-plane:   auto vmid (dhcp ip)",);
    println!("  operator pk:     {}", answers.operator_pk);
    println!("  ──────────────────────────────────────────────");
    if !confirm("Proceed?", true)? {
        println!("aborted.");
        return Ok(());
    }

    let agent_pk = ops_pubkey()?;
    let pubkey = run_stage("provisioning the runner…", || {
        stage_provision(&answers, &agent_pk)
    })?;
    if let Some(pk) = pubkey {
        door_gate(&answers, &pk)?;
    }

    run_stage("granting the ops agent…", || stage_grant(&answers))?;
    println!("  ✓ ops agent granted on {}", answers.runner);
    let serve_pid = run_stage("starting the runner in the background…", || {
        stage_serve(&answers)
    })?;
    verify_loop(&answers)?;
    // Phase 0.12 durable plane: resolve/ensure the storage backend FIRST so
    // the relay/CP/k3s guests are BORN with their dataset mounts. The
    // interactive front-end asks consent (creating a backend is a
    // destructive host mutation — gated like teardown); the non-interactive
    // pipeline passes the front-end's answer as the bool.
    run_stage("resolving the durable volume plane…", || {
        let consent = dialoguer::Confirm::new()
            .with_prompt(
                "No existing storage backend — create one (ZFS/LVM-thin)? \
                 this carves/relabels host storage",
            )
            .default(false)
            .interact()
            .unwrap_or(false);
        freehold_installer::stage_storage(&answers, consent)
    })?;
    run_stage("booting the relay LXC…", || {
        stage_bootstrap(&answers, "relay", answers.relay_vmid)
    })?;
    run_stage("booting the cp LXC…", || {
        stage_bootstrap(&answers, "cp", answers.cp_vmid)
    })?;
    run_stage("deploying the Buzz relay…", || {
        stage_deploy_relay(&answers)
    })?;
    run_stage("deploying the control plane…", || {
        stage_deploy_cp(&answers)
    })?;

    if let Some(rpk) = relay_pubkey_nip11(&answers.domain) {
        println!("  ✓ relay signing key: {rpk}");
    } else {
        println!(
            "  (relay signing key unreadable via NIP-11 — read it from the relay's data dir when you need --relay-pubkey)"
        );
    }

    let cfg_path = config::Config::default_path();
    let cfg = config::Config::from_answers(&answers);
    cfg.save(&cfg_path)?;
    println!("  ✓ wrote config {}", cfg_path.display());

    summary(&answers, &serve_pid, &cfg_path);
    Ok(())
}
