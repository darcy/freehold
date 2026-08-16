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

use freehold_core::audit::{Auditor, SignedEvent};
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
    #[error("unknown target: {0} (known: local)")]
    UnknownTarget(String),
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
        out.push((env_name(name), Zeroizing::new(value)));
    }
    Ok(out)
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

    let status = match timeout_s {
        Some(t) => match tokio::time::timeout(Duration::from_secs(t), child.wait()).await {
            Ok(st) => st?,
            Err(_) => {
                let _ = child.kill().await;
                let _ = child.wait().await;
                return Ok(ExecResult {
                    stdout: join_read(out_task).await?,
                    stderr: join_read(err_task).await?,
                    exit_code: None,
                    timed_out: true,
                });
            }
        },
        None => child.wait().await?,
    };
    Ok(ExecResult {
        stdout: join_read(out_task).await?,
        stderr: join_read(err_task).await?,
        exit_code: status.code(),
        timed_out: false,
    })
}

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

async fn read_to_string<R: tokio::io::AsyncRead + Unpin>(mut r: R) -> std::io::Result<String> {
    let mut s = String::new();
    r.read_to_string(&mut s).await?;
    Ok(s)
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
}

struct Session {
    raw_output: Arc<Mutex<String>>,
    secrets: Arc<Vec<(String, Zeroizing<String>)>>,
    done: Arc<AtomicBool>,
    result: Arc<Mutex<Option<ExecResult>>>,
}

impl Session {
    fn snapshot(&self, session_id: &str) -> StreamSnapshot {
        let mut output = self.raw_output.lock().clone();
        redact(&mut output, self.secrets.as_slice());
        StreamSnapshot {
            session_id: session_id.to_string(),
            output,
            done: self.done.load(Ordering::SeqCst),
            result: self.result.lock().clone(),
        }
    }
}

/// Owns exec sessions and the audit sink. One per runner process.
pub struct ExecManager {
    sessions: Mutex<HashMap<String, Arc<Session>>>,
    next_id: AtomicU64,
    auditor: Option<Arc<Auditor>>,
    state_dir: Option<Arc<Path>>,
}

impl Default for ExecManager {
    fn default() -> Self {
        Self::new(None, None)
    }
}

impl ExecManager {
    pub fn new(auditor: Option<Arc<Auditor>>, state_dir: Option<Arc<Path>>) -> Self {
        Self {
            sessions: Mutex::new(HashMap::new()),
            next_id: AtomicU64::new(1),
            auditor,
            state_dir,
        }
    }

    /// Run a non-streaming exec to completion on the local target, then sign
    /// it into the audit log.
    pub async fn run(
        &self,
        cmd: &str,
        target: &str,
        secrets: Vec<(String, Zeroizing<String>)>,
        timeout_s: Option<u64>,
    ) -> Result<ExecResult, ExecError> {
        let started = now_secs();
        let result = run_local(cmd, &secrets, timeout_s).await?;
        self.audit(cmd, target, &result, started);
        Ok(result)
    }

    /// Readiness self-check: runs `cmd` WITHOUT audit (a status probe is not a
    /// privileged exec record).
    pub async fn self_check(&self, cmd: &str) -> Result<ExecResult, ExecError> {
        run_local(cmd, &[], None).await
    }

    /// Start a streaming exec on the local target; returns the session id.
    /// Audit is signed when the spawned task completes.
    pub fn start_streaming(
        &self,
        cmd: &str,
        target: &str,
        secrets: Vec<(String, Zeroizing<String>)>,
        timeout_s: Option<u64>,
    ) -> String {
        let id = format!("{:016x}", self.next_id.fetch_add(1, Ordering::SeqCst));
        let secrets = Arc::new(secrets);
        let session = Arc::new(Session {
            raw_output: Arc::new(Mutex::new(String::new())),
            secrets: secrets.clone(),
            done: Arc::new(AtomicBool::new(false)),
            result: Arc::new(Mutex::new(None)),
        });
        self.sessions.lock().insert(id.clone(), session.clone());

        let raw = session.raw_output.clone();
        let done = session.done.clone();
        let result_slot = session.result.clone();
        let cmd = cmd.to_string();
        let target = target.to_string();
        let started = now_secs();
        let auditor = self.auditor.clone();
        let state_dir = self.state_dir.clone();
        tokio::spawn(async move {
            let result = run_local_streaming(&cmd, timeout_s, secrets, raw).await;
            match &result {
                Ok(r) => {
                    *result_slot.lock() = Some(r.clone());
                    if let (Some(a), Some(d)) = (&auditor, &state_dir)
                        && let Err(e) = write_audit(d, a, &cmd, &target, r, started)
                    {
                        tracing::warn!(error = %e, "audit write failed (exec still succeeded)");
                    }
                }
                Err(e) => tracing::warn!(error = %e, "streaming exec failed"),
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

    fn audit(&self, cmd: &str, target: &str, result: &ExecResult, started_at: u64) {
        let (Some(auditor), Some(state_dir)) = (&self.auditor, &self.state_dir) else {
            return;
        };
        if let Err(e) = write_audit(state_dir, auditor, cmd, target, result, started_at) {
            tracing::warn!(error = %e, "audit write failed (exec still succeeded)");
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
        loop {
            match stdout.read(&mut buf).await {
                Ok(0) => break,
                Ok(n) => {
                    if let Ok(s) = std::str::from_utf8(&buf[..n]) {
                        sink_a.lock().push_str(s);
                    }
                }
                Err(_) => break,
            }
        }
    });
    let sink_b = sink.clone();
    let stderr_reader = tokio::spawn(async move {
        let mut buf = [0u8; 4096];
        loop {
            match stderr.read(&mut buf).await {
                Ok(0) => break,
                Ok(n) => {
                    if let Ok(s) = std::str::from_utf8(&buf[..n]) {
                        sink_b.lock().push_str(s);
                    }
                }
                Err(_) => break,
            }
        }
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
    let _ = tokio::join!(stdout_reader, stderr_reader);

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

fn now_secs() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// Sign and append one audit record for a completed exec. The runner's Nostr
/// key signs `{cmd, target, exit_code, timed_out, started_at, duration_ms}`;
/// appended as a JSON line to `<state_dir>/audit.log` (0600). This same event
/// shape ports to a relay in Chunk 2.
pub fn write_audit(
    state_dir: &Path,
    auditor: &Auditor,
    cmd: &str,
    target: &str,
    result: &ExecResult,
    started_at: u64,
) -> Result<SignedEvent, ExecError> {
    let content = serde_json::json!({
        "cmd": cmd,
        "target": target,
        "exit_code": result.exit_code,
        "timed_out": result.timed_out,
        "started_at": started_at,
        "duration_ms": now_secs().saturating_sub(started_at).saturating_mul(1000),
    })
    .to_string();

    let event = auditor.sign(&content)?;
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
        let mgr = ExecManager::new(None, None);
        let sid = mgr.start_streaming(
            "for i in 1 2 3 4 5; do echo line-$i; sleep 0.05; done",
            "local",
            envs,
            Some(10),
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
        let mgr = ExecManager::new(None, None);
        let sid = mgr.start_streaming("echo leaked-$TOK", "local", envs, Some(10));
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
    async fn audit_log_recorded_and_verifies() {
        let (dir, id) = sealed_runner_dir("k", "key-value-12345678");
        let auditor = Arc::new(Auditor::new(hex_to_arr(&id.nostr_secret_hex()).unwrap()));
        let state_dir = dir.path().to_path_buf();
        let mgr = ExecManager::new(Some(auditor), Some(Arc::from(state_dir.as_path())));
        let _ = mgr
            .run("echo audited", "local", vec![], None)
            .await
            .unwrap();

        let raw = std::fs::read_to_string(dir.path().join(AUDIT_FILE)).unwrap();
        let event: SignedEvent = serde_json::from_str(raw.trim()).unwrap();
        assert!(
            freehold_core::audit::verify_event(&event).is_ok(),
            "audit event must verify against the runner pubkey"
        );
        assert!(
            event.content.contains("audited"),
            "content: {}",
            event.content
        );
        assert!(
            event.content.contains("\"target\":\"local\""),
            "content: {}",
            event.content
        );
    }
}
