//! Generic exec (Phase A4) — the heart of the runner.
//!
//! The agent writes the command; the runner executes it VERBATIM on the
//! connection it owns. Chunk 1's `local` target spawns a process on the
//! runner's own host (`sh -c <cmd>`); connector targets (ssh, vultr, b2)
//! arrive in Phase C over the same interface.
//!
//! Secrets are resolved BY NAME from the runner's ciphertext package,
//! injected as process env vars (in memory only), and their values are
//! redacted from every string returned to the agent — plaintext never leaves
//! the runner. Streaming is pull-style: `stream: true` starts a session and
//! returns a session id; the agent polls the session for accumulated (still
//! redacted) output until `done`. Every completed exec is signed into the
//! local audit log (Phase A6).

use std::collections::HashMap;
use std::path::Path;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use freehold_core::audit::Auditor;
use freehold_core::{crypto, identity::Identity, secrets::SecretPackage};
use parking_lot::Mutex;
use serde::{Deserialize, Serialize};
use thiserror::Error;
use tokio::io::AsyncReadExt;
use tokio::process::Command;
use zeroize::Zeroize as _;
use zeroize::Zeroizing;

/// Minimum secret length required for output redaction. Shorter values can't
/// be reliably removed without mangling the surrounding output; operational
/// credentials are far longer. Documented expectation, not a guarantee.
pub const MIN_REDACT_LEN: usize = 4;

#[derive(Debug, Error)]
pub enum ExecError {
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
    #[error("bad hex: {0}")]
    Hex(#[from] hex::FromHexError),
    #[error("identity error: {0}")]
    Identity(#[from] freehold_core::identity::IdentityError),
    #[error("crypto error: {0}")]
    Crypto(#[from] crypto::CryptoError),
    #[error("audit error: {0}")]
    Audit(#[from] freehold_core::audit::AuditError),
    #[error("serialization error: {0}")]
    Serde(#[from] serde_json::Error),
    #[error("exec requires field: {0}")]
    MissingField(&'static str),
    #[error("unknown secret name: {0}")]
    UnknownSecret(String),
    #[error("secret {0} could not be decrypted: {1}")]
    SecretDecrypt(String, crypto::CryptoError),
    #[error("secret {0} is not valid utf-8")]
    SecretNotUtf8(String),
    #[error("unknown target: {0} (known: local, or a shipped ssh target)")]
    UnknownTarget(String),
    #[error("ssh error: {0}")]
    Ssh(String),
    #[error("session {0} not found")]
    SessionNotFound(String),
    #[error("command timed out after {0}s")]
    TimedOut(u64),
}

/// Result of a completed command. The output fields hold RAW output until the
/// MCP layer redacts before returning to the agent; the struct is zeroized on
/// drop so transient secret-in-output bytes don't linger in freed memory.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
pub struct ExecResult {
    pub stdout: String,
    pub stderr: String,
    /// None when the process was killed by a signal (or the timeout).
    pub exit_code: Option<i32>,
    pub timed_out: bool,
}

impl Drop for ExecResult {
    fn drop(&mut self) {
        self.stdout.zeroize();
        self.stderr.zeroize();
    }
}

/// Secret name -> env var name: uppercase, non-alphanumerics become `_`
/// (`vultr-api-key` → `VULTR_API_KEY`). The agent references secrets by these
/// env names inside the command; the runner injects the values.
pub fn env_name(secret_name: &str) -> String {
    secret_name
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() {
                c.to_ascii_uppercase()
            } else {
                '_'
            }
        })
        .collect()
}

/// Decrypt the requested secrets (by name) from the runner's ciphertext
/// package. Values stay in memory (`Zeroizing`), are never logged, and are
/// only injected into the child process env or used for redaction.
pub fn resolve_secrets(
    identity: &Identity,
    pkg: &SecretPackage,
    requested: &[String],
) -> Result<Vec<(String, Zeroizing<String>)>, ExecError> {
    let enc_key = hex_to_arr(&identity.enc_secret_hex())?;
    let mut out = Vec::with_capacity(requested.len());
    for name in requested {
        let ct_hex = pkg
            .secrets
            .get(name)
            .ok_or_else(|| ExecError::UnknownSecret(name.clone()))?;
        let blob = hex::decode(ct_hex)?;
        let value = crypto::open(&enc_key, name.as_bytes(), &blob)
            .map_err(|e| ExecError::SecretDecrypt(name.clone(), e))?;
        let value = String::from_utf8(value).map_err(|_| ExecError::SecretNotUtf8(name.clone()))?;
        if value.len() < MIN_REDACT_LEN {
            // Too short to redact reliably — make the gap visible (name only).
            tracing::warn!(
                secret = %name,
                "secret shorter than {MIN_REDACT_LEN} chars cannot be redacted from output"
            );
        }
        out.push((env_name(name), Zeroizing::new(value)));
    }
    Ok(out)
}

/// Decrypt ONE secret by name (used by connectors that need the raw value —
/// e.g. an SSH private key PEM — not an env injection).
pub fn resolve_secret_value(
    identity: &Identity,
    pkg: &SecretPackage,
    name: &str,
) -> Result<Zeroizing<String>, ExecError> {
    let ct_hex = pkg
        .secrets
        .get(name)
        .ok_or_else(|| ExecError::UnknownSecret(name.to_string()))?;
    let blob = hex::decode(ct_hex)?;
    let enc_key = hex_to_arr(&identity.enc_secret_hex())?;
    let value = crypto::open(&enc_key, name.as_bytes(), &blob)
        .map_err(|e| ExecError::SecretDecrypt(name.to_string(), e))?;
    Ok(Zeroizing::new(
        String::from_utf8(value).map_err(|_| ExecError::SecretNotUtf8(name.to_string()))?,
    ))
}

/// Replace every occurrence of a secret value with `***`. Applied to ALL
/// output the runner returns (stdout, stderr, streaming chunks, errors).
pub fn redact(output: &mut String, secrets: &[(String, Zeroizing<String>)]) {
    for (_, value) in secrets {
        if value.len() >= MIN_REDACT_LEN {
            let replaced = output.replace(value.as_str(), "***");
            *output = replaced;
        }
    }
}

/// Run `cmd` verbatim on the local target, collecting full output.
async fn run_local(
    cmd: &str,
    envs: &[(String, Zeroizing<String>)],
    timeout_s: Option<u64>,
) -> Result<ExecResult, ExecError> {
    let mut child = spawn(cmd, envs)?;
    let out_task = tokio::spawn(read_to_string(child.stdout.take().expect("piped stdout")));
    let err_task = tokio::spawn(read_to_string(child.stderr.take().expect("piped stderr")));

    let (exit_code, timed_out) = match timeout_s {
        Some(t) => match tokio::time::timeout(Duration::from_secs(t), child.wait()).await {
            Ok(Ok(st)) => (st.code(), false),
            Ok(Err(e)) => return Err(e.into()),
            Err(_) => {
                let _ = child.kill().await;
                let _ = child.wait().await;
                (None, true)
            }
        },
        None => match child.wait().await {
            Ok(st) => (st.code(), false),
            Err(e) => return Err(e.into()),
        },
    };

    // With a timeout requested, the WHOLE exec is bounded: a forked daemon
    // inheriting the pipe must not extend it past the grace (the
    // `sh -c "cmd &"` case, where the shell itself exits instantly). Without
    // a timeout, wait for the complete output.
    let (stdout, stderr) = if timeout_s.is_some() {
        bounded_collect(out_task, err_task).await
    } else {
        (join_read(out_task).await?, join_read(err_task).await?)
    };

    Ok(ExecResult {
        stdout,
        stderr,
        exit_code,
        timed_out,
    })
}

/// Grace period after a kill: the killed command may have descendants still
/// holding the pipe; never wait on them forever.
const JOIN_GRACE: Duration = Duration::from_secs(2);

/// Await a spawned stdout/stderr reader, mapping the join error into ExecError.
async fn join_read(
    task: tokio::task::JoinHandle<std::io::Result<String>>,
) -> Result<String, ExecError> {
    match task.await {
        Ok(Ok(s)) => Ok(s),
        Ok(Err(e)) => Err(e.into()),
        Err(e) => Err(ExecError::Io(std::io::Error::other(e.to_string()))),
    }
}

fn reader_value(r: Result<std::io::Result<String>, tokio::task::JoinError>) -> String {
    match r {
        Ok(Ok(s)) => s,
        _ => String::new(),
    }
}

/// Collect both readers under ONE grace deadline. Used whenever a timeout was
/// requested: a forked daemon inheriting both pipes must not extend the exec
/// past the grace on either stream.
async fn bounded_collect(
    out_task: tokio::task::JoinHandle<std::io::Result<String>>,
    err_task: tokio::task::JoinHandle<std::io::Result<String>>,
) -> (String, String) {
    let both = async {
        let (o, e) = tokio::join!(out_task, err_task);
        (reader_value(o), reader_value(e))
    };
    tokio::time::timeout(JOIN_GRACE, both)
        .await
        .unwrap_or_else(|_| (String::new(), String::new()))
}

async fn read_to_string<R: tokio::io::AsyncRead + Unpin>(mut r: R) -> std::io::Result<String> {
    let mut s = String::new();
    r.read_to_string(&mut s).await?;
    Ok(s)
}

/// Streaming UTF-8 decoder with a trailing-byte carry: a multi-byte char
/// split across two reads must not drop the whole chunk (the 4 KB-drop bug).
#[derive(Default)]
struct Utf8Carry {
    pending: Vec<u8>,
}

impl Utf8Carry {
    fn push(&mut self, bytes: &[u8], out: &mut String) {
        self.pending.extend_from_slice(bytes);
        match std::str::from_utf8(&self.pending) {
            Ok(s) => {
                out.push_str(s);
                self.pending.clear();
            }
            Err(e) => {
                let valid_up_to = e.valid_up_to();
                if valid_up_to > 0 {
                    out.push_str(
                        std::str::from_utf8(&self.pending[..valid_up_to])
                            .expect("valid_up_to is a UTF-8 boundary"),
                    );
                }
                match e.error_len() {
                    // Invalid sequence: drop it, keep the rest.
                    Some(n) => {
                        self.pending.drain(..valid_up_to + n);
                    }
                    // Incomplete at the end: keep the tail for the next read.
                    None => {
                        self.pending.drain(..valid_up_to);
                    }
                }
            }
        }
    }

    fn finish(&mut self, out: &mut String) {
        if !self.pending.is_empty() {
            out.push_str(&String::from_utf8_lossy(&self.pending));
            self.pending.clear();
        }
    }
}

fn spawn(
    cmd: &str,
    envs: &[(String, Zeroizing<String>)],
) -> std::io::Result<tokio::process::Child> {
    Command::new("sh")
        .arg("-c")
        .arg(cmd)
        .envs(envs.iter().map(|(k, v)| (k.as_str(), v.as_str())))
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .kill_on_drop(true)
        .spawn()
}

// ---------------------------------------------------------------------------
// Pull-style streaming sessions
// ---------------------------------------------------------------------------

#[derive(Debug, Default, Serialize, Deserialize)]
pub struct StreamSnapshot {
    pub session_id: String,
    /// Accumulated output so far, redacted.
    pub output: String,
    pub done: bool,
    pub result: Option<ExecResult>,
    /// Set when the streaming exec itself failed to start/run — distinguishes
    /// a genuine failure from a command that simply printed nothing.
    pub error: Option<String>,
}

struct Session {
    raw_output: Arc<Mutex<String>>,
    secrets: Arc<Vec<(String, Zeroizing<String>)>>,
    done: Arc<AtomicBool>,
    result: Arc<Mutex<Option<ExecResult>>>,
    error: Arc<Mutex<Option<String>>>,
}

impl Session {
    fn snapshot(&self, session_id: &str) -> StreamSnapshot {
        // Load `done` BEFORE cloning the buffer: the stream task appends the
        // final bytes and only then stores `done` — reading `done` first
        // guarantees the snapshot never sees `done: true` with a truncated
        // tail (the poll that deletes the session would otherwise lose it).
        let done = self.done.load(Ordering::SeqCst);
        let mut output = self.raw_output.lock().clone();
        redact(&mut output, self.secrets.as_slice());
        StreamSnapshot {
            session_id: session_id.to_string(),
            output,
            done,
            result: self.result.lock().clone(),
            error: self.error.lock().clone(),
        }
    }
}

/// Owns exec sessions and the audit sink. One per runner process.
pub struct ExecManager {
    sessions: Mutex<HashMap<String, Arc<Session>>>,
    next_id: AtomicU64,
    auditor: Option<Arc<Auditor>>,
    state_dir: Option<Arc<Path>>,
    /// Relay for Phase D5 audit publishing (kind 48001, same event as the
    /// local spool); None = local spool only.
    relay_url: Option<String>,
    /// Canonical NIP-98 URL when `relay_url` is a LAN dial (None = sign the
    /// dial URL).
    relay_auth_url: Option<String>,
}

impl Default for ExecManager {
    fn default() -> Self {
        Self::new(None, None, None, None)
    }
}

impl ExecManager {
    pub fn new(
        auditor: Option<Arc<Auditor>>,
        state_dir: Option<Arc<Path>>,
        relay_url: Option<String>,
        relay_auth_url: Option<String>,
    ) -> Self {
        Self {
            sessions: Mutex::new(HashMap::new()),
            next_id: AtomicU64::new(1),
            auditor,
            state_dir,
            relay_url,
            relay_auth_url,
        }
    }

    /// Run a non-streaming exec to completion on the local target, then sign
    /// it into the audit log. `caller` is the verified agent pubkey.
    pub async fn run(
        &self,
        cmd: &str,
        target: &str,
        secrets: Vec<(String, Zeroizing<String>)>,
        timeout_s: Option<u64>,
        caller: Option<&str>,
    ) -> Result<ExecResult, ExecError> {
        let started = now_secs();
        let result = run_local(cmd, &secrets, timeout_s).await?;
        self.audit(cmd, target, &result, started, caller);
        Ok(result)
    }

    /// Readiness self-check: runs `cmd` WITHOUT audit (a status probe is not a
    /// privileged exec record).
    pub async fn self_check(&self, cmd: &str) -> Result<ExecResult, ExecError> {
        run_local(cmd, &[], None).await
    }

    /// Local run WITHOUT audit: self-check probes against owned services
    /// (a probe is not a privileged exec record).
    pub async fn probe_env(
        &self,
        cmd: &str,
        envs: &[(String, Zeroizing<String>)],
    ) -> Result<ExecResult, ExecError> {
        run_local(cmd, envs, None).await
    }

    /// Start a streaming exec on the local target; returns the session id.
    /// Audit is signed when the spawned task completes.
    pub fn start_streaming(
        &self,
        cmd: &str,
        target: &str,
        secrets: Vec<(String, Zeroizing<String>)>,
        timeout_s: Option<u64>,
        caller: Option<&str>,
    ) -> String {
        let id = format!("{:016x}", self.next_id.fetch_add(1, Ordering::SeqCst));
        let secrets = Arc::new(secrets);
        let session = Arc::new(Session {
            raw_output: Arc::new(Mutex::new(String::new())),
            secrets: secrets.clone(),
            done: Arc::new(AtomicBool::new(false)),
            result: Arc::new(Mutex::new(None)),
            error: Arc::new(Mutex::new(None)),
        });
        self.sessions.lock().insert(id.clone(), session.clone());

        let raw = session.raw_output.clone();
        let done = session.done.clone();
        let result_slot = session.result.clone();
        let error_slot = session.error.clone();
        let cmd = cmd.to_string();
        let target = target.to_string();
        let started = now_secs();
        let auditor = self.auditor.clone();
        let state_dir = self.state_dir.clone();
        let caller = caller.map(String::from);
        tokio::spawn(async move {
            let result = run_local_streaming(&cmd, timeout_s, secrets, raw).await;
            match &result {
                Ok(r) => {
                    *result_slot.lock() = Some(r.clone());
                    if let (Some(a), Some(d)) = (&auditor, &state_dir)
                        && let Err(e) =
                            write_audit(d, a, &cmd, &target, r, started, caller.as_deref())
                    {
                        tracing::warn!(error = %e, "audit write failed (exec still succeeded)");
                    }
                }
                Err(e) => {
                    // Surface the failure to the agent: done + error, not a
                    // look-alike of a silent-empty command.
                    *error_slot.lock() = Some(e.to_string());
                }
            }
            done.store(true, Ordering::SeqCst);
        });
        id
    }

    /// Poll a streaming session; removes it once done.
    pub fn poll(&self, session_id: &str) -> Result<StreamSnapshot, ExecError> {
        let session = self
            .sessions
            .lock()
            .get(session_id)
            .cloned()
            .ok_or_else(|| ExecError::SessionNotFound(session_id.to_string()))?;
        let snap = session.snapshot(session_id);
        if snap.done {
            self.sessions.lock().remove(session_id);
        }
        Ok(snap)
    }

    /// Sign + append an audit record for any completed exec (used by the MCP
    /// layer for connector paths outside this manager, e.g. ssh).
    pub fn audit_cmd(
        &self,
        cmd: &str,
        target: &str,
        result: &ExecResult,
        started_at: u64,
        caller: Option<&str>,
    ) {
        self.audit(cmd, target, result, started_at, caller)
    }

    fn audit(
        &self,
        cmd: &str,
        target: &str,
        result: &ExecResult,
        started_at: u64,
        caller: Option<&str>,
    ) {
        let (Some(auditor), Some(state_dir)) = (&self.auditor, &self.state_dir) else {
            return;
        };
        match write_audit(state_dir, auditor, cmd, target, result, started_at, caller) {
            Ok(event) => {
                // Phase D5: additive relay copy, detached + surfaced on failure.
                if let Some(url) = &self.relay_url {
                    let auth = self.relay_auth_url.as_deref().unwrap_or(url);
                    spawn_audit_publish(url, auth, auditor, event);
                }
            }
            Err(e) => {
                tracing::warn!(error = %e, "audit write failed (exec still succeeded)");
            }
        }
    }
}

/// Streaming run: appends raw output chunks to `sink` as they arrive. The
/// returned ExecResult carries only exit status — its output fields are empty
/// (the raw stream lives in the session buffer and is redacted at poll time).
async fn run_local_streaming(
    cmd: &str,
    timeout_s: Option<u64>,
    envs: Arc<Vec<(String, Zeroizing<String>)>>,
    sink: Arc<Mutex<String>>,
) -> Result<ExecResult, ExecError> {
    let mut child = spawn(cmd, envs.as_slice())?;
    let mut stdout = child.stdout.take().expect("piped stdout");
    let mut stderr = child.stderr.take().expect("piped stderr");

    let sink_a = sink.clone();
    let stdout_reader = tokio::spawn(async move {
        let mut buf = [0u8; 4096];
        let mut carry = Utf8Carry::default();
        loop {
            match stdout.read(&mut buf).await {
                Ok(0) => break,
                Ok(n) => {
                    let mut out = sink_a.lock();
                    carry.push(&buf[..n], &mut out);
                }
                Err(_) => break,
            }
        }
        let mut out = sink_a.lock();
        carry.finish(&mut out);
    });
    let sink_b = sink.clone();
    let stderr_reader = tokio::spawn(async move {
        let mut buf = [0u8; 4096];
        let mut carry = Utf8Carry::default();
        loop {
            match stderr.read(&mut buf).await {
                Ok(0) => break,
                Ok(n) => {
                    let mut out = sink_b.lock();
                    carry.push(&buf[..n], &mut out);
                }
                Err(_) => break,
            }
        }
        let mut out = sink_b.lock();
        carry.finish(&mut out);
    });

    let (exit_code, timed_out) = match timeout_s {
        Some(t) => match tokio::time::timeout(Duration::from_secs(t), child.wait()).await {
            Ok(Ok(st)) => (st.code(), false),
            Ok(Err(e)) => return Err(e.into()),
            Err(_) => {
                let _ = child.kill().await;
                let _ = child.wait().await;
                (None, true)
            }
        },
        None => match child.wait().await {
            Ok(st) => (st.code(), false),
            Err(e) => return Err(e.into()),
        },
    };
    // Bounded even here: a descendant holding the pipe after a kill must not
    // keep `done` from ever being set.
    let _ = tokio::time::timeout(JOIN_GRACE, async {
        let _ = (stdout_reader.await, stderr_reader.await);
    })
    .await;

    Ok(ExecResult {
        stdout: String::new(),
        stderr: String::new(),
        exit_code,
        timed_out,
    })
}

// ---------------------------------------------------------------------------
// Audit (Phase A6): signed events appended to a local 0600 log
// ---------------------------------------------------------------------------

pub const AUDIT_FILE: &str = "audit.log";

pub fn now_secs() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

fn now_millis() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

/// Sign and append one audit record for a completed exec. Phase D5: the
/// event is a NIP-01 kind-48001 event (id over the canonical serialization,
/// BIP-340 signature over the id) — the SAME bytes spooled here as JSON line
/// to `<state_dir>/audit.log` (0600) AND published to the relay when
/// `relay_url` is set (additive; publish failure degrades to spool-only and
/// is surfaced, never silent-ok).
pub fn write_audit(
    state_dir: &Path,
    auditor: &Auditor,
    cmd: &str,
    target: &str,
    result: &ExecResult,
    started_at: u64,
    caller: Option<&str>,
) -> Result<serde_json::Value, ExecError> {
    let content = serde_json::json!({
        "cmd": cmd,
        "target": target,
        "agent_pubkey": caller.unwrap_or("unknown"),
        "exit_code": result.exit_code,
        "timed_out": result.timed_out,
        "started_at": started_at,
        "duration_ms": now_millis().saturating_sub(started_at.saturating_mul(1000)),
    })
    .to_string();

    let tags = caller
        .map(|c| vec![vec!["p".to_string(), c.to_string()]])
        .unwrap_or_default();
    let event = auditor.event(48001, tags, &content)?;
    freehold_core::futil::ensure_private_dir(state_dir)?;
    let path = state_dir.join(AUDIT_FILE);
    let mut opts = std::fs::OpenOptions::new();
    opts.create(true).append(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    let mut f = opts.open(&path)?;
    use std::io::Write;
    f.write_all(serde_json::to_string(&event)?.as_bytes())?;
    f.write_all(b"\n")?;
    f.flush()?;
    Ok(event)
}

/// Fire the spooled audit event at the relay, detached: the agent's exec
/// result must NEVER wait on a wedged relay. Failure is surfaced (warn) and
/// the local spool stays authoritative — never silently dropped, never a
/// hard failure of the executed command.
/// Publish one audit event and REPORT the outcome. Sync + testable on the
/// calling thread (the surfaced rule is a unit-testable contract, not a
/// thread artifact): both the publish error and success are observable.
pub fn report_audit_publish(relay_url: &str, secret: &[u8; 32], event_json: &str) {
    report_audit_publish_auth(relay_url, relay_url, secret, event_json)
}

/// [`report_audit_publish`] with a separate NIP-98 auth URL.
pub fn report_audit_publish_auth(
    dial_url: &str,
    auth_url: &str,
    secret: &[u8; 32],
    event_json: &str,
) {
    match freehold_core::relay_http::publish_event_json_auth(dial_url, auth_url, secret, event_json)
    {
        Ok(()) => {}
        Err(e) => tracing::warn!(error = %e, "audit relay publish failed — local spool only"),
    }
}

/// Fire the publish detached (spawn_blocking): the agent's exec result must
/// NEVER wait on a wedged relay. The join failure is ALSO surfaced — the
/// surfaced rule covers the task boundary too.
pub fn spawn_audit_publish(
    relay_url: &str,
    relay_auth_url: &str,
    auditor: &Auditor,
    event: serde_json::Value,
) {
    let url = relay_url.to_string();
    let auth = relay_auth_url.to_string();
    let secret = auditor.secret();
    let ev = event.to_string();
    tokio::spawn(async move {
        match tokio::task::spawn_blocking(move || {
            report_audit_publish_auth(&url, &auth, &secret, &ev)
        })
        .await
        {
            Ok(()) => {}
            Err(e) => tracing::warn!(error = %e, "audit publish task panicked/failed"),
        }
    });
}

#[inline]
fn hex_to_arr(s: &str) -> Result<[u8; 32], ExecError> {
    let bytes = hex::decode(s)?;
    if bytes.len() != 32 {
        return Err(ExecError::Hex(hex::FromHexError::InvalidStringLength));
    }
    let mut arr = [0u8; 32];
    arr.copy_from_slice(&bytes);
    Ok(arr)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;

    use freehold_core::secrets::SecretPackage;

    /// Ship a runner dir the way the CP would: identity + one sealed secret.
    fn sealed_runner_dir(secret_name: &str, secret_value: &str) -> (tempfile::TempDir, Identity) {
        let dir = tempfile::tempdir().unwrap();
        let id = Identity::generate();
        id.write_to_dir(dir.path()).unwrap();
        let enc_pub = hex_to_arr(&id.enc_pubkey_hex()).unwrap();
        let blob = crypto::seal(&enc_pub, secret_name.as_bytes(), secret_value.as_bytes()).unwrap();
        let pkg = SecretPackage {
            secrets: BTreeMap::from([(secret_name.to_string(), hex::encode(blob))]),
            targets: BTreeMap::new(),
            grants: Vec::new(),
        };
        pkg.write_to_dir(dir.path()).unwrap();
        (dir, id)
    }

    #[tokio::test]
    async fn run_local_echo() {
        let res = run_local("echo hello-exec", &[], None).await.unwrap();
        assert_eq!(res.stdout.trim(), "hello-exec");
        assert_eq!(res.exit_code, Some(0));
    }

    #[tokio::test]
    async fn timeout_kills_the_command() {
        let res = run_local("sleep 5", &[], Some(1)).await.unwrap();
        assert!(res.timed_out, "must be flagged as timed out");
        assert_eq!(res.exit_code, None);
    }

    #[test]
    fn env_name_mapping() {
        assert_eq!(env_name("vultr-api-key"), "VULTR_API_KEY");
        assert_eq!(env_name("b2"), "B2");
        assert_eq!(env_name("my secret!"), "MY_SECRET_");
        assert_eq!(env_name("a.b_c"), "A_B_C");
    }

    #[tokio::test]
    async fn secrets_resolve_decrypt_and_redact() {
        let (dir, id) = sealed_runner_dir("vultr-api-key", "sk-abc123xyz");
        let pkg = SecretPackage::load(dir.path()).unwrap();
        let envs = resolve_secrets(&id, &pkg, &["vultr-api-key".to_string()]).unwrap();
        assert_eq!(envs[0].0, "VULTR_API_KEY");

        // The command reads the value via the injected env var...
        let res = run_local("printf %s \"$VULTR_API_KEY\"", &envs, None)
            .await
            .unwrap();
        assert_eq!(res.stdout, "sk-abc123xyz");
        // ...and redaction strips it before anything returns to the agent.
        let mut out = res.stdout.clone();
        redact(&mut out, &envs);
        assert!(
            !out.contains("sk-abc123xyz"),
            "value must be redacted: {out}"
        );

        // Unknown names and wrong keys fail cleanly.
        assert!(matches!(
            resolve_secrets(&id, &pkg, &["nope".to_string()]),
            Err(ExecError::UnknownSecret(_))
        ));
    }

    #[tokio::test]
    async fn streaming_session_polls_until_done() {
        let (dir, id) = sealed_runner_dir("b2", "b2key-123456");
        let pkg = SecretPackage::load(dir.path()).unwrap();
        let envs = resolve_secrets(&id, &pkg, &["b2".to_string()]).unwrap();
        let mgr = ExecManager::new(None, None, None, None);
        let sid = mgr.start_streaming(
            "for i in 1 2 3 4 5; do echo line-$i; sleep 0.05; done",
            "local",
            envs,
            Some(10),
            None,
        );
        let mut all;
        loop {
            let snap = mgr.poll(&sid).unwrap();
            all = snap.output.clone();
            if snap.done {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        for i in 1..=5 {
            assert!(
                all.contains(&format!("line-{i}")),
                "missing line {i}: {all}"
            );
        }
        // Session is removed once done.
        assert!(matches!(mgr.poll(&sid), Err(ExecError::SessionNotFound(_))));
    }

    #[tokio::test]
    async fn streaming_redacts_secret_values() {
        let (dir, id) = sealed_runner_dir("tok", "super-secret-token-9911");
        let pkg = SecretPackage::load(dir.path()).unwrap();
        let envs = resolve_secrets(&id, &pkg, &["tok".to_string()]).unwrap();
        let mgr = ExecManager::new(None, None, None, None);
        let sid = mgr.start_streaming("echo leaked-$TOK", "local", envs, Some(10), None);
        let mut all;
        loop {
            let snap = mgr.poll(&sid).unwrap();
            all = snap.output.clone();
            if snap.done {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        assert!(
            !all.contains("super-secret-token-9911"),
            "secret leaked into streamed output: {all}"
        );
        assert!(
            all.contains("leaked-***"),
            "redaction marker expected: {all}"
        );
    }

    #[tokio::test]
    async fn timeout_bounds_even_when_the_shell_exits_early() {
        // `sh -c "sleep 5 & echo bg"` exits instantly but the forked daemon
        // inherits the pipe — without a bounded join this hangs forever even
        // though the timeout never fires.
        let start = std::time::Instant::now();
        let res = run_local("sleep 5 & echo bg-started", &[], Some(1))
            .await
            .unwrap();
        assert!(
            start.elapsed() < Duration::from_secs(4),
            "must not hang on a forked daemon holding the pipe"
        );
        assert_eq!(res.exit_code, Some(0), "sh itself exited cleanly");
        assert!(!res.timed_out, "the shell exited before the timeout");
    }

    #[tokio::test]
    async fn streaming_survives_multibyte_chunk_boundaries() {
        // Multi-byte UTF-8 straddling a 4096-byte read boundary must not drop
        // the whole chunk (the 4 KB-drop bug): 5000 'é' outputs ~15 KB, so
        // several characters cross boundaries.
        let mgr = ExecManager::new(None, None, None, None);
        let sid = mgr.start_streaming(
            "python3 -c \"print('é' * 5000)\"",
            "local",
            vec![],
            Some(10),
            None,
        );
        let mut all;
        let mut attempts = 0;
        loop {
            let snap = mgr.poll(&sid).unwrap();
            all = snap.output.clone();
            if snap.done {
                break;
            }
            attempts += 1;
            assert!(attempts < 200, "stream never finished");
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        let e_count = all.matches('é').count();
        assert_eq!(
            e_count, 5000,
            "all multibyte chars must survive: {e_count}/5000"
        );
        assert!(!all.contains('\u{fffd}'), "no replacement chars in output");
    }

    #[tokio::test]
    async fn audit_log_recorded_and_verifies() {
        let (dir, id) = sealed_runner_dir("k", "key-value-12345678");
        let auditor = Arc::new(Auditor::new(hex_to_arr(&id.nostr_secret_hex()).unwrap()));
        let state_dir = dir.path().to_path_buf();
        let mgr = ExecManager::new(
            Some(auditor),
            Some(Arc::from(state_dir.as_path())),
            None,
            None,
        );
        let _ = mgr
            .run("echo audited", "local", vec![], None, None)
            .await
            .unwrap();

        let raw = std::fs::read_to_string(dir.path().join(AUDIT_FILE)).unwrap();
        let event: serde_json::Value = serde_json::from_str(raw.trim()).unwrap();
        // Phase D5: the spooled row is a kind-48001 NIP-01 event (id + BIP-340
        // over the id) — the SAME shape the relay stores; G3.1's existence
        // check keeps passing, and the row is relay-verifiable as-is.
        assert_eq!(event["kind"].as_u64(), Some(48001));
        assert_eq!(event["pubkey"].as_str().unwrap(), &id.nostr_pubkey_hex());
        assert!(event["content"].as_str().unwrap().contains("audited"));
        assert!(
            event["content"]
                .as_str()
                .unwrap()
                .contains("\"target\":\"local\"")
        );
        freehold_core::nip98::verify_event(
            event["pubkey"].as_str().unwrap(),
            event["created_at"].as_i64().unwrap(),
            event["kind"].as_u64().unwrap() as u32,
            &[],
            event["content"].as_str().unwrap(),
            event["sig"].as_str().unwrap(),
        )
        .expect("kind-48001 row verifies (id + BIP-340)");
    }
}
