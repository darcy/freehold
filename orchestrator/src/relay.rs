//! Phase B (Chunk 2): deploy the Buzz relay onto the target via the
//! provisioning RUNNER (runner-direct, no delegation).
//!
//! Orchestration only — the actual install is agent-written commands run
//! verbatim by the runner (the same generic `exec` primitive):
//! 1. docker/compose presence gate (B1) — the PVE host has neither, so the
//!    gate must fail with a remediation, not a raw 127.
//! 2. fetch the official compose bundle over curl+tar (the target has curl;
//!    no git dependency).
//! 3. `cp .env.example .env` + `./run.sh start` (B1's install, as the
//!    upstream bundle intends).
//! 4. liveness poll on the relay port (B2) — the relay's OWN health, the
//!    same self-check discipline as runner readiness.
//! 5. report the relay URL; it becomes the control plane's ONE scope (B3).

use crate::client::{ExecOutcome, McpClient};

use crate::bootstrap::BootstrapError;

#[derive(Debug, Clone)]
pub struct RelayDeploySpec {
    /// Relay hostname as reported (e.g. `relay-box`); NOT interpolated into
    /// commands (shell guard lives in `bootstrap::plain`).
    pub relay_name: String,
    /// Where the bundle lands on the target. Boot-time writable, not the
    /// repo — plain value.
    pub deploy_dir: String,
    /// The relay HTTP port to poll; the operator sets it in the compose .env.
    pub http_port: u16,
}

#[derive(Debug)]
pub struct RelayDeployResult {
    pub relay_url: String,
    pub detail: String,
}

fn exec_to_ok(
    client: &McpClient,
    target: &str,
    cmd: &str,
    step: &str,
) -> Result<ExecOutcome, BootstrapError> {
    let out = client.exec(target, cmd, &[target], 180)?;
    crate::bootstrap::expect_ok(&out, step)?;
    Ok(out)
}

/// B1 gate: docker + the compose plugin must exist on the target. Missing is
/// a remediation, not a hard crash — installing docker on a PVE host (or
/// inside the Phase-A LXC) is an operator call.
fn check_docker(client: &McpClient, target: &str) -> Result<(), BootstrapError> {
    match exec_to_ok(
        client,
        target,
        "command -v docker && docker compose version",
        "docker presence",
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
    crate::bootstrap::plain_path(&spec.deploy_dir)?;

    check_docker(client, target)?;

    // Fetch the official bundle (curl+tar — both present on the laptop-style
    // PVE host; no git dependency). Two SEPARATE steps, deliberately not a
    // pipeline: each fails with its own attribution, and pipelines behave
    // differently across dash/bash variants inside remote shells.
    let dl = format!(
        "set -e; rm -rf {dir} && mkdir -p {dir} && \
         curl -fsSL https://github.com/block/buzz/archive/refs/heads/main.tar.gz \
         -o {dir}/buzz.tar.gz",
        dir = spec.deploy_dir
    );
    exec_to_ok(client, target, &dl, "download bundle")?;
    let extract = format!(
        "tar -xzf {dir}/buzz.tar.gz -C {dir} --strip-components=1",
        dir = spec.deploy_dir
    );
    exec_to_ok(client, target, &extract, "extract bundle")?;

    // Install per the upstream bundle's own contract: .env from example,
    // then run.sh. A missing template/env var trips run.sh with output.
    let install = format!(
        "set -e; cd {dir}/deploy/compose && cp .env.example .env && ./run.sh start",
        dir = spec.deploy_dir
    );
    let out = exec_to_ok(client, target, &install, "run.sh start")?;
    let _ = out;

    // B2: the relay's OWN health. Poll the liveness endpoint (bounded 30s).
    let mut healthy = false;
    for _ in 0..15 {
        let probe = format!(
            "curl -fsS http://127.0.0.1:{port}/_liveness",
            port = spec.http_port
        );
        match exec_to_ok(client, target, &probe, "relay liveness") {
            Ok(_) => {
                healthy = true;
                break;
            }
            Err(_) => {}
        }
        tokio::time::sleep(std::time::Duration::from_secs(2)).await;
    }
    if !healthy {
        return Err(BootstrapError::Verify(format!(
            "relay did not pass /_liveness on port {} within the poll window \
             (check `docker compose logs` on the target)",
            spec.http_port
        )));
    }

    let relay_url = format!(
        "http://{host}:{port}",
        host = spec.relay_name,
        port = spec.http_port
    );
    Ok(RelayDeployResult {
        relay_url: relay_url.clone(),
        detail: format!(
            "Buzz relay deployed from the official bundle ({dir}) and healthy \
             at {relay_url} — this becomes the control plane's ONE scope (B3)",
            dir = spec.deploy_dir
        ),
    })
}
