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
    /// Container vmid; None = the driver picks the lowest free id >= 100
    /// (`pct list`).
    pub vmid: Option<u32>,
    /// Template name (e.g. `debian-12-standard_12.7-1_amd64.tar.zst`). If
    /// None, the flow ensures a Debian template matching the HOST arch:
    /// reuse the newest present or `pveam update` + download the newest
    /// available.
    pub template: Option<String>,
    pub storage: String,
    /// Rootfs size in GB on `storage` (the relay stack needs room for
    /// images; the pct default of 4G is too tight).
    pub rootfs_gb: u32,
    /// RAM in MB (the compose stack — Postgres/Redis/MinIO/relay/Caddy —
    /// needs more than the pct default of 512 and OOMs otherwise).
    pub memory_mb: u32,
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
    /// The target's first global IPv4, when the driver can learn it (the
    /// A4 domain gate requires it so the DOMAIN — never the IP — is verified).
    pub ip: Option<String>,
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

pub(crate) fn exec_to_ok(
    client: &McpClient,
    target: &str,
    cmd: &str,
    step: &str,
    timeout_s: u64,
) -> Result<ExecOutcome, BootstrapError> {
    let out = exec(client, target, cmd, timeout_s)?;
    expect_ok(&out, step)?;
    Ok(out)
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
    let arch = host_arch(client, target)?;

    // pvesm plain table: header + rows of `<volid> <format> <type> <size> <vmid>`.
    let list = exec(
        client,
        target,
        &format!("pvesm list {TEMPLATE_STORAGE}"),
        120,
    )?;
    expect_ok(&list, "template list")?;
    // `_<arch>.tar.[gz|zst]` — the catalog and store mix amd64 and arm64
    // rows for the SAME release; an arm64 guest cannot spawn on x86_64
    // (observed live). Only the host's arch is a candidate.
    let arch_suffix = format!("_{arch}.tar.");
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
        .filter(|n| n.contains("debian-") && n.contains("standard_") && n.contains(&arch_suffix))
        .filter_map(|n| template_version(&n).map(|v| (v, n)));
    if let Some((_, name)) = present.clone().max_by(|a, b| a.0.cmp(&b.0)) {
        plain(&name)?;
        return Ok(name);
    }

    // None in the store: sync the catalog and download the newest Debian
    // standard template. Update runs first on purpose — the local catalog may
    // predate the template the user wants.
    let update = exec(client, target, "pveam update", 180)?;
    expect_ok(&update, "pveam update")?;
    let available = exec(client, target, "pveam available --section system", 120)?;
    expect_ok(&available, "pveam available")?;
    // Columns: `<section> <template> <size> <needs-reboot>` — the FIRST token
    // is the section (`system`), the template name is the SECOND.
    let name = available
        .stdout
        .lines()
        .filter_map(|line| line.split_whitespace().nth(1).map(str::to_string))
        .filter(|n| {
            n.contains("debian-")
                && n.contains("standard_")
                && (n.ends_with(".tar.zst") || n.ends_with(".tar.gz"))
                && n.contains(&arch_suffix)
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
    // The name came from the host's own catalog, not the operator, but it
    // still lands in a shell command — same guard as every other value.
    plain(&name)?;
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
    // download the newest available. The template MUST match the HOST arch —
    // the pveam catalog mixes amd64/arm64 rows and an arm64 guest cannot
    // spawn on x86_64 (observed live: `Failed to spawn container`).
    // Idempotent — a re-run with a template present makes NO pveam calls.
    let tpl = ensure_debian_template(client, target, &spec.template)?;

    plain(&spec.hostname)?;
    if let Some(t) = &spec.template {
        plain(t)?;
    }
    plain(&spec.storage)?;
    plain(&spec.bridge)?;

    // Explicit vmid must be in the PVE system range; an omitted one is
    // picked cluster-wide (`pvesh get /cluster/nextid`) — and a same-name
    // container is REFUSED, never duplicated.
    let vmid = match spec.vmid {
        Some(v) => {
            if v < 100 {
                return Err(BootstrapError::Verify(format!(
                    "vmid {v} is below the PVE system range (100+); pick a free id"
                )));
            }
            v
        }
        None => pick_free_vmid(client, target, &spec.hostname)?,
    };

    // --unprivileged 1 is explicit, not the CLI default: pct's CLI defaults
    // to PRIVILEGED, and this container will host the relay + control plane
    // (runner ciphertext, relay membership) — a root escape inside it must
    // not land as host root on the PVE machine. Nothing downstream needs
    // privileged (A3 verifies via `pct exec` through the host).
    // --features nesting=1 is what lets dockerd (overlay2) run inside the
    // unprivileged container — without it docker-in-LXC fails at the mount
    // namespace boundary. keyctl=1 is required alongside for docker itself
    // (kernel keyring access inside the guest). fuse=1 exposes /dev/fuse, the
    // fuse-overlayfs storage-driver device (observed live: docker in the
    // guest failed with `fuse: device not found` until it was set).
    let create = format!(
        "pct create {vmid} local:vztmpl/{tpl} --rootfs {storage}:{rootfs_gb} \
         --memory {memory_mb} --hostname {host} --unprivileged 1 \
         --features fuse=1,keyctl=1,nesting=1 --net0 name=eth0,bridge={bridge},ip=dhcp",
        vmid = vmid,
        tpl = tpl,
        storage = spec.storage,
        rootfs_gb = spec.rootfs_gb,
        memory_mb = spec.memory_mb,
        host = spec.hostname,
        bridge = spec.bridge,
    );
    let out = exec(client, target, &create, 120)?;
    expect_ok(&out, "pct create")?;

    let start = format!("pct start {vmid}", vmid = vmid);
    let out = exec(client, target, &start, 120)?;
    expect_ok(&out, "pct start")?;

    // A3 — reachability self-check: the runner asks the guest, through the
    // host, `pct exec`; the guest answers. No IP guessing.
    let verify = format!(
        "pct exec {vmid} -- sh -c 'hostname && uname -s && whoami'",
        vmid = vmid
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
    ensure_guest_docker(client, target, vmid).await?;

    // A4 support — the domain gate needs the guest's IP so the install can
    // require the DOMAIN to resolve to it (the IP is never the identity).
    // `pct exec <vmid> -- ip -4 -o addr` needs no shell quoting.
    let ip = exec(
        client,
        target,
        &format!("pct exec {vmid} -- ip -4 -o addr"),
        60,
    )
    .map(|out| {
        out.stdout
            .split_whitespace()
            .collect::<Vec<_>>()
            .windows(2)
            .find(|w| w[0] == "inet" && !w[1].starts_with("127."))
            .and_then(|w| w[1].split('/').next().map(str::to_owned))
    })
    .unwrap_or(None);

    Ok(BootstrapResult {
        kind: TargetKind::ProxmoxLxc,
        id: vmid.to_string(),
        name: spec.hostname.clone(),
        ip,
        detail: format!(
            "lxc {vmid} ({hostname}) created from {tpl} on {storage} ({rootfs_gb}G rootfs), \
             started, and the guest verified via `pct exec` (kernel {kernel}); \
             docker+compose ready",
            vmid = vmid,
            hostname = spec.hostname,
            tpl = tpl,
            storage = spec.storage,
            rootfs_gb = spec.rootfs_gb,
            kernel = out.stdout.lines().nth(1).unwrap_or("?")
        ),
    })
}

/// Map the host arch to the template arch suffix. `uname -m` on PVE: x86_64
/// or aarch64 (arm64 alias included for safety). Anything else fails closed.
fn host_arch(client: &McpClient, target: &str) -> Result<&'static str, BootstrapError> {
    let out = exec(client, target, "uname -m", 120)?;
    expect_ok(&out, "uname -m")?;
    match out.stdout.trim() {
        "x86_64" => Ok("amd64"),
        "aarch64" | "arm64" => Ok("arm64"),
        other => Err(BootstrapError::Verify(format!(
            "unsupported host arch {other:?} (expected x86_64 or aarch64)"
        ))),
    }
}

/// Pick a vmid when the operator omitted `--vmid`. Two real constraints:
/// 1. NAMES are unique on the box — a second `bootstrap --name relay-box`
///    must NOT create a duplicate host answering the same name. `pct list`
///    (plain table `<vmid> <status> <type> <name>`) is checked first and a
///    same-hostname container is refused outright (the operator targets the
///    existing one with `--vmid` or picks a new name).
/// 2. The vmid namespace is SHARED with QEMU VMs, which `pct list` misses —
///    so the id comes from `pvesh get /cluster/nextid` (the canonical
///    cluster-wide next free id), not a pct-only scan.
fn pick_free_vmid(client: &McpClient, target: &str, hostname: &str) -> Result<u32, BootstrapError> {
    let list = exec(client, target, "pct list", 120)?;
    expect_ok(&list, "pct list")?;
    // The name is the LAST token: the Lock column is blank for an unlocked
    // container, so the normal row is only three fields (`nth(3)` = None).
    for line in list.stdout.lines().skip(1) {
        let name = line.split_whitespace().last().unwrap_or("");
        if name == hostname {
            return Err(BootstrapError::Verify(format!(
                "a container named {hostname:?} already exists on this host; pass --vmid to \
                 target it, or pick a different --name"
            )));
        }
    }
    let next = exec(client, target, "pvesh get /cluster/nextid", 120)?;
    expect_ok(&next, "cluster nextid")?;
    let vmid: u32 = next.stdout.trim().parse().map_err(|_| {
        BootstrapError::Verify(format!(
            "pvesh nextid did not yield a number: {:?}",
            next.stdout
        ))
    })?;
    if vmid < 100 {
        return Err(BootstrapError::Verify(format!(
            "pvesh nextid returned a vmid below the PVE system range: {vmid}"
        )));
    }
    Ok(vmid)
}

/// Ensure docker + compose run inside the guest. Install-if-missing is one
/// `pct exec` (baked to skip when already present — idempotent re-runs; the
/// apt run is the slow path: 600s). The daemon must then answer `docker
/// info`; when the default overlay2 driver fails inside the unprivileged
/// container, fall back to fuse-overlayfs (the canonical fix), restart, and
/// re-verify. A second failure surfaces the raw output with a hint.
async fn ensure_guest_docker(
    client: &McpClient,
    target: &str,
    vmid: u32,
) -> Result<(), BootstrapError> {
    // Docker + compose v2, on BOTH current Debian releases that PVE templates
    // come in: debian-13/trixie carries `docker-compose-v2` in main, but
    // debian-12/bookworm does NOT (only v1 `docker-compose`, which is not the
    // `docker compose` plugin). When the Debian name is unavailable, fall
    // back to download.docker.com's own `docker-compose-plugin` (Docker's
    // official channel for bookworm). The install-if-missing guard makes
    // re-runs and retries free. Retried 3x because `pct start` may succeed
    // before the guest has a DHCP lease — apt against no network fails on
    // the first attempt.
    let install = format!(
        "pct exec {vmid} -- sh -c 'export DEBIAN_FRONTEND=noninteractive; \
         if ! docker compose version >/dev/null 2>&1; then \
         apt-get update >/dev/null 2>&1; \
         if ! apt-get install -y docker.io docker-compose-v2 >/dev/null; then \
         apt-get install -y curl gpg >/dev/null && \
         curl -fsSL https://download.docker.com/linux/debian/gpg | \
         gpg --batch --yes --dearmor -o /usr/share/keyrings/docker.gpg && \
         echo \"deb [arch=amd64 signed-by=/usr/share/keyrings/docker.gpg] \
         https://download.docker.com/linux/debian $(. /etc/os-release && echo $VERSION_CODENAME) \
         stable\" > /etc/apt/sources.list.d/docker.list && \
         apt-get update >/dev/null 2>&1 && \
         apt-get install -y docker.io docker-compose-plugin >/dev/null; fi; fi; \
         docker compose version'",
        vmid = vmid
    );
    let mut install_err: Option<BootstrapError> = None;
    for attempt in 0..3 {
        match exec(client, target, &install, 600).and_then(|out| {
            expect_ok(&out, "guest docker install")?;
            Ok(())
        }) {
            Ok(()) => {
                install_err = None;
                break;
            }
            Err(e) => install_err = Some(e),
        }
        if attempt < 2 {
            tokio::time::sleep(std::time::Duration::from_secs(8)).await;
        }
    }
    if let Some(e) = install_err {
        return Err(BootstrapError::Verify(format!(
            "guest docker/compose install failed after 3 attempts (the guest may still be \
             getting its DHCP lease): {e}"
        )));
    }

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
        "pct exec {vmid} -- sh -c 'export DEBIAN_FRONTEND=noninteractive; \
         apt-get install -y fuse-overlayfs >/dev/null && \
         mkdir -p /etc/docker && \
         printf \"{{\\\"storage-driver\\\":\\\"fuse-overlayfs\\\"}}\\n\" \
         > /etc/docker/daemon.json && \
         systemctl restart docker >/dev/null 2>&1 || \
         service docker restart >/dev/null 2>&1'",
        vmid = vmid
    );
    // Even a failed fallback must surface the ORIGINAL daemon error + hint.
    if let Err(e) = exec(client, target, &fallback, 600).and_then(|out| {
        expect_ok(&out, "guest docker fuse fallback")?;
        Ok(())
    }) {
        return Err(BootstrapError::Verify(format!(
            "fuse-overlayfs fallback failed in the guest ({e}); original daemon error: \
             ({first}); consider a privileged container"
        )));
    }

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
    let mut main_ip = String::new();
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
            if !ip.is_empty() && ip != "0.0.0.0" {
                main_ip = ip.to_string();
            }
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
        ip: (!main_ip.is_empty()).then_some(main_ip),
        detail,
    })
}

/// A4 — the DOMAIN GATE (blocking): after the target is up, the install
/// waits until `domain` resolves to `want_ip` (LAN DNS, or /etc/hosts for
/// the POC). The domain is the community's identity (BUZZ_SURFACE §9.8);
/// an IP-hosted community IS IP-identity, which is exactly what this gate
/// prevents from ever happening.
pub fn wait_for_domain_resolution(
    domain: &str,
    want_ip: &str,
    wait_secs: u64,
    mut resolve: impl FnMut(&str) -> Option<std::net::IpAddr>,
    mut sleep: impl FnMut(std::time::Duration),
) -> Result<(), BootstrapError> {
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(wait_secs);
    let hint = format!("map '{domain} {want_ip}' in your LAN DNS (or /etc/hosts for the POC)");
    loop {
        match resolve(domain) {
            Some(ip) if ip.to_string() == want_ip => {
                println!("DOMAIN-GATE: {domain} resolves to {want_ip} — continuing");
                return Ok(());
            }
            Some(ip) => println!("DOMAIN-GATE: {domain} resolves to {ip}, want {want_ip} — {hint}"),
            None => println!("DOMAIN-GATE: {domain} does not resolve yet — {hint}"),
        }
        if std::time::Instant::now() >= deadline {
            return Err(BootstrapError::Verify(format!(
                "domain gate: {domain} never resolved to {want_ip} within {wait_secs}s — {hint}"
            )));
        }
        sleep(std::time::Duration::from_secs(5));
    }
}

/// Resolve a domain to its first address (honors the system resolver,
/// including /etc/hosts).
pub fn resolve_ip(domain: &str) -> Option<std::net::IpAddr> {
    use std::net::ToSocketAddrs;
    (domain, 0).to_socket_addrs().ok()?.map(|sa| sa.ip()).next()
}

/// A bare 64-hex Nostr pubkey (the kind the relay env expects).
pub fn is_hex64(s: &str) -> bool {
    s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit())
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

#[cfg(test)]
mod domain_gate_tests {
    use super::*;
    use std::net::IpAddr;
    use std::time::Duration;

    fn no_sleep(_: Duration) {}
    fn ip(s: &str) -> Option<IpAddr> {
        s.parse().ok()
    }

    #[test]
    fn first_hit_passes() {
        let r = wait_for_domain_resolution(
            "relay.example",
            "10.0.0.5",
            0,
            |_| ip("10.0.0.5"),
            no_sleep,
        );
        assert!(r.is_ok(), "{r:?}");
    }

    #[test]
    fn hits_after_misses_passes() {
        let mut n = 0;
        let r = wait_for_domain_resolution(
            "relay.example",
            "10.0.0.5",
            60,
            |_| {
                n += 1;
                if n < 3 { None } else { ip("10.0.0.5") }
            },
            no_sleep,
        );
        assert!(r.is_ok(), "{r:?}");
        assert_eq!(n, 3);
    }

    #[test]
    fn never_resolves_fails_with_hint() {
        let e = wait_for_domain_resolution("relay.example", "10.0.0.5", 0, |_| None, no_sleep)
            .unwrap_err();
        let msg = e.to_string();
        assert!(msg.contains("relay.example"), "{msg}");
        assert!(msg.contains("10.0.0.5"), "{msg}");
        assert!(msg.contains("/etc/hosts"), "{msg}");
    }

    #[test]
    fn wrong_ip_fails() {
        let e = wait_for_domain_resolution(
            "relay.example",
            "10.0.0.5",
            0,
            |_| ip("10.0.0.9"),
            no_sleep,
        )
        .unwrap_err();
        assert!(msg_has(e, "10.0.0.5"));
    }

    fn msg_has(e: BootstrapError, needle: &str) -> bool {
        e.to_string().contains(needle)
    }

    #[test]
    fn hex64_validates() {
        let good = "8b31e8f0aa563344fc148e5608c73b6415605aa03eeded3275d25715872a30d8";
        assert!(is_hex64(good));
        assert!(!is_hex64(&good[..63]));
        assert!(!is_hex64(
            "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
        ));
    }
}
