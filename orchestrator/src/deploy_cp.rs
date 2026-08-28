//! Phase C (Chunk 2), C1: deploy the control plane onto the target BOX in
//! OPERATE mode — the process + its data live on the box; the console stays
//! loopback-bound (C3); operator access from elsewhere is an SSH tunnel.
//!
//! Transport: the runner's ONE primitive is `exec` (locked model: no scp, no
//! semantic tools). The binary therefore travels as base64 — written in
//! chunks on the box, decoded, verified by byte size — and the process is
//! started detached (`setsid nohup`), probed at `/healthz`, and KILL-CHECKED
//! by pid (`kill -0`) so a serve that dies right after binding is surfaced.
//!
//! Identity: NO keypair is shipped, ever. The box's `serve` (via
//! `Console::load_or_create`) generates a fresh identity; its PUBKEY is read
//! back with `control-plane identity` (pubkey-only output) so the operator /
//! CPA can add it as a relay member. A pre-made keypair would land in the
//! runner's verbatim audit log + `ps` — the locked rule "never put secrets
//! in logs" (relay owner private keys must never leave the operator's side).

use std::path::PathBuf;

use base64::Engine as _;

use crate::bootstrap::{BootstrapError, exec_to_ok, plain_path};
use crate::client::McpClient;

/// Derived from `planebase::GUEST_PATH_CP` (the mount guest path) — pinned
/// by `cp_dirs_track_guest_path` so they can't silently drift from the
/// plane. clap `default_value` needs a literal, so these can't `concat!`.
pub const DEFAULT_CP_STATE_DIR: &str = "/srv/data/cp/control-plane";
pub const DEFAULT_CP_BIN_DIR: &str = "/srv/data/cp/bin";
pub const DEFAULT_CP_BIND: &str = "127.0.0.1:8080";

/// The console bind to ship: an EXPLICIT operator value always wins; the
/// DEFAULT becomes the LAN bind exactly when NIP-98 authn is on (the
/// operator's proxy path); no authn keeps the loopback posture.
pub fn resolve_cp_bind(explicit: Option<&str>, authn: bool) -> String {
    match explicit {
        Some(b) => b.to_string(),
        None if authn => format!(
            "0.0.0.0:{}",
            DEFAULT_CP_BIND
                .rsplit_once(':')
                .map(|(_, p)| p)
                .unwrap_or("8080")
        ),
        None => DEFAULT_CP_BIND.to_string(),
    }
}

#[cfg(test)]
mod bind_resolve_tests {
    use super::resolve_cp_bind;

    #[test]
    fn explicit_bind_always_wins() {
        // a DELIBERATE loopback bind is honored even under authn (the
        // SSH-tunnel operator) — the Option shape makes this expressible.
        assert_eq!(
            resolve_cp_bind(Some("127.0.0.1:8080"), true),
            "127.0.0.1:8080"
        );
        assert_eq!(
            resolve_cp_bind(Some("127.0.0.1:9000"), true),
            "127.0.0.1:9000"
        );
        assert_eq!(resolve_cp_bind(Some("0.0.0.0:8080"), false), "0.0.0.0:8080");
        assert_eq!(resolve_cp_bind(Some("[::1]:8080"), true), "[::1]:8080");
    }

    #[test]
    fn default_flips_only_under_authn() {
        assert_eq!(resolve_cp_bind(None, true), "0.0.0.0:8080");
        assert_eq!(resolve_cp_bind(None, false), "127.0.0.1:8080");
    }
}

/// base64 chunk size written per exec (kept well under the transport frame;
/// a chunk is one `printf` of pure base64 — shell-safe).

#[derive(Debug, Clone)]
pub struct DeployCpSpec {
    /// Remote absolute state dir (holds state.json + `console/identity.json`).
    pub state_dir: String,
    /// Remote dir for the shipped binary.
    pub bin_dir: String,
    /// Loopback bind for the console (C3: validated loopback-only, STRICT
    /// host:port — no shell tail can ride the port).
    pub bind_addr: String,
    /// LOCAL path of the built control-plane binary to ship.
    pub binary_path: PathBuf,
    /// The relay this CP helps serve — the ONE scope (C4 posture).
    pub relay_url: String,
    /// The relay's signing pubkey (the 39002 roster trust anchor).
    pub relay_pubkey: Option<String>,
    /// The relay LXC's LAN IP, pinned into the guest's /etc/hosts.
    pub relay_host_ip: Option<String>,
    /// Operator/admin Nostr pubkeys (64-hex) seeding the console's NIP-98
    /// auth whitelist (C3.5). Non-empty => the console may bind non-loopback
    /// and the deploy's own loopback guard is relaxed.
    pub admin_pubkeys: Vec<String>,
    /// Deploy INTO this LXC on the target (the CP lives in its OWN guest —
    /// a different LXC than the relay's by default). Every remote command is
    /// wrapped in `pct exec`, so state + binary land inside the guest and the
    /// loopback console binds the GUEST's 127.0.0.1.
    pub lxc: Option<u32>,
    /// The console's PUBLIC host (convention: cp-<relay-host>) when the relay
    /// is fronted by a proxy — the DNS-rebinding guard also allows it.
    pub public_origin: Option<String>,
    /// LOCAL path of the built `freehold-runner` binary (the CO-LOCATED
    /// runner: the architecture rule is the CP's own runner lives on the
    /// CP's target). When given with `runner_package`, the deploy ships the
    /// runner into the guest, starts it as a systemd unit, ADOPTS it into
    /// the console registry and self-grants the console.
    pub runner_binary: Option<PathBuf>,
    /// LOCAL dir of an EXISTING runner package (identity.json + secrets.json
    /// + known_hosts.json) to co-locate + adopt on this deploy.
    pub runner_package: Option<PathBuf>,
}

#[derive(Debug)]
pub struct DeployCpResult {
    pub state_dir: String,
    pub bind_addr: String,
    /// The box's FRESH console pubkey (generated on the box; never shipped).
    /// Add it as a relay member (`freehold relay-member --pubkey <this>`).
    pub pubkey: String,
    pub detail: String,
}

/// Ship a LOCAL file to the target as base64 chunks through the runner's
/// exec-only primitive (decoded + size-verified remotely; lxc-wrapped; the
/// .b64 is truncated first so an interrupted ship never wedges the next one).
async fn ship_file(
    client: &McpClient,
    target: &str,
    spec: &DeployCpSpec,
    local_path: &PathBuf,
    remote_final: &str,
    step: &str,
) -> Result<(), BootstrapError> {
    // ONE sftp upload over the runner's pooled connection to a HOST temp
    // path (the ssh lane ends at the host — pct exec is how we reach the
    // guest), then `pct push` into the guest. Raw binary streaming: no
    // base64, no command-size limits, no per-chunk round trips (the old
    // 24KB-exec ship took ~19 minutes for a 9.4MB binary; this is seconds).
    let local_size = std::fs::metadata(local_path)?.len();
    let host_tmp = format!("/tmp/freehold-ship-{}", std::process::id());
    let _ = exec_to_ok(
        client,
        target,
        &format!("rm -f {host_tmp}"),
        &format!("reset host tmp {step}"),
        30,
    );
    let remote_size = client
        .upload(target, &local_path.to_string_lossy(), &host_tmp, 300)
        .map_err(|e| BootstrapError::Step {
            step: format!("sftp {step}"),
            exit: None,
            output: format!("{e}"),
        })?;
    if remote_size != local_size {
        return Err(BootstrapError::Verify(format!(
            "shipped {step} size mismatch: remote {remote_size} vs local {local_size}"
        )));
    }
    // move the host file into place: inside an LXC via pct push, on a bare
    // host (VPS / non-LXC CP) the upload already landed on the host — a mv.
    let place = match spec.lxc {
        Some(lxc) => format!("pct push {lxc} {host_tmp} {remote_final}"),
        None => format!("mv {host_tmp} {remote_final}"),
    };
    exec_to_ok(client, target, &place, &format!("place {step}"), 120)?;
    exec_to_ok(
        client,
        target,
        &crate::relay::lxc_cmd(spec.lxc, &format!("chmod 755 {remote_final}")),
        &format!("chmod {step}"),
        30,
    )?;
    let out = exec_to_ok(
        client,
        target,
        &crate::relay::lxc_cmd(spec.lxc, &format!("wc -c < {remote_final}")),
        &format!("verify {step}"),
        30,
    )?;
    let guest_size: usize = out.stdout.trim().parse().map_err(|_| {
        BootstrapError::Verify(format!("guest size not a number: {:?}", out.stdout))
    })?;
    if guest_size != local_size as usize {
        return Err(BootstrapError::Verify(format!(
            "shipped {step} guest size mismatch: {guest_size} vs local {local_size}"
        )));
    }
    let _ = exec_to_ok(
        client,
        target,
        &format!("rm -f {host_tmp}"),
        &format!("clean {step}"),
        30,
    );
    Ok(())
}

/// Ship a SMALL file (identity/known_hosts/secrets) in ONE exec.
async fn ship_small_file(
    client: &McpClient,
    target: &str,
    spec: &DeployCpSpec,
    local_path: &PathBuf,
    remote_final: &str,
    step: &str,
) -> Result<(), BootstrapError> {
    let bytes = std::fs::read(local_path)?;
    let b64 = base64::engine::general_purpose::STANDARD.encode(&bytes);
    let parent = remote_final.rsplit_once('/').map(|(p, _)| p).unwrap_or(".");
    exec_to_ok(
        client,
        target,
        &crate::relay::lxc_cmd(
            spec.lxc,
            &format!(
                "mkdir -p {parent} && printf %s \"{b64}\" | base64 -d > {remote_final} \
                 && chmod 600 {remote_final}"
            ),
        ),
        step,
        60,
    )?;
    Ok(())
}

pub async fn deploy_cp(
    client: &McpClient,
    target: &str,
    spec: &DeployCpSpec,
) -> Result<DeployCpResult, BootstrapError> {
    plain_path(&spec.state_dir)?;
    crate::relay::safe_deploy_dir(&spec.state_dir)?;
    plain_path(&spec.bin_dir)?;
    crate::relay::safe_deploy_dir(&spec.bin_dir)?;
    // Same guard the CP itself enforces at serve time — fail the deploy early
    // instead of shipping a config the box will refuse. C3.5: an admin
    // whitelist (NIP-98 console auth) RELAXES the guard — the console may
    // bind the LAN; without authn the loopback-only refusal stays.
    if spec.admin_pubkeys.is_empty() {
        freehold_control_plane::validate_loopback_bind(&spec.bind_addr)
            .map_err(BootstrapError::Verify)?;
    }

    exec_to_ok(
        client,
        target,
        &crate::relay::lxc_cmd(
            spec.lxc,
            &format!(
                "mkdir -p {sd}/console && mkdir -p {bd} && rm -f {bd}/control-plane.b64",
                sd = spec.state_dir,
                bd = spec.bin_dir
            ),
        ),
        "mkdir deploy dirs",
        30,
    )?;

    // Stop any PRIOR serve instance BEFORE writing over the binary — writing
    // a running executable fails with ETXTBSY and wedges the upgrade path.
    // Deterministic pidfile (written by the start step), no pkill/pattern
    // matching. `; true` guarantees the stop itself never fails the deploy.
    exec_to_ok(
        client,
        target,
        &crate::relay::lxc_cmd(
            spec.lxc,
            &format!(
                "p=$(cat {sd}/serve.pid 2>/dev/null); [ -n \"$p\" ] && kill \"$p\" \
                 >/dev/null 2>&1; rm -f {sd}/serve.pid; true",
                sd = spec.state_dir,
            ),
        ),
        "stop prior control plane",
        30,
    )?;

    // Ship the control-plane binary (base64 chunks through exec-only).
    ship_file(
        client,
        target,
        spec,
        &spec.binary_path,
        &format!("{bd}/control-plane", bd = spec.bin_dir),
        "control-plane binary",
    )
    .await?;

    // Start detached (setsid: not killed when the exec channel closes),
    // echo the pid, then probe loopback /healthz AND kill -0 the pid — a
    // serve that died (e.g. address already in use) is never reported as up.
    let admin_flag = if spec.admin_pubkeys.is_empty() {
        String::new()
    } else {
        format!(" --admin-pubkeys {}", spec.admin_pubkeys.join(","))
    };
    let origin_flag = spec
        .public_origin
        .as_ref()
        .map(|o| format!(" --public-origin {o}"))
        .unwrap_or_default();
    // The console's relay SCOPE: 2.6.1 features (grant sync, runner channel
    // views, the AGENTS availability probe) read `state.relay_url` — the
    // serve must be told the scope or it runs loopback-posture with none.
    // The relay signing pubkey is auto-discovered via NIP-11 (best-effort:
    // no pubkey => no scope flags => the console stays scope-less rather
    // than passing a half scope that `serve` rejects).
    let domain = spec
        .relay_url
        .trim_start_matches("https://")
        .trim_start_matches("http://")
        .trim_end_matches('/')
        .to_string();
    // Explicit --relay-pubkey wins; NIP-11 discovery is the fallback (Buzz
    // often advertises none — the operator reads the key from the relay box).
    let relay_pk = spec
        .relay_pubkey
        .clone()
        .or_else(|| freehold_installer::relay_pubkey_nip11(&domain));
    // A co-located console (in an LXC on the relay's host) cannot terminate
    // the operator's public TLS nor resolve the tailnet DNS — its relay
    // scope points straight at the relay LXC's HTTP listener.
    let scope_url = match &spec.relay_host_ip {
        Some(ip) => format!("http://{ip}:3000"),
        None => spec.relay_url.clone(),
    };
    let relay_flag = match relay_pk {
        Some(pk) => format!(" --relay-url {scope_url} --relay-pubkey {pk} --relay-host {domain}"),
        None => String::new(),
    };
    // The console (co-located in an LXC) must RESOLVE the relay domain to
    // query it — the operator's DNS may not reach inside the guests.
    if let Some(ip) = &spec.relay_host_ip {
        let host = spec
            .relay_url
            .trim_start_matches("https://")
            .trim_start_matches("http://")
            .trim_end_matches('/')
            .split(':')
            .next()
            .unwrap_or_default()
            .to_string();
        let hosts_cmd = format!(
            "grep -q '{host}' /etc/hosts 2>/dev/null || echo '{ip} {host}' >> /etc/hosts",
            host = host,
            ip = ip,
        );
        // the console's relay reads DEPEND on this pin — a failure must
        // surface, not vanish into `let _ =`.
        exec_to_ok(
            client,
            target,
            &crate::relay::lxc_cmd(spec.lxc, &hosts_cmd),
            "pin relay host",
            30,
        )?;
    }
    let start = format!(
        "setsid nohup {bd}/control-plane serve --state-dir {sd} --addr {ba}{admin_flag}{origin_flag}{relay_flag} \
         >> {sd}/serve.log 2>&1 < /dev/null & echo $! | tee {sd}/serve.pid",
        bd = spec.bin_dir,
        sd = spec.state_dir,
        ba = spec.bind_addr,
        admin_flag = admin_flag,
        origin_flag = origin_flag,
        relay_flag = relay_flag,
    );
    let out = exec_to_ok(
        client,
        target,
        &crate::relay::lxc_cmd(spec.lxc, &start),
        "start control plane",
        30,
    )?;
    let pid: u32 = out.stdout.trim().parse().map_err(|_| {
        BootstrapError::Verify(format!("start did not yield a pid: {:?}", out.stdout))
    })?;

    let mut healthy = false;
    let mut last_err: Option<String> = None;
    for _ in 0..15 {
        let probe = format!("curl -fsS -m 3 http://{ba}/healthz", ba = spec.bind_addr);
        match exec_to_ok(
            client,
            target,
            &crate::relay::lxc_cmd(spec.lxc, &probe),
            "cp healthz",
            20,
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
            "control plane did not answer /healthz on {} within the poll window{}",
            spec.bind_addr,
            last_err
                .map(|e| format!(" (last probe error: {e})"))
                .unwrap_or_default()
        )));
    }
    let alive = format!("kill -0 {pid} >/dev/null 2>&1", pid = pid);
    exec_to_ok(
        client,
        target,
        &crate::relay::lxc_cmd(spec.lxc, &alive),
        "control plane still alive",
        10,
    )
    .map_err(|_| {
        BootstrapError::Verify(format!(
            "control plane answered /healthz but the started process (pid {pid}) is gone — \
                 check {sd}/serve.log (e.g. address already in use)",
            pid = pid,
            sd = spec.state_dir,
        ))
    })?;

    // The box generated its own identity (brand new keypair); read back ONLY
    // the pubkey so the operator/CPA can add it as a relay member.
    let out = exec_to_ok(
        client,
        target,
        &crate::relay::lxc_cmd(
            spec.lxc,
            &format!(
                "{bd}/control-plane identity --state-dir {sd}",
                bd = spec.bin_dir,
                sd = spec.state_dir,
            ),
        ),
        "console identity pubkey",
        30,
    )?;
    let pubkey = out.stdout.trim().to_string();
    if pubkey.len() != 64 || !pubkey.chars().all(|c| c.is_ascii_hexdigit()) {
        return Err(BootstrapError::Verify(format!(
            "console identity pubkey readback is not 64-hex: {pubkey:?}"
        )));
    }

    // CO-LOCATED RUNNER (the architecture rule: the CP's own runner lives on
    // the CP's target). When a runner binary + an EXISTING package are given,
    // ship both, start the runner as a systemd unit, ADOPT it into the
    // console registry and self-grant the console — the dogfood state becomes
    // the deploy. Skipped entirely when not requested.
    if let (Some(rb), Some(rp)) = (&spec.runner_binary, &spec.runner_package) {
        let runner_name = rp
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_else(|| "runner".to_string());
        let runner_dir = format!("{}/runner/{runner_name}", spec.state_dir);
        ship_file(
            client,
            target,
            spec,
            rb,
            &format!("{}/freehold-runner", spec.bin_dir),
            "runner binary",
        )
        .await?;
        for f in ["identity.json", "secrets.json", "known_hosts.json"] {
            let lp = rp.join(f);
            if lp.exists() {
                ship_small_file(
                    client,
                    target,
                    spec,
                    &lp,
                    &format!("{runner_dir}/{f}"),
                    &format!("runner {f}"),
                )
                .await?;
            }
        }
        exec_to_ok(
            client,
            target,
            &crate::relay::lxc_cmd(
                spec.lxc,
                &format!(
                    "systemctl reset-failed freehold-runner 2>/dev/null; \
                     systemd-run --unit=freehold-runner --collect {bd}/freehold-runner serve \
                       --state-dir {rd} >/dev/null 2>&1; sleep 2; \
                     systemctl is-active freehold-runner",
                    bd = spec.bin_dir,
                    rd = runner_dir,
                ),
            ),
            "start co-located runner",
            60,
        )?;
        let (kind, address) = {
            let raw = std::fs::read_to_string(rp.join("secrets.json")).unwrap_or_default();
            let v: serde_json::Value =
                serde_json::from_str(&raw).unwrap_or(serde_json::Value::Null);
            let t = v["targets"]
                .as_object()
                .and_then(|m| m.values().next())
                .cloned()
                .unwrap_or(serde_json::Value::Null);
            (
                t["kind"].as_str().unwrap_or("ssh").to_string(),
                t["address"].as_str().unwrap_or("").to_string(),
            )
        };
        let adopt = format!(
            "{bd}/control-plane adopt --kind {kind} --address {address} --package-dir {rd} \
             --state-dir {sd} --mcp-addr 127.0.0.1:8787 {runner_name}",
            bd = spec.bin_dir,
            rd = runner_dir,
            sd = spec.state_dir,
        );
        exec_to_ok(
            client,
            target,
            &crate::relay::lxc_cmd(spec.lxc, &adopt),
            "adopt co-located runner",
            60,
        )?;
        let grant = format!(
            "{bd}/control-plane grant --state-dir {sd} {runner_name} {console_pk}",
            bd = spec.bin_dir,
            sd = spec.state_dir,
            console_pk = pubkey,
        );
        exec_to_ok(
            client,
            target,
            &crate::relay::lxc_cmd(spec.lxc, &grant),
            "self-grant console to co-located runner",
            60,
        )?;
    }

    // the message must agree with the BIND GUARD that allowed the bind
    // (loopback incl. ::1 / [::1]) and derive the authn claim from the
    // actual admin whitelist — never infer either.
    let is_loopback = freehold_control_plane::validate_loopback_bind(&spec.bind_addr).is_ok();
    let authn_on = !spec.admin_pubkeys.is_empty();
    let bind_hint = if is_loopback {
        format!(
            "console loopback {ba} (reach it via `ssh -L 8080:127.0.0.1:8080 root@<box>`)",
            ba = spec.bind_addr
        )
    } else {
        format!(
            "console on {ba} (LAN — the operator's proxy/path can reach it; NIP-98 auth {}on)",
            if authn_on { "" } else { "NOT " },
            ba = spec.bind_addr
        )
    };
    Ok(DeployCpResult {
        state_dir: spec.state_dir.clone(),
        bind_addr: spec.bind_addr.clone(),
        detail: format!(
            "control plane deployed in OPERATE mode: state {sd}, {bh}; relay scope {relay} \
             (C4: relay authoritative post-port, local state = offline cache mirror); \
             the box's console identity ({pk:?}) GENERATED ON THE BOX — add it as a relay \
             member with `freehold relay-member --pubkey {pk}`",
            sd = spec.state_dir,
            bh = bind_hint,
            relay = spec.relay_url,
            pk = &pubkey,
        ),
        // detail is built BEFORE the move (struct fields evaluate in order)
        pubkey,
    })
}
