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
    /// The relay's OWN resolvable URL (e.g. `http://192.168.30.248:3000`) —
    /// written into BUZZ_DOMAIN/RELAY_URL/media URLs. The bundle's
    /// example.com placeholders are NOT literal CHANGE_ME, so the secret
    /// sweep never touched them; a relay that identifies as buzz.example.com
    /// binds a phantom community and the HTTP bridge 404s real hosts.
    pub relay_url: String,
    /// The OPERATOR's Nostr pubkey (64-hex): invite the human operator to
    /// the relay right after it comes up (buzz-admin add-member through the
    /// relay LXC) — fail-closed: required; machines alone shouldn't own the
    /// community.
    pub operator_pubkey: String,
    /// The forced identity DOMAIN (never an IP — BUZZ_SURFACE §9.8). When
    /// set: the .env BUZZ_DOMAIN/RELAY_URL/media become <domain> /
    /// wss(s)://<domain>, AND a LOCAL CA TLS posture is provisioned (own
    /// openssl CA + server cert, Caddyfile `tls` directive, certs mount).
    pub domain: Option<String>,
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
    if !crate::bootstrap::is_hex64(&spec.operator_pubkey) {
        return Err(BootstrapError::Verify(format!(
            "operator pubkey must be a 64-character hex Nostr pubkey (got {:?})",
            spec.operator_pubkey
        )));
    }
    if let Some(d) = &spec.domain {
        crate::bootstrap::plain(d)?;
        if !d
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '.'))
        {
            return Err(BootstrapError::Verify(format!(
                "domain must be a bare DNS name (got {d:?})"
            )));
        }
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
         (grep -q \"^BUZZ_DOMAIN=\" .env && \
          sed -i \"s|^BUZZ_DOMAIN=.*|BUZZ_DOMAIN={rhost}|\" .env || \
          echo \"BUZZ_DOMAIN={rhost}\" >> .env) && \
         (grep -q \"^RELAY_URL=\" .env && \
          sed -i \"s|^RELAY_URL=.*|RELAY_URL={rws}|\" .env || \
          echo \"RELAY_URL={rws}\" >> .env) && \
         (grep -q \"^BUZZ_MEDIA_BASE_URL=\" .env && \
          sed -i \"s|^BUZZ_MEDIA_BASE_URL=.*|BUZZ_MEDIA_BASE_URL={rhttp}/media|\" .env || \
          echo \"BUZZ_MEDIA_BASE_URL={rhttp}/media\" >> .env) && \
         (grep -q \"^BUZZ_MEDIA_SERVER_DOMAIN=\" .env && \
          sed -i \"s|^BUZZ_MEDIA_SERVER_DOMAIN=.*|BUZZ_MEDIA_SERVER_DOMAIN={rhost}|\" .env || \
          echo \"BUZZ_MEDIA_SERVER_DOMAIN={rhost}\" >> .env) && \
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
        rhost = spec.domain.clone().unwrap_or_else(|| {
            spec.relay_url
                .trim_start_matches("http://")
                .trim_start_matches("https://")
                .trim_end_matches('/')
                .to_string()
        }),
        rws = format!(
            "{}://{}",
            if spec.domain.is_some() || spec.relay_url.trim_start().starts_with("https://") {
                "wss"
            } else {
                "ws"
            },
            spec.domain.clone().unwrap_or_else(|| {
                spec.relay_url
                    .trim_start_matches("http://")
                    .trim_start_matches("https://")
                    .trim_end_matches('/')
                    .to_string()
            })
        ),
        rhttp = spec
            .domain
            .as_ref()
            .map(|d| format!("https://{d}"))
            .unwrap_or_else(|| { spec.relay_url.trim_end_matches('/').to_string() }),
    );
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &lxc_cmd(spec.lxc, &install),
        "run.sh start",
        600,
    )?;

    // TLS on the domain (local CA posture — Docs-B): an OWN openssl CA +
    // domain server cert (idempotent via existence guard), the bundle's tiny
    // Caddyfile becomes the domain site with the cert, and compose gains the
    // certs mount. Let's Encrypt DNS-01 is NOT wired here: the stock
    // caddy:2-alpine image ships no DNS provider modules (a custom image is
    // a named follow-up when a provider key exists). Clients trust ca.crt.
    if let Some(domain) = &spec.domain {
        let compose = format!("{cdir}/deploy/compose", cdir = spec.deploy_dir);
        let tls = format!(
            "set -e; cd {compose} && mkdir -p certs && \
             if [ ! -f certs/ca.crt ]; then \
             openssl req -x509 -newkey rsa:3072 -keyout certs/ca.key -out certs/ca.crt -days 3650 -nodes \
               -subj \"/CN=freehold local CA\" && \
             openssl req -newkey rsa:3072 -keyout certs/{d}.key -out /tmp/{d}.csr -nodes \
               -subj \"/CN={d}\" && \
             printf \"subjectAltName=DNS:{d}\\n\" > /tmp/{d}.ext && \
             openssl x509 -req -in /tmp/{d}.csr -CA certs/ca.crt -CAkey certs/ca.key \
               -CAcreateserial -out certs/{d}.crt -days 825 -extfile /tmp/{d}.ext && \
             rm -f /tmp/{d}.csr /tmp/{d}.ext; fi && \
             printf \"%s\\n\" \"{d} {{\" \
               \"  encode zstd gzip\" \
               \"  tls /etc/caddy/certs/{d}.crt /etc/caddy/certs/{d}.key\" \
               \"  reverse_proxy relay:3000\" \
               \"}}\" > Caddyfile && \
             (grep -q \"certs:/etc/caddy/certs\" compose.caddy.yml || \
              sed -i \"s#- ./Caddyfile:/etc/caddy/Caddyfile:ro#- ./Caddyfile:/etc/caddy/Caddyfile:ro\\n      - ./certs:/etc/caddy/certs:ro#\" compose.caddy.yml) && \
             docker compose up -d >/dev/null 2>&1 && \
             echo TLS-LOCAL-CA-{d}",
            compose = compose,
            d = domain,
        );
        crate::bootstrap::exec_to_ok(
            client,
            target,
            &lxc_cmd(spec.lxc, &tls),
            "tls local CA",
            120,
        )?;
    }

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

    // Invite the OPERATOR: after the relay is up, make the human a member
    // (the acceptance's invite step — machines alone shouldn't own the
    // community). Runs through the relay-admin runner at the SAME compose
    // dir the bundle landed in (`spec.deploy_dir`). An already-invited
    // member (buzz-admin exits non-zero on re-add) DEGRADES to a SURFACED
    // warn — re-runs must never sink a healthy deploy at the final step.
    {
        let operator = &spec.operator_pubkey;
        crate::bootstrap::plain(operator)?;
        let invite_dir = format!("{}/deploy/compose", spec.deploy_dir);
        let invite = crate::relay_member::add_member_cmd(&invite_dir, operator, None);
        if let Err(e) = crate::bootstrap::exec_to_ok(
            client,
            target,
            &lxc_cmd(spec.lxc, &invite),
            "invite operator",
            120,
        ) {
            // The freehold binary has NO tracing subscriber (it reports via
            // println/eprintln) — a tracing::warn would vanish, and a silent
            // invite failure would tell the operator they're a member when
            // they aren't. eprintln it for real.
            eprintln!("WARN: installer invite failed — the relay is up and the deploy stands: {e}");
        }
    }

    // B3: the scope claim. Liveness is proven on the target's loopback; the
    // URL is the operator's NAME mapping for the box (it is the ONE scope
    // for the CP once Phase C adds membership — nothing persists here).
    let (relay_url, tls_note) = match &spec.domain {
        Some(d) => (
            format!("https://{d}"),
            format!(
                " TLS on the domain via the LOCAL CA ({cdir}/deploy/compose/certs/ca.crt — \
                 import it into your devices to trust https://{d} + wss://{d})",
                cdir = spec.deploy_dir
            ),
        ),
        None => (
            format!(
                "http://{host}:{port}",
                host = spec.relay_name,
                port = spec.http_port
            ),
            String::new(),
        ),
    };
    Ok(RelayDeployResult {
        relay_url: relay_url.clone(),
        detail: format!(
            "Buzz relay deployed from the pinned bundle ({dir}, ref {ref}) and healthy at \
             {relay_url} (loopback-proven){tls_note} — becomes the control plane's ONE scope \
             once Phase C records membership",
            dir = spec.deploy_dir,
            ref = spec.buzz_ref,
        ),
    })
}
