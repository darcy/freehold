//! `freehold teardown` — destroy the managed world.
//!
//! ORDER MATTERS (each step verified before the next):
//!   1. the door must prove itself (an exec through the runner) — NOTHING
//!      remote happens without it;
//!   2. destroy the managed LXCs (relay + cp; the compose deployments live
//!      inside them, so destroying the guest removes the deployment + data);
//!   3. REMOVE THE RUNNER'S KEY from the host's authorized_keys — the LAST
//!      mutation of the host, after every other remote action succeeded
//!      (the runner IS the SSH client, so this must precede stopping it) —
//!      then VERIFY the line is gone;
//!   4. local cleanup: stop the runner serve, remove `~/.freehold`, remove
//!      the config file.
//!
//! Scoped to `cfg.managed`: a relay we were INVITED to is not ours to
//! destroy. Operator-key authorization is DEFERRED (per plan) — the prompt
//! is the gate today; the auth seam is the `confirm` passed in by the CLI.

use crate::config::Config;
use crate::{Answers, DoorProbe, bin, port_open, runner_pkgs, verify_door_once};
use anyhow::{Context, Result, bail};

/// Everything the CLI needs to show before it asks for confirmation.
pub struct Plan {
    pub domain: String,
    pub lxcs: Vec<(String, u32)>,
    pub managed: Vec<String>,
    pub world_home: std::path::PathBuf,
    pub config_path: std::path::PathBuf,
    pub door_target: String,
}

/// Compute what teardown would destroy — the CLI prints this, then asks.
pub fn plan(config_path: &std::path::Path) -> Result<Option<Plan>> {
    let Some(cfg) = Config::load(config_path)? else {
        return Ok(None);
    };
    let lxcs = cfg
        .managed
        .iter()
        .filter(|m| m.as_str() == "relay" || m.as_str() == "cp")
        .filter_map(|m| {
            let vmid = if m == "relay" {
                cfg.lxc.relay.vmid
            } else {
                cfg.lxc.cp.vmid
            };
            vmid.map(|v| (m.clone(), v))
        })
        .collect();
    Ok(Some(Plan {
        domain: cfg.domain.clone(),
        lxcs,
        managed: cfg.managed.clone(),
        world_home: crate::freehold_home(),
        config_path: config_path.to_path_buf(),
        door_target: cfg.runner.target.clone(),
    }))
}

/// Execute the teardown. `confirm` is the CLI's authorization gate (typed
/// "yes" today; the operator-signature check slots in there later).
pub fn run(config_path: &std::path::Path, confirm: bool) -> Result<String> {
    let Some(cfg) = Config::load(config_path)? else {
        return Ok("nothing to tear down — no config".to_string());
    };
    if !confirm {
        bail!("teardown aborted (not confirmed)");
    }
    let a = Answers::from_config(&cfg);
    let mut log: Vec<String> = Vec::new();

    // 1. the door must work before anything remote.
    if !port_open(&a.serve) {
        crate::stage_serve(&a)?;
    }
    match verify_door_once(&a)? {
        DoorProbe::Ok => {}
        DoorProbe::AuthFailed(t) | DoorProbe::Failed(t) => bail!(
            "teardown won't touch the host: the door can't be verified — \
             fix/start the runner first (tail: {})",
            t.lines().last().unwrap_or("")
        ),
    }
    log.push(format!("door verified ({})", cfg.runner.target));

    // 2. destroy the managed LXCs (each checked present → stopped → destroyed).
    for (role, vmid) in [("relay", cfg.lxc.relay.vmid), ("cp", cfg.lxc.cp.vmid)] {
        if !cfg.managed.iter().any(|m| m == role) {
            let label = vmid.map(|v| v.to_string()).unwrap_or_else(|| "?".into());
            log.push(format!("skipped {role} LXC {label} (not managed)"));
            continue;
        }
        let Some(vmid) = vmid else {
            log.push(format!("{role} LXC: never created (no vmid recorded)"));
            continue;
        };
        if !lxc_exists(&a, vmid)? {
            log.push(format!("{role} LXC {vmid}: already gone"));
            continue;
        }
        let status = exec_pct(&a, &format!("pct status {vmid}"))?;
        if status.contains("status: running") {
            exec_pct(&a, &format!("pct stop {vmid} --skiplock"))?;
        }
        exec_pct(&a, &format!("pct destroy {vmid}"))?;
        // verified destroyed
        if lxc_exists(&a, vmid)? {
            bail!("{role} LXC {vmid} still exists after destroy");
        }
        log.push(format!("destroyed {role} LXC {vmid}"));
    }

    // 3. THE DOOR: remove the runner's key from the host — the last host
    //    mutation — then verify it's gone (read-only).
    let key_removal = format!(
        "sed -i '/ssh-ed25519 [A-Za-z0-9+/=]* {}$/d' /root/.ssh/authorized_keys",
        a.runner
    );
    exec_pct(&a, &key_removal)?;
    let check = exec_pct(
        &a,
        &format!(
            "grep -c 'ssh-ed25519 .* {}' /root/.ssh/authorized_keys || true",
            a.runner
        ),
    )?;
    let still = check.trim().parse::<u32>().unwrap_or(1);
    if still != 0 {
        bail!("the runner's key is STILL in authorized_keys ({still} line(s)) — ");
    }
    log.push(format!(
        "door removed from {} ({} — verified)",
        a.runner, cfg.runner.target
    ));

    // 4. local cleanup.
    stop_local_serve(&a)?;
    let home = crate::freehold_home();
    if home.exists() {
        std::fs::remove_dir_all(&home).with_context(|| format!("removing {}", home.display()))?;
        log.push(format!("removed world {}", home.display()));
    }
    if config_path.exists() {
        std::fs::remove_file(config_path)
            .with_context(|| format!("removing {}", config_path.display()))?;
        log.push(format!("removed config {}", config_path.display()));
    }

    Ok(format!("teardown complete:\n  {}", log.join("\n  ")))
}

/// Does the LXC exist? via `pct list | grep -c` (exits 0 either way so a
/// GONE vmid is never mistaken for an exec failure).
fn lxc_exists(a: &Answers, vmid: u32) -> Result<bool> {
    let out = exec_pct(a, &format!("pct list | grep -c '^\\s*{vmid} ' || true"))?;
    Ok(out.trim() != "0")
}

fn exec_pct(a: &Answers, cmd: &str) -> Result<String> {
    let (ok, out) = crate::run(
        &bin("freehold-orchestrator"),
        &[
            "exec",
            "--addr",
            &a.serve,
            "--agent-dir",
            crate::ops_dir().to_str().unwrap(),
            &a.runner,
            cmd,
        ],
    )?;
    if ok {
        Ok(out)
    } else {
        bail!(
            "exec failed on {}: {}",
            a.runner,
            out.lines().last().unwrap_or("")
        )
    }
}

/// Stop the background runner serve for this package (the one stage_serve
/// started; a serve RESTARTED elsewhere with the same package is also ours
/// to stop — the door is gone, it can't serve anything anymore).
fn stop_local_serve(a: &Answers) -> Result<()> {
    let pkg = runner_pkgs().join(&a.runner);
    let pat = format!("runner serve --state-dir {}", pkg.display());
    let out = std::process::Command::new("pkill")
        .args(["-f", &pat])
        .output()
        .context("pkill the runner serve")?;
    if !out.status.success() {
        // pkill exits 1 when nothing matched — fine (already stopped).
        return Ok(());
    }
    // give the process a moment, then confirm the port closed
    std::thread::sleep(std::time::Duration::from_millis(500));
    if port_open(&a.serve) {
        bail!("the runner serve on {} is still up after pkill", a.serve);
    }
    Ok(())
}
