//! Phase A (Chunk 2): bootstrap provisioning — CPA drives a provisioning
//! RUNNER runner-direct (no relay, no delegation) to stand up a target.
//!
//! Provisioning logic is owned by the expert (the agent-written commands the
//! runner executes verbatim); this module holds ONLY the orchestration:
//! command sequence, response parsing, and the reachability verify (A3).
//!
//! Drivers:
//! - `proxmox-lxc` — `pvesh`/`pct` exec'd through the ssh runner on the PVE
//!   host (the laptop). Template pre-check; create; start; verify via
//!   `pct exec` (no IP needed for the self-check).
//! - `vultr-vps` — the proven curl create/poll/destroy shapes through the
//!   vultr runner. Reachability at the API level (active + main_ip); the
//!   ssh leg is operator wiring (needs the VPS credential).

use serde_json::Value;
use thiserror::Error;

use crate::client::{ClientError, ExecOutcome, McpClient};

#[derive(Debug, Error)]
pub enum BootstrapError {
    #[error("client error: {0}")]
    Client(#[from] ClientError),
    #[error("step '{step}' failed (exit {exit:?}): {output}")]
    Step {
        step: String,
        exit: Option<i32>,
        output: String,
    },
    #[error("verify failed: {0}")]
    Verify(String),
    #[error("bootstrap io: {0}")]
    Io(#[from] std::io::Error),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TargetKind {
    ProxmoxLxc,
    VultrVps,
}

#[derive(Debug, Clone)]
pub struct ProxmoxLxcSpec {
    pub hostname: String,
    pub vmid: u32,
    /// Template name (e.g. `debian-12-standard_12.7-1_amd64.tar.zst`). If
    /// None, the flow discovers the first vztmpl in `local`.
    pub template: Option<String>,
    pub storage: String,
    pub bridge: String,
}

pub struct VultrVpsSpec {
    pub label: String,
    pub region: String,
    pub plan: String,
    pub os_id: u32,
    pub destroy_after: bool,
}

/// Bootstrap result for the report.
#[derive(Debug)]
pub struct BootstrapResult {
    pub kind: TargetKind,
    pub id: String,
    pub name: String,
    pub detail: String,
}

fn exec(client: &McpClient, target: &str, cmd: &str) -> Result<ExecOutcome, BootstrapError> {
    Ok(client.exec(target, cmd, &[target], 120)?)
}

fn expect_ok(out: &ExecOutcome, step: &str) -> Result<(), BootstrapError> {
    match out.exit_code {
        Some(0) => Ok(()),
        code => Err(BootstrapError::Step {
            step: step.to_string(),
            exit: code,
            output: format!("stdout: {}\nstderr: {}", out.stdout, out.stderr),
        }),
    }
}

/// `pct create` + `pct start` + `pct exec` verify — the proxmox-lxc driver.
pub async fn bootstrap_proxmox_lxc(
    client: &McpClient,
    target: &str,
    spec: &ProxmoxLxcSpec,
) -> Result<BootstrapResult, BootstrapError> {
    // Template discovery (A2 pre-check): an LXC can't be created without a
    // vztmpl; give the operator the exact remediation instead of a raw pct
    // error.
    let tpl = match &spec.template {
        Some(t) => t.clone(),
        None => {
            // pvesm takes no --output-format (verified against the laptop's
            // PVE 9.2); parse the plain table: header + rows of
            // `<volid> <format> <type> <size> <vmid>`.
            let list = exec(client, target, "pvesm list local")?;
            expect_ok(&list, "template list")?;
            let first = list
                .stdout
                .lines()
                .skip(1) // header
                .find_map(|line| {
                    let volid = line.split_whitespace().next().unwrap_or("");
                    volid.strip_prefix("local:vztmpl/").map(str::to_string)
                });
            match first {
                Some(n) => n,
                None => {
                    return Err(BootstrapError::Verify(
                        "no LXC template in storage 'local' — run `pveam update && \
                         pveam download local <debian-12-standard_...-amd64.tar.zst>` \
                         on the PVE host first"
                            .into(),
                    ));
                }
            }
        }
    };

    // Bounds check the vmid (PVE convention: systems are 100+).
    if spec.vmid < 100 {
        return Err(BootstrapError::Verify(format!(
            "vmid {} is below the PVE system range (100+); pick a free id",
            spec.vmid
        )));
    }

    let create = format!(
        "pct create {vmid} local:vztmpl/{tpl} --storage {storage} --hostname {host} \
         --net0 name=eth0,bridge={bridge},ip=dhcp",
        vmid = spec.vmid,
        tpl = tpl,
        storage = spec.storage,
        host = spec.hostname,
        bridge = spec.bridge,
    );
    let out = exec(client, target, &create)?;
    expect_ok(&out, "pct create")?;

    let start = format!("pct start {vmid}", vmid = spec.vmid);
    let out = exec(client, target, &start)?;
    expect_ok(&out, "pct start")?;

    // A3 — reachability self-check: the runner asks the guest, through the
    // host, `pct exec`; the guest answers. No IP guessing.
    let verify = format!(
        "pct exec {vmid} -- sh -c 'hostname && uname -s && whoami'",
        vmid = spec.vmid
    );
    let out = exec(client, target, &verify)?;
    expect_ok(&out, "pct exec verify")?;
    if !out.stdout.contains("Linux") || !out.stdout.trim().contains(&spec.hostname) {
        return Err(BootstrapError::Verify(format!(
            "guest did not answer as expected (hostname {hostname}): {out}",
            hostname = spec.hostname,
            out = out.stdout
        )));
    }

    Ok(BootstrapResult {
        kind: TargetKind::ProxmoxLxc,
        id: spec.vmid.to_string(),
        name: spec.hostname.clone(),
        detail: format!(
            "lxc {vmid} ({hostname}) created from {tpl} on {storage}, started, and the \
             guest verified via `pct exec` (kernel {kernel})",
            vmid = spec.vmid,
            hostname = spec.hostname,
            tpl = tpl,
            storage = spec.storage,
            kernel = out.stdout.lines().nth(1).unwrap_or("?")
        ),
    })
}

/// The vultr-vps driver — the proven curl shapes from C2/G2.2, wrapped in
/// the bootstrap orchestration (create → poll active → report; optional
/// destroy for tests/cleanup).
pub async fn bootstrap_vultr_vps(
    client: &McpClient,
    target: &str,
    spec: &VultrVpsSpec,
) -> Result<BootstrapResult, BootstrapError> {
    let create = format!(
        "curl -sS -X POST \"$VULTR_URL/v2/instances\" -H \"Authorization: Bearer $VULTR\" \
         -H 'Content-Type: application/json' -d '{{\"region\":\"{region}\",\"plan\":\"{plan}\",\
         \"os_id\":{os_id},\"label\":\"{label}\"}}'",
        region = spec.region,
        plan = spec.plan,
        os_id = spec.os_id,
        label = spec.label,
    );
    let out = exec(client, target, &create)?;
    expect_ok(&out, "vultr create")?;
    let created: Value = serde_json::from_str(out.stdout.trim())
        .map_err(|e| BootstrapError::Verify(format!("create parse: {e}: {}", out.stdout)))?;
    let id = created["instance"]["id"]
        .as_str()
        .ok_or_else(|| BootstrapError::Verify(format!("no instance id: {}", out.stdout)))?
        .to_string();

    // Poll until the instance reports active (bounded; the mock is instant,
    // a real Vultr box takes a couple of minutes).
    let mut detail = String::new();
    for _ in 0..60 {
        let poll = format!(
            "curl -sS \"$VULTR_URL/v2/instances/{id}\" -H \"Authorization: Bearer $VULTR\"",
            id = id
        );
        let out = exec(client, target, &poll)?;
        expect_ok(&out, "vultr poll")?;
        let status: Value = serde_json::from_str(out.stdout.trim())
            .map_err(|e| BootstrapError::Verify(format!("poll parse: {e}")))?;
        let instance = &status["instance"];
        let state = instance["status"].as_str().unwrap_or("");
        if state == "active" {
            let ip = instance["main_ip"].as_str().unwrap_or("(pending)");
            detail = format!(
                "vultr instance {id} active; main_ip {ip}; label {}",
                spec.label
            );
            break;
        }
        tokio::time::sleep(std::time::Duration::from_secs(2)).await;
    }
    if detail.is_empty() {
        return Err(BootstrapError::Verify(format!(
            "instance {id} did not reach active within the poll window"
        )));
    }

    if spec.destroy_after {
        let destroy = format!(
            "curl -sS -X DELETE \"$VULTR_URL/v2/instances/{id}\" -H \"Authorization: Bearer $VULTR\" \
             -o /dev/null -w done",
            id = id
        );
        let out = exec(client, target, &destroy)?;
        expect_ok(&out, "vultr destroy")?;
        detail.push_str("; destroyed");
    }

    Ok(BootstrapResult {
        kind: TargetKind::VultrVps,
        id,
        name: spec.label.clone(),
        detail,
    })
}
