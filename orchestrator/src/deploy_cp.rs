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

pub const DEFAULT_CP_STATE_DIR: &str = "/srv/freehold/control-plane";
pub const DEFAULT_CP_BIN_DIR: &str = "/srv/freehold/bin";
pub const DEFAULT_CP_BIND: &str = "127.0.0.1:8080";

/// base64 chunk size written per exec (kept well under the transport frame;
/// a chunk is one `printf` of pure base64 — shell-safe).
const CHUNK: usize = 24_000;

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

    let binary = std::fs::read(&spec.binary_path)?;

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

    // Binary as base64 chunks appended to one .b64 file, then decoded once
    // (the .b64 was truncated in the mkdir step, so an interrupted deploy
    // can never wedge the next one behind a false size mismatch).
    let b64 = base64::engine::general_purpose::STANDARD.encode(&binary);
    let mut sent = 0usize;
    while sent < b64.len() {
        let end = (sent + CHUNK).min(b64.len());
        let piece = &b64[sent..end];
        exec_to_ok(
            client,
            target,
            &crate::relay::lxc_cmd(
                spec.lxc,
                &format!(
                    "printf %s \"{piece}\" >> {bd}/control-plane.b64",
                    piece = piece,
                    bd = spec.bin_dir
                ),
            ),
            "ship binary chunk",
            60,
        )?;
        sent = end;
    }
    let out = exec_to_ok(
        client,
        target,
        &crate::relay::lxc_cmd(
            spec.lxc,
            &format!(
                "base64 -d {bd}/control-plane.b64 > {bd}/control-plane && \
                 chmod 755 {bd}/control-plane && rm {bd}/control-plane.b64 && \
                 wc -c < {bd}/control-plane",
                bd = spec.bin_dir,
            ),
        ),
        "decode + verify binary",
        60,
    )?;
    let remote_size: usize = out.stdout.trim().parse().map_err(|_| {
        BootstrapError::Verify(format!("remote binary size not a number: {:?}", out.stdout))
    })?;
    if remote_size != binary.len() {
        return Err(BootstrapError::Verify(format!(
            "shipped binary size mismatch: remote {remote_size} vs local {}",
            binary.len()
        )));
    }

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
    let start = format!(
        "setsid nohup {bd}/control-plane serve --state-dir {sd} --addr {ba}{admin_flag}{origin_flag} \
         >> {sd}/serve.log 2>&1 < /dev/null & echo $! | tee {sd}/serve.pid",
        bd = spec.bin_dir,
        sd = spec.state_dir,
        ba = spec.bind_addr,
        admin_flag = admin_flag,
        origin_flag = origin_flag,
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

    Ok(DeployCpResult {
        state_dir: spec.state_dir.clone(),
        bind_addr: spec.bind_addr.clone(),
        detail: format!(
            "control plane deployed in OPERATE mode: state {sd}, console loopback {ba} \
             (reach it via `ssh -L 8080:127.0.0.1:8080 root@<box>`); relay scope {relay} \
             (C4: relay authoritative post-port, local state = offline cache mirror); \
             the box's console identity ({pk:?}) GENERATED ON THE BOX — add it as a relay \
             member with `freehold relay-member --pubkey {pk}`",
            sd = spec.state_dir,
            ba = spec.bind_addr,
            relay = spec.relay_url,
            pk = &pubkey,
        ),
        // detail is built BEFORE the move (struct fields evaluate in order)
        pubkey,
    })
}
