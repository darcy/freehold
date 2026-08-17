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

use crate::client::McpClient;

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
    /// Deploy INTO this LXC on the target (the target is the PVE host, the
    /// LXC holds docker). None = deploy directly on the target host. When
    /// set, every command is wrapped `pct exec <lxc> -- sh -c '<cmd>'` and
    /// the deploy dir is a path INSIDE the guest.
    pub lxc: Option<u32>,
    /// The relay OWNER Nostr pubkey (64-hex) — written into `RELAY_OWNER_PUBKEY`
    /// in the compose .env. The bundle's run.sh refuses to start with CHANGE_ME
    /// placeholders; the owner is the CP's identity (Phase C member admin).
    pub owner_pubkey: String,
}

/// Wrap a target command for execution inside an LXC via the host runner.
/// The command must be single-quote-FREE (the payload is single-quoted for
/// the guest `sh -c`); the guest then sees double quotes/`$()` normally.
pub(crate) fn lxc_cmd(lxc: Option<u32>, cmd: &str) -> String {
    match lxc {
        Some(id) => format!("pct exec {id} -- sh -c '{cmd}'"),
        None => cmd.to_string(),
    }
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
pub(crate) fn safe_deploy_dir(s: &str) -> Result<(), BootstrapError> {
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

/// B1 gate: docker + the compose plugin must exist on the target (or inside
/// the target's LXC when `lxc` is set — that is where the bootstrap puts it).
fn check_docker(client: &McpClient, target: &str, lxc: Option<u32>) -> Result<(), BootstrapError> {
    match crate::bootstrap::exec_to_ok(
        client,
        target,
        &lxc_cmd(lxc, "command -v docker && docker compose version"),
        "docker presence",
        60,
    ) {
        Ok(_) => Ok(()),
        Err(e) => Err(BootstrapError::Verify(format!(
            "docker + compose plugin are missing on the target: {e}\n\
             remediation: install them (Debian 13+: `apt-get install docker.io \
             docker-compose-v2`; Debian 12: docker-compose-v2 from \
             bookworm-backports or download.docker.com's `docker-compose-plugin`), \
             or run `freehold bootstrap` — it installs docker+compose inside the \
             provisioned LXC."
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
    // A ref interpolates straight into curl's URL — tags/SHAs legitimately
    // contain '/', but anything shell-hostile must be rejected.
    crate::bootstrap::plain_path(&spec.buzz_ref)?;
    // The owner pubkey lands in the compose .env unquoted; it must be a bare
    // 64-hex Nostr pubkey (run.sh itself rejects anything shorter/else).
    if spec.owner_pubkey.len() != 64 || !spec.owner_pubkey.chars().all(|c| c.is_ascii_hexdigit()) {
        return Err(BootstrapError::Verify(format!(
            "owner pubkey must be a 64-character hex Nostr pubkey (got {:?})",
            spec.owner_pubkey
        )));
    }

    check_docker(client, target, spec.lxc)?;

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
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &lxc_cmd(spec.lxc, &dl),
        "download bundle",
        180,
    )?;
    let extract = format!(
        "tar -xzf {dir}/buzz.tar.gz -C {dir} --strip-components=1",
        dir = spec.deploy_dir
    );
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &lxc_cmd(spec.lxc, &extract),
        "extract bundle",
        180,
    )?;

    // Install per the upstream bundle contract: .env ONCE (preserve the
    // signing identity), then the port + the owner + per-key secrets, then
    // run.sh. The cold first pull of Postgres/Redis/MinIO/relay/Caddy gets a
    // real timeout (600s), not the default.
    // Single-quote-free on purpose: the whole command is single-quoted when
    // wrapped for `pct exec ... sh -c`.
    // Entropy: each secret comes from /dev/urandom (od -N32 -> 64 hex), NOT a
    // timestamp — `sha256(date +%s%N)` is guessable within a bound run (the
    // relay's first signed event leaks the boot time) and BUZZ_RELAY_PRIVATE_KEY
    // is the relay's signing key (forging kind 13534 = self-admission).
    let install = format!(
        "set -e; cd {dir}/deploy/compose && (test -f .env || cp .env.example .env) && \
         (grep -q \"^BUZZ_HTTP_PORT=\" .env && \
          sed -i \"s/^BUZZ_HTTP_PORT=.*/BUZZ_HTTP_PORT={port}/\" .env || \
          echo \"BUZZ_HTTP_PORT={port}\" >> .env) && \
         (grep -q \"^RELAY_OWNER_PUBKEY=\" .env && \
          sed -i \"s/^RELAY_OWNER_PUBKEY=.*/RELAY_OWNER_PUBKEY={owner}/\" .env || \
          echo \"RELAY_OWNER_PUBKEY={owner}\" >> .env) && \
         for k in BUZZ_RELAY_PRIVATE_KEY BUZZ_GIT_HOOK_HMAC_SECRET POSTGRES_PASSWORD \
         REDIS_PASSWORD BUZZ_S3_ACCESS_KEY BUZZ_S3_SECRET_KEY; do \
         if grep -q \"^$k=CHANGE_ME\" .env; then \
         v=$(od -An -N32 -tx1 /dev/urandom | tr -d \"\\n \"); \
         sed -i \"s/^$k=CHANGE_ME.*/$k=$v/\" .env; \
         fi; done && \
         (grep -qE \"=CHANGE_ME\" .env && echo \"still has CHANGE_ME placeholders in \
         {dir}/deploy/compose/.env\" >&2 && exit 1 || true) && \
         ./run.sh start",
        dir = spec.deploy_dir,
        port = spec.http_port,
        owner = spec.owner_pubkey,
    );
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &lxc_cmd(spec.lxc, &install),
        "run.sh start",
        600,
    )?;

    // B2: the relay's OWN health on the loopback we just verified. Poll,
    // keeping the last non-transient error so expiry says WHY.
    let mut healthy = false;
    let mut last_err: Option<String> = None;
    for _ in 0..30 {
        let probe = format!(
            "curl -fsS http://127.0.0.1:{port}/_liveness",
            port = spec.http_port
        );
        match crate::bootstrap::exec_to_ok(
            client,
            target,
            &lxc_cmd(spec.lxc, &probe),
            "relay liveness",
            30,
        ) {
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
