//! Phase B (Chunk 2): deploy the Buzz relay onto the target via the
//! provisioning RUNNER (runner-direct, no delegation).
//!
//! Orchestration only — the actual install is agent-written commands run
//! verbatim by the runner (the same generic `exec` primitive):
//! 1. docker/compose presence gate (B1) — with a remediation, not a raw 127.
//! 2. fetch a PINNED block/buzz tarball over curl+tar (no git dependency);
//!    the overlay is refreshed but `.env` and the docker volumes SURVIVE, so
//!    re-runs never regenerate the relay's signing identity.
//! 3. `cp .env.example .env` (only when absent) + `BUZZ_HTTP_PORT` written +
//!    `./run.sh start` (B1's install, per the upstream bundle contract).
//! 4. liveness poll on the relay port (B2) — the relay's OWN health.
//! 5. report the relay URL; the CP's membership/recording lands in Phase C.

use crate::client::{ExecOutcome, McpClient};

use crate::bootstrap::BootstrapError;

/// Pin the fetched bundle: `refs/heads/main` is a code-to-root path with
/// zero reproducibility. Default = a specific main SHA; `--ref` overrides.
pub const DEFAULT_BUZZ_REF: &str = "f956e6fe06a76e50cbd8fba1a162482e752e7f1a";

#[derive(Debug, Clone)]
pub struct RelayDeploySpec {
    /// Relay hostname as reported (e.g. `relay-box`); NOT interpolated into
    /// commands (shell guard lives in `bootstrap::plain`).
    pub relay_name: String,
    /// Where the bundle overlay lands on the target. Absolute, no `..`,
    /// at least two components — this is also the tar extraction root.
    pub deploy_dir: String,
    /// The relay HTTP port — WRITTEN into the compose .env, not just probed.
    pub http_port: u16,
    /// Which block/buzz ref to fetch (tag or SHA; default = pinned SHA).
    pub buzz_ref: String,
}

#[derive(Debug)]
pub struct RelayDeployResult {
    pub relay_url: String,
    pub detail: String,
}

/// A deploy dir is a DESTRUCTIVE-adjacent operand (tar extraction root,
/// overlay refresh) on a system the operator may run as root. `plain_path`
/// stops injection; this stops deleting the wrong thing: absolute, no `..`,
/// at least two components (never `/`, never `/srv`).
fn safe_deploy_dir(s: &str) -> Result<(), BootstrapError> {
    crate::bootstrap::plain_path(s)?;
    if !s.starts_with('/') {
        return Err(BootstrapError::Verify(format!(
            "deploy dir must be an absolute path (got {s:?})"
        )));
    }
    if s.split('/').any(|part| part == "..") {
        return Err(BootstrapError::Verify(format!(
            "deploy dir must not contain '..' (got {s:?})"
        )));
    }
    let depth = s.split('/').filter(|p| !p.is_empty()).count();
    if depth < 2 {
        return Err(BootstrapError::Verify(format!(
            "deploy dir must be at least two components deep (got {s:?})"
        )));
    }
    Ok(())
}

fn exec_to_ok(
    client: &McpClient,
    target: &str,
    cmd: &str,
    step: &str,
    timeout_s: u64,
) -> Result<ExecOutcome, BootstrapError> {
    let out = client.exec(target, cmd, &[target], timeout_s)?;
    crate::bootstrap::expect_ok(&out, step)?;
    Ok(out)
}

/// B1 gate: docker + the compose plugin must exist on the target.
fn check_docker(client: &McpClient, target: &str) -> Result<(), BootstrapError> {
    match exec_to_ok(
        client,
        target,
        "command -v docker && docker compose version",
        "docker presence",
        60,
    ) {
        Ok(_) => Ok(()),
        Err(e) => Err(BootstrapError::Verify(format!(
            "docker + compose plugin are missing on the target: {e}\n\
             remediation: install them (e.g. `apt-get install docker.io \
             docker-compose-plugin`), or deploy inside the Phase-A LXC after \
             installing docker there — the PVE host currently has neither."
        ))),
    }
}

pub async fn deploy_relay(
    client: &McpClient,
    target: &str,
    spec: &RelayDeploySpec,
) -> Result<RelayDeployResult, BootstrapError> {
    crate::bootstrap::plain(&spec.relay_name)?;
    safe_deploy_dir(&spec.deploy_dir)?;

    check_docker(client, target)?;

    // Fetch the PINNED bundle. No `rm -rf` anywhere: the overlay is
    // refreshed by tar's overwrite, while `.env` (which carries
    // BUZZ_RELAY_PRIVATE_KEY — BUZZ_SURFACE: stable across restarts, needed
    // for member admin) and the docker volumes SURVIVE a re-run.
    let dl = format!(
        "set -e; mkdir -p {dir} && curl -fsSL https://github.com/block/buzz/archive/{ref}.tar.gz \
         -o {dir}/buzz.tar.gz",
        dir = spec.deploy_dir,
        ref = spec.buzz_ref,
    );
    exec_to_ok(client, target, &dl, "download bundle", 180)?;
    let extract = format!(
        "tar -xzf {dir}/buzz.tar.gz -C {dir} --strip-components=1",
        dir = spec.deploy_dir
    );
    exec_to_ok(client, target, &extract, "extract bundle", 180)?;

    // Install per the upstream bundle contract: .env ONCE (preserve the
    // signing identity), then the port, then run.sh. The cold first pull of
    // Postgres/Redis/MinIO/relay/Caddy gets a real timeout (600s), not the
    // default.
    let install = format!(
        "set -e; cd {dir}/deploy/compose && (test -f .env || cp .env.example .env) && \
         (grep -q '^BUZZ_HTTP_PORT=' .env && sed -i 's/^BUZZ_HTTP_PORT=.*/BUZZ_HTTP_PORT={port}/' .env \
          || echo 'BUZZ_HTTP_PORT={port}' >> .env) && ./run.sh start",
        dir = spec.deploy_dir,
        port = spec.http_port,
    );
    exec_to_ok(client, target, &install, "run.sh start", 600)?;

    // B2: the relay's OWN health on the loopback we just verified. Poll,
    // keeping the last non-transient error so expiry says WHY.
    let mut healthy = false;
    let mut last_err: Option<String> = None;
    for _ in 0..30 {
        let probe = format!(
            "curl -fsS http://127.0.0.1:{port}/_liveness",
            port = spec.http_port
        );
        match exec_to_ok(client, target, &probe, "relay liveness", 30) {
            Ok(_) => {
                healthy = true;
                break;
            }
            Err(e) => last_err = Some(e.to_string()),
        }
        tokio::time::sleep(std::time::Duration::from_secs(2)).await;
    }
    if !healthy {
        return Err(BootstrapError::Verify(format!(
            "relay did not pass /_liveness on port {} within the poll window{}",
            spec.http_port,
            last_err
                .map(|e| format!(" (last probe error: {e})"))
                .unwrap_or_default()
        )));
    }

    // B3: the scope claim. Liveness is proven on the target's loopback; the
    // URL is the operator's NAME mapping for the box (it is the ONE scope
    // for the CP once Phase C adds membership — nothing persists here).
    let relay_url = format!(
        "http://{host}:{port}",
        host = spec.relay_name,
        port = spec.http_port
    );
    Ok(RelayDeployResult {
        relay_url: relay_url.clone(),
        detail: format!(
            "Buzz relay deployed from the pinned bundle ({dir}, ref {ref}) and healthy at \
             {relay_url} (loopback-proven) — becomes the control plane's ONE scope once \
             Phase C records membership",
            dir = spec.deploy_dir,
            ref = spec.buzz_ref,
        ),
    })
}
