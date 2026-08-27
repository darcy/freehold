//! `freehold teardown` — destroy the managed world.
//!
//! THREE scopes (Phase 0.12):
//!   - **Whole-world** (default): destroy every managed LXC + remove the
//!     runner's door + local cleanup (world home + config). Optional `--data`
//!     ALSO destroys every tenant's dataset subtree.
//!   - **Per-tenant compute-only**: destroy ONE tenant's LXC; the dataset is
//!     untouched and the CONFIG SURVIVES (it holds the coords + the
//!     tenant→dataset mapping for reattach).
//!   - **Per-tenant data+compute**: compute-only PLUS that tenant's dataset
//!     subtree is destroyed (full intended loss, scoped to exactly the named
//!     tenant by the per-tenant model).
//!
//! ORDER MATTERS (each step verified before the next):
//!   1. the door must prove itself (an exec through the runner) — NOTHING
//!      remote happens without it;
//!   2. destroy the targeted LXC(s) (compose deployments live inside them);
//!   3. (whole-world only) REMOVE THE RUNNER'S KEY from the host's
//!      authorized_keys — the LAST host mutation — then verify it's gone;
//!   4. (whole-world only) local cleanup: stop the serve, remove `~/.freehold`
//!      + the config. Per-tenant scopes KEEP the config + local home.
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
        .filter(|m| m.as_str() == "relay" || m.as_str() == "cp" || m.as_str() == "k3s")
        .filter_map(|m| {
            let vmid = match m.as_str() {
                "relay" => cfg.lxc.relay.vmid,
                "cp" => cfg.lxc.cp.vmid,
                "k3s" => cfg.lxc.k3s.vmid,
                _ => None,
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

/// The three teardown scopes (Phase 0.12).
#[derive(Debug, Clone, PartialEq, Eq)]
enum Scope {
    /// Every managed LXC + the config + local home. Optional data: also
    /// destroy every tenant dataset subtree.
    WholeWorld { data: bool },
    /// ONE tenant's LXC (compute-only): the dataset is untouched, the
    /// config SURVIVES (coords + mapping for reattach).
    TenantCompute { tenant: String },
    /// ONE tenant's LXC + its dataset subtree (data+compute): full intended
    /// loss, scoped by construction.
    TenantData { tenant: String },
}

/// Execute the teardown. `confirm` is the CLI's authorization gate (typed
/// "yes" today; the operator-signature check slots in there later).
pub fn run(config_path: &std::path::Path, confirm: bool) -> Result<String> {
    run_scoped(config_path, None, false, confirm)
}

/// Execute the teardown at an optionally tenant-scoped + data granularity.
pub fn run_scoped(
    config_path: &std::path::Path,
    tenant: Option<&str>,
    data: bool,
    confirm: bool,
) -> Result<String> {
    run_scope(config_path, scope_for(tenant, data), confirm)
}

/// The single source of truth for the three-scope derivation (used by both
/// `run_scoped` and the tests — a regression in the real mapping must fail
/// the tests, not be mirrored by a copy).
fn scope_for(tenant: Option<&str>, data: bool) -> Scope {
    match tenant {
        Some(t) if data => Scope::TenantData { tenant: t.into() },
        Some(t) => Scope::TenantCompute { tenant: t.into() },
        None => Scope::WholeWorld { data },
    }
}

fn run_scope(config_path: &std::path::Path, scope: Scope, confirm: bool) -> Result<String> {
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

    // 2. destroy the targeted LXC(s).
    match &scope {
        Scope::WholeWorld { .. } => {
            let guests = [
                ("relay", cfg.lxc.relay.vmid),
                ("cp", cfg.lxc.cp.vmid),
                ("k3s", cfg.lxc.k3s.vmid),
            ];
            for (role, vmid) in guests {
                if !cfg.managed.iter().any(|m| m == role) {
                    let label = vmid.map(|v| v.to_string()).unwrap_or_else(|| "?".into());
                    log.push(format!("skipped {role} LXC {label} (not managed)"));
                    continue;
                }
                log.extend(destroy_one_lxc(&a, role, vmid)?);
            }
        }
        Scope::TenantCompute { tenant } | Scope::TenantData { tenant } => {
            let role = tenant_lxc_role(tenant);
            let vmid = match role {
                "relay" => cfg.lxc.relay.vmid,
                "cp" => cfg.lxc.cp.vmid,
                "k3s" => cfg.lxc.k3s.vmid,
                _ => bail!("unknown tenant {tenant:?} (relay | cp | k3s-volumes)"),
            };
            log.extend(destroy_one_lxc(&a, role, vmid)?);
        }
    }

    // 3. optionally destroy the tenant dataset subtree (data+compute), or
    //    all of them on whole-world --data.
    match &scope {
        Scope::WholeWorld { data: true } | Scope::TenantData { .. } => {
            let tenants: Vec<String> = match &scope {
                Scope::TenantData { tenant } => vec![tenant.clone()],
                _ => vec!["relay".into(), "cp".into(), "k3s-volumes".into()],
            };
            for tenant in tenants {
                // The two-place rule's second place (independent): re-derive
                // `<pool>/freehold/<domain-dashes>/<tenant>` from the backend
                // + naming convention, so a tampered/missing recorded mapping
                // can't silently skip a data+compute destroy.
                let dataset = dataset_path_for(&cfg, &tenant, &cfg.domain);
                log.push(destroy_dataset(&a, &cfg, &tenant, &dataset)?);
            }
        }
        _ => {}
    }

    // 4. whole-world: THE DOOR + local cleanup. Per-tenant scopes KEEP the
    //    local home + config (coords + mapping for reattach).
    if matches!(scope, Scope::WholeWorld { .. }) {
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

        stop_local_serve(&a)?;
        let home = crate::freehold_home();
        if home.exists() {
            std::fs::remove_dir_all(&home)
                .with_context(|| format!("removing {}", home.display()))?;
            log.push(format!("removed world {}", home.display()));
        }
        if config_path.exists() {
            std::fs::remove_file(config_path)
                .with_context(|| format!("removing {}", config_path.display()))?;
            log.push(format!("removed config {}", config_path.display()));
        }
    } else {
        log.push(
            "config KEPT (per-tenant teardown — coords + dataset mapping for reattach)".to_string(),
        );
    }

    Ok(format!("teardown complete:\n  {}", log.join("\n  ")))
}

/// Map a tenant name to the LXC role it rides (k3s-volumes rides the k3s
/// guest). Empty for unknown tenants.
fn tenant_lxc_role(tenant: &str) -> &'static str {
    match tenant {
        "k3s-volumes" | "k3s" => "k3s",
        "relay" => "relay",
        "cp" => "cp",
        _ => "",
    }
}

/// Destroy one LXC (checked present → stopped → destroyed → verified gone).
fn destroy_one_lxc(a: &Answers, role: &str, vmid: Option<u32>) -> Result<Vec<String>> {
    let mut log = Vec::new();
    let Some(vmid) = vmid else {
        log.push(format!("{role} LXC: never created (no vmid recorded)"));
        return Ok(log);
    };
    if !lxc_exists(a, vmid)? {
        log.push(format!("{role} LXC {vmid}: already gone"));
        return Ok(log);
    }
    let status = exec_pct(a, &format!("pct status {vmid}"))?;
    if status.contains("status: running") {
        exec_pct(a, &format!("pct stop {vmid} --skiplock"))?;
    }
    exec_pct(a, &format!("pct destroy {vmid}"))?;
    if lxc_exists(a, vmid)? {
        bail!("{role} LXC {vmid} still exists after destroy");
    }
    log.push(format!("destroyed {role} LXC {vmid}"));
    Ok(log)
}

/// Destroy a tenant's dataset subtree through the runner (data+compute).
fn destroy_dataset(a: &Answers, cfg: &Config, tenant: &str, dataset: &str) -> Result<String> {
    let pool = cfg.plane.backend.clone().unwrap_or_else(|| "rpool".into());
    let (ok, out) = crate::run(
        &bin("freehold-orchestrator"),
        &[
            "storage",
            "destroy",
            "--addr",
            &a.serve,
            "--agent-dir",
            crate::ops_dir().to_str().unwrap(),
            "--target",
            &a.runner,
            "--tenant",
            tenant,
            "--domain",
            &cfg.domain,
            "--pool",
            &pool,
        ],
    )?;
    if !ok {
        bail!("dataset destroy for {tenant} failed:\n{out}");
    }
    Ok(format!("destroyed {tenant} dataset subtree ({dataset})"))
}

/// Re-derive a tenant's dataset from the two-place rule (independent of the
/// recorded mapping): `<pool>/freehold/<domain-with-dashes>/<tenant>`.
/// This is what makes the mapping recoverable from the volume listing alone.
fn dataset_path_for(cfg: &Config, tenant: &str, domain: &str) -> String {
    let pool = cfg.plane.backend.clone().unwrap_or_else(|| "rpool".into());
    let dom = domain.replace('.', "-");
    format!("{pool}/freehold/{dom}/{tenant}")
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn default_is_whole_world() {
        assert_eq!(scope_for(None, false), Scope::WholeWorld { data: false });
        assert_eq!(scope_for(None, true), Scope::WholeWorld { data: true });
    }

    #[test]
    fn tenant_scoped_stays_compute_only_without_data() {
        assert_eq!(
            scope_for(Some("relay"), false),
            Scope::TenantCompute {
                tenant: "relay".into()
            }
        );
    }

    #[test]
    fn tenant_data_adds_dataset_destroy() {
        assert_eq!(
            scope_for(Some("cp"), true),
            Scope::TenantData {
                tenant: "cp".into()
            }
        );
    }

    #[test]
    fn k3s_volumes_maps_to_k3s_lxc() {
        assert_eq!(tenant_lxc_role("k3s-volumes"), "k3s");
        assert_eq!(tenant_lxc_role("k3s"), "k3s");
        assert_eq!(tenant_lxc_role("relay"), "relay");
        assert_eq!(tenant_lxc_role("cp"), "cp");
        assert_eq!(tenant_lxc_role("bogus"), "");
    }
}
