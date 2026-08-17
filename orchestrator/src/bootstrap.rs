//! Phase A (Chunk 2): bootstrap provisioning — CPA drives a provisioning
//! RUNNER runner-direct (no relay, no delegation) to stand up a target.
//!
//! Provisioning logic is owned by the expert (the agent-written commands the
//! runner executes verbatim); this module holds ONLY the orchestration:
//! command sequence, response parsing, and the reachability verify (A3).
//!
//! Drivers:
//! - `proxmox-lxc` — `pvesh`/`pct` exec'd through the ssh runner on the PVE
//!   host (the laptop). Template ensurement (idempotent pveam download);
//!   nested+unprivileged create; start; verify via `pct exec` (no IP needed
//!   for the self-check); docker+compose installed and verified in the guest.
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
    /// None, the flow ensures a Debian template: reuse the newest present or
    /// `pveam update` + download the newest available.
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

fn exec(
    client: &McpClient,
    target: &str,
    cmd: &str,
    timeout_s: u64,
) -> Result<ExecOutcome, BootstrapError> {
    Ok(client.exec(target, cmd, &[target], timeout_s)?)
}

pub(crate) fn expect_ok(out: &ExecOutcome, step: &str) -> Result<(), BootstrapError> {
    if out.timed_out {
        return Err(BootstrapError::Step {
            step: step.to_string(),
            exit: None,
            output: "TIMED OUT (runner watchdog killed the command)".into(),
        });
    }
    match out.exit_code {
        Some(0) => Ok(()),
        code => Err(BootstrapError::Step {
            step: step.to_string(),
            exit: code,
            output: format!("stdout: {}\nstderr: {}", out.stdout, out.stderr),
        }),
    }
}

/// Operator-supplied values are interpolated into commands the runner
/// executes. Reject anything outside a conservative safe alphabet so a value
/// can never break out of the shell or a quoted JSON body.
pub(crate) fn plain(s: &str) -> Result<(), BootstrapError> {
    if s.chars()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
    {
        Ok(())
    } else {
        Err(BootstrapError::Verify(format!(
            "unexpected characters in value {s:?} (allowed: [A-Za-z0-9._-])"
        )))
    }
}

/// A filesystem PATH variant: `/` is legitimate, everything shell-hostile
/// (`;|&$`'\"` etc.) is not — these values are interpolated into commands.
pub(crate) fn plain_path(s: &str) -> Result<(), BootstrapError> {
    if s.chars()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.' | '/'))
    {
        Ok(())
    } else {
        Err(BootstrapError::Verify(format!(
            "unexpected characters in path {s:?} (allowed: [A-Za-z0-9._-/])"
        )))
    }
}

/// `debian-12-standard_12.7-1_amd64.tar.zst` -> Some([12, 7]); None for
/// anything without a `standard` segment or a numeric version. Numeric
/// component compare (12.10 > 12.7); the `-1` build counter is dropped so
/// 12.7-2 does not outrank 12.10-1.
fn template_version(name: &str) -> Option<Vec<u32>> {
    // `debian-12-standard` is ONE underscore-separated segment; the version
    // (`12.7-1`) is the segment that follows it.
    let mut it = name.split('_');
    while let Some(seg) = it.next() {
        if seg.ends_with("standard") {
            let version = it.next()?;
            return version
                .split('.')
                .map(|part| part.split('-').next()?.parse().ok())
                .collect();
        }
    }
    None
}

/// Ensure a Debian LXC template exists in the PVE `local` directory store,
/// downloading the newest available one when missing. Idempotent: a present
/// template is reused, and `pveam update` is only run when the local store
/// has none. The template store is `local` (not the container's `storage`):
/// `pct create` addresses templates as `local:vztmpl/<name>`. pvesm/pveam
/// take NO `--output-format` (verified against PVE 9.2) — plain tables are
/// parsed.
fn ensure_debian_template(
    client: &McpClient,
    target: &str,
    spec_template: &Option<String>,
) -> Result<String, BootstrapError> {
    const TEMPLATE_STORAGE: &str = "local";
    // Operator pinned a template: use it verbatim, no download.
    if let Some(t) = spec_template {
        plain(t)?;
        return Ok(t.clone());
    }

    // pvesm plain table: header + rows of `<volid> <format> <type> <size> <vmid>`.
    let list = exec(
        client,
        target,
        &format!("pvesm list {TEMPLATE_STORAGE}"),
        120,
    )?;
    expect_ok(&list, "template list")?;
    let present = list
        .stdout
        .lines()
        .skip(1) // header
        .filter_map(|line| {
            let volid = line.split_whitespace().next()?;
            volid
                .strip_prefix(&format!("{TEMPLATE_STORAGE}:vztmpl/"))
                .map(str::to_string)
        })
        .filter(|n| n.contains("debian-") && n.contains("standard_"))
        .filter_map(|n| template_version(&n).map(|v| (v, n)));
    if let Some((_, name)) = present.clone().max_by(|a, b| a.0.cmp(&b.0)) {
        return Ok(name);
    }

    // None in the store: sync the catalog and download the newest Debian
    // standard template. Update runs first on purpose — the local catalog may
    // predate the template the user wants.
    let update = exec(client, target, "pveam update", 180)?;
    expect_ok(&update, "pveam update")?;
    let available = exec(client, target, "pveam available --section system", 120)?;
    expect_ok(&available, "pveam available")?;
    let name = available
        .stdout
        .lines()
        .filter_map(|line| line.split_whitespace().next().map(str::to_string))
        .filter(|n| {
            n.contains("debian-")
                && n.contains("standard_")
                && (n.ends_with(".tar.zst") || n.ends_with(".tar.gz"))
        })
        .filter_map(|n| template_version(&n).map(|v| (v, n)))
        .max_by(|a, b| a.0.cmp(&b.0))
        .map(|(_, n)| n)
        .ok_or_else(|| {
            BootstrapError::Verify(
                "no Debian LXC template available — run `pveam update` on the PVE host and \
                 check its internet access"
                    .into(),
            )
        })?;
    let download = exec(
        client,
        target,
        &format!("pveam download {TEMPLATE_STORAGE} {name}"),
        600,
    )?;
    expect_ok(&download, "pveam download")?;
    Ok(name)
}

/// `pct create` + `pct start` + `pct exec` verify — the proxmox-lxc driver.
pub async fn bootstrap_proxmox_lxc(
    client: &McpClient,
    target: &str,
    spec: &ProxmoxLxcSpec,
) -> Result<BootstrapResult, BootstrapError> {
    // Template ensurement: reuse the newest Debian template already in the
    // store; only when none is present, sync the catalog (`pveam update`) and
    // download the newest available. Idempotent — a re-run with a template
    // present makes NO pveam calls.
    let tpl = ensure_debian_template(client, target, &spec.template)?;

    plain(&spec.hostname)?;
    if let Some(t) = &spec.template {
        plain(t)?;
    }
    plain(&spec.storage)?;
    plain(&spec.bridge)?;

    // Bounds check the vmid (PVE convention: systems are 100+).
    if spec.vmid < 100 {
        return Err(BootstrapError::Verify(format!(
            "vmid {} is below the PVE system range (100+); pick a free id",
            spec.vmid
        )));
    }

    // --unprivileged 1 is explicit, not the CLI default: pct's CLI defaults
    // to PRIVILEGED, and this container will host the relay + control plane
    // (runner ciphertext, relay membership) — a root escape inside it must
    // not land as host root on the PVE machine. Nothing downstream needs
    // privileged (A3 verifies via `pct exec` through the host).
    // --features nesting=1 is what lets dockerd (overlay2) run inside the
    // unprivileged container — without it docker-in-LXC fails at the mount
    // namespace boundary.
    let create = format!(
        "pct create {vmid} local:vztmpl/{tpl} --storage {storage} --hostname {host} \
         --unprivileged 1 --features nesting=1 --net0 name=eth0,bridge={bridge},ip=dhcp",
        vmid = spec.vmid,
        tpl = tpl,
        storage = spec.storage,
        host = spec.hostname,
        bridge = spec.bridge,
    );
    let out = exec(client, target, &create, 120)?;
    expect_ok(&out, "pct create")?;

    let start = format!("pct start {vmid}", vmid = spec.vmid);
    let out = exec(client, target, &start, 120)?;
    expect_ok(&out, "pct start")?;

    // A3 — reachability self-check: the runner asks the guest, through the
    // host, `pct exec`; the guest answers. No IP guessing.
    let verify = format!(
        "pct exec {vmid} -- sh -c 'hostname && uname -s && whoami'",
        vmid = spec.vmid
    );
    let out = exec(client, target, &verify, 120)?;
    expect_ok(&out, "pct exec verify")?;
    if !out.stdout.contains("Linux") || !out.stdout.trim().contains(&spec.hostname) {
        return Err(BootstrapError::Verify(format!(
            "guest did not answer as expected (hostname {hostname}): {out}",
            hostname = spec.hostname,
            out = out.stdout
        )));
    }

    // Get ahead of docker-in-LXC: the guest hosts the relay, so it needs
    // docker + compose BEFORE Phase B's deploy gate runs.
    ensure_guest_docker(client, target, spec.vmid)?;

    Ok(BootstrapResult {
        kind: TargetKind::ProxmoxLxc,
        id: spec.vmid.to_string(),
        name: spec.hostname.clone(),
        detail: format!(
            "lxc {vmid} ({hostname}) created from {tpl} on {storage}, started, and the \
             guest verified via `pct exec` (kernel {kernel}); docker+compose ready",
            vmid = spec.vmid,
            hostname = spec.hostname,
            tpl = tpl,
            storage = spec.storage,
            kernel = out.stdout.lines().nth(1).unwrap_or("?")
        ),
    })
}

/// Ensure docker + compose run inside the guest. Install-if-missing is one
/// `pct exec` (baked to skip when already present — idempotent re-runs; the
/// apt run is the slow path: 600s). The daemon must then answer `docker
/// info`; when the default overlay2 driver fails inside the unprivileged
/// container, fall back to fuse-overlayfs (the canonical fix), restart, and
/// re-verify. A second failure surfaces the raw output with a hint.
fn ensure_guest_docker(client: &McpClient, target: &str, vmid: u32) -> Result<(), BootstrapError> {
    let install = format!(
        "pct exec {vmid} -- sh -c 'DEBIAN_FRONTEND=noninteractive; \
         if ! docker compose version >/dev/null 2>&1; then \
         apt-get update >/dev/null && \
         apt-get install -y docker.io docker-compose-plugin >/dev/null; fi; \
         docker compose version'",
        vmid = vmid
    );
    let out = exec(client, target, &install, 600)?;
    expect_ok(&out, "guest docker install")?;

    let info = format!("pct exec {vmid} -- docker info", vmid = vmid);
    // A non-zero exit arrives as Ok(outcome), so BOTH a transport error and a
    // failing exec must fall through to the overlay fallback.
    let first = match exec(client, target, &info, 120) {
        Ok(out) => match expect_ok(&out, "guest docker info") {
            Ok(()) if out.stdout.contains("Server Version") => return Ok(()),
            Ok(()) => {
                BootstrapError::Verify("guest docker info answered without 'Server Version'".into())
            }
            Err(e) => e,
        },
        Err(e) => e,
    };

    // overlay2 can fail to mount inside this unprivileged/NESTED guest:
    // fuse-overlayfs is the drop-in storage driver for exactly that.
    let fallback = format!(
        "pct exec {vmid} -- sh -c 'apt-get install -y fuse-overlayfs >/dev/null && \
         mkdir -p /etc/docker && \
         printf \"{{\\\"storage-driver\\\":\\\"fuse-overlayfs\\\"}}\\n\" \
         > /etc/docker/daemon.json && \
         systemctl restart docker >/dev/null 2>&1 || \
         service docker restart >/dev/null 2>&1'",
        vmid = vmid
    );
    let out = exec(client, target, &fallback, 600)?;
    expect_ok(&out, "guest docker fuse fallback")?;

    match exec(client, target, &info, 120) {
        Ok(out) => match expect_ok(&out, "guest docker info (after fallback)") {
            Ok(()) if out.stdout.contains("Server Version") => Ok(()),
            Ok(()) => Err(BootstrapError::Verify(format!(
                "docker daemon failed in the guest after the fuse-overlayfs fallback \
                 ({first}); consider a privileged container"
            ))),
            Err(second) => Err(BootstrapError::Verify(format!(
                "docker daemon failed in the guest after the fuse-overlayfs fallback \
                 ({first}; re-check: {second}); consider a privileged container"
            ))),
        },
        Err(second) => Err(BootstrapError::Verify(format!(
            "docker daemon failed in the guest after the fuse-overlayfs fallback \
             ({first}; re-check: {second}); consider a privileged container"
        ))),
    }
}

/// The vultr-vps driver — the proven curl shapes from C2/G2.2, wrapped in
/// the bootstrap orchestration (create → poll active → report; optional
/// destroy for tests/cleanup).
pub async fn bootstrap_vultr_vps(
    client: &McpClient,
    target: &str,
    spec: &VultrVpsSpec,
) -> Result<BootstrapResult, BootstrapError> {
    plain(&spec.label)?;
    plain(&spec.region)?;
    plain(&spec.plan)?;
    // The runner injects the credential as <ENV> and the base URL as
    // <ENV>_URL, where ENV derives from the TARGET's name (exec::env_name:
    // alnum upcased, everything else '_'). Derive here too, so the driver
    // works for any target name — not just literals named `vultr`.
    let env = target
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() {
                c.to_ascii_uppercase()
            } else {
                '_'
            }
        })
        .collect::<String>();
    // Shell tokens; the substituted values are strings, so no format-brace
    // conflict: the shell expands ${BOX} / ${BOX_URL} at exec time.
    let env_cred = format!("${{{env}}}");
    let env_url = format!("${{{}_URL}}", env);
    let create = format!(
        "curl -sS -X POST \"{env_url}/v2/instances\" -H \"Authorization: Bearer {env_cred}\" \
         -H 'Content-Type: application/json' -d '{{\"region\":\"{region}\",\"plan\":\"{plan}\",\
         \"os_id\":{os_id},\"label\":\"{label}\"}}'",
        env_url = env_url,
        env_cred = env_cred,
        region = spec.region,
        plan = spec.plan,
        os_id = spec.os_id,
        label = spec.label,
    );
    let out = exec(client, target, &create, 120)?;
    expect_ok(&out, "vultr create")?;
    let created: Value = serde_json::from_str(out.stdout.trim())
        .map_err(|e| BootstrapError::Verify(format!("create parse: {e}: {}", out.stdout)))?;
    let id = created["instance"]["id"]
        .as_str()
        .ok_or_else(|| BootstrapError::Verify(format!("no instance id: {}", out.stdout)))?
        .to_string();

    // Poll until the instance is reachable (bounded; the mock is instant, a
    // real Vultr box takes a couple of minutes). 'active' alone is NOT
    // enough: Vultr reports 0.0.0.0 until the IP is assigned. destroy_after
    // ALWAYS destroys — including on every post-create failure — so a test or
    // a bad run can never leak a billed instance.
    let mut detail: Option<String> = None;
    let mut verified = false;
    let mut poll_err: Option<String> = None;
    'poll: for _ in 0..60 {
        let poll = format!(
            "curl -sS \"{env_url}/v2/instances/{id}\" -H \"Authorization: Bearer {env_cred}\"",
            env_url = env_url,
            env_cred = env_cred,
            id = id
        );
        match exec(client, target, &poll, 120).and_then(|out| {
            expect_ok(&out, "vultr poll")?;
            let status: Value = serde_json::from_str(out.stdout.trim())
                .map_err(|e| BootstrapError::Verify(format!("poll parse: {e}")))?;
            let instance = &status["instance"];
            let state = instance["status"].as_str().unwrap_or("");
            let ip = instance["main_ip"].as_str().unwrap_or("");
            if state == "active" && !ip.is_empty() && ip != "0.0.0.0" {
                detail = Some(format!(
                    "vultr instance {id} active; main_ip {ip}; label {}",
                    spec.label
                ));
                verified = true;
            }
            Ok(())
        }) {
            Ok(()) => {
                if verified {
                    break 'poll;
                }
                // a successful round clears any earlier transient error
                poll_err = None;
            }
            Err(e) => poll_err = Some(e.to_string()),
        }
        tokio::time::sleep(std::time::Duration::from_secs(2)).await;
    }
    let mut detail = match detail {
        Some(d) => d,
        None => match poll_err {
            // destroy (if requested) still runs below, before we return.
            Some(e) => format!("polling failed: {e}"),
            None => format!("instance {id} did not become reachable within the poll window"),
        },
    };

    if spec.destroy_after {
        // --fail: curl exits non-zero on any HTTP >= 400, so a 401/404/500
        // destroy can NEVER report "; destroyed" while the instance lives.
        // Destroy is the one step whose silent failure costs money. Check the
        // HTTP code explicitly: curl exits 0 on many transport-time errors and
        // a body-less `-o /dev/null` cannot otherwise be trusted.
        let destroy = format!(
            "curl -sS -X DELETE \"{env_url}/v2/instances/{id}\" \
             -H \"Authorization: Bearer {env_cred}\" -o /dev/null -w '%{{http_code}}'",
            env_url = env_url,
            env_cred = env_cred,
            id = id
        );
        let out = exec(client, target, &destroy, 120)?;
        expect_ok(&out, "vultr destroy")?;
        let code = out.stdout.trim();
        if !code.starts_with('2') {
            return Err(BootstrapError::Verify(format!(
                "destroy of instance {id} returned HTTP {code} — the instance may still be running"
            )));
        }
        detail.push_str("; destroyed");
    }

    if !verified {
        return Err(BootstrapError::Verify(detail));
    }

    Ok(BootstrapResult {
        kind: TargetKind::VultrVps,
        id,
        name: spec.label.clone(),
        detail,
    })
}

#[cfg(test)]
mod tests {
    use super::template_version;

    #[test]
    fn template_version_parses_numeric_components() {
        assert_eq!(
            template_version("debian-12-standard_12.7-1_amd64.tar.zst"),
            Some(vec![12, 7])
        );
        assert_eq!(
            template_version("debian-12-standard_12.10-1_amd64.tar.zst"),
            Some(vec![12, 10])
        );
        assert_eq!(
            template_version("debian-13-standard_13.2-1_amd64.tar.zst"),
            Some(vec![13, 2])
        );
    }

    #[test]
    fn template_version_rejects_versionless_or_garbage() {
        // no `standard` segment at all
        assert_eq!(
            template_version("debian-12-custom_12.7-1_amd64.tar.zst"),
            None
        );
        // version segment not numeric
        assert_eq!(
            template_version("debian-12-standard_x.y-1_amd64.tar.zst"),
            None
        );
        // suffix variant parses the same
        assert_eq!(
            template_version("debian-12-standard_12.7-2_amd64.tar.gz"),
            Some(vec![12, 7])
        );
        assert_eq!(template_version(""), None);
    }
}
