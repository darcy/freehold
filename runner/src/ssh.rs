//! SSH connector (Phase C1): the runner reaches a remote host with an
//! IN-MEMORY private key and executes the agent's command verbatim over a
//! persistent SSH session — the doc's "cheap repeated commands" via a
//! connection pool (the runner owns connection lifetime, never the agent).
//!
//! Secrets: the private key PEM is the service credential; it lives in the
//! runner's memory only (decrypted from ciphertext, parsed, never written to
//! disk). Host keys are TOFU in a per-runner `known_hosts.json` (0600): the
//! first connect stores the server key, every later connect REQUIRES it to
//! match — a changed key is a MITM alarm and rejects the connection.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use parking_lot::Mutex;
use russh::client;
use russh::keys::ssh_key::PublicKey;
use russh::keys::{self, PrivateKeyWithHashAlg, decode_secret_key};
use thiserror::Error;

use crate::exec::ExecResult;

pub const KNOWN_HOSTS_FILE: &str = "known_hosts.json";

#[derive(Debug, Error)]
pub enum SshError {
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
    #[error("invalid target address {address:?}: expected user@host[:port]")]
    BadAddress { address: String },
    #[error("invalid ssh key: {0}")]
    Key(String),
    #[error("ssh error: {0}")]
    Russh(String),
    #[error("authentication failed (remaining methods: {0})")]
    Auth(String),
    #[error("host key for {0} changed since first connect — possible MITM")]
    HostKeyChanged(String),
    #[error("command timed out after {0}s")]
    TimedOut(u64),
}

impl From<russh::Error> for SshError {
    fn from(e: russh::Error) -> Self {
        SshError::Russh(e.to_string())
    }
}

impl From<russh_sftp::client::error::Error> for SshError {
    fn from(e: russh_sftp::client::error::Error) -> Self {
        SshError::Russh(format!("sftp: {e}"))
    }
}

impl From<keys::Error> for SshError {
    fn from(e: keys::Error) -> Self {
        SshError::Key(e.to_string())
    }
}

/// A parsed SSH endpoint from the CP's address string: `user@host[:port]`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SshTarget {
    pub name: String,
    pub host: String,
    pub port: u16,
    pub user: String,
}

impl SshTarget {
    pub fn parse(name: &str, address: &str) -> Result<Self, SshError> {
        let (user, rest) = address
            .rsplit_once('@')
            .ok_or_else(|| SshError::BadAddress {
                address: address.to_string(),
            })?;
        let (host, port) = match rest.rsplit_once(':') {
            Some((h, p)) => (
                h,
                p.parse::<u16>().map_err(|_| SshError::BadAddress {
                    address: address.to_string(),
                })?,
            ),
            None => (rest, 22),
        };
        if user.is_empty() || host.is_empty() {
            return Err(SshError::BadAddress {
                address: address.to_string(),
            });
        }
        Ok(Self {
            name: name.to_string(),
            host: host.to_string(),
            port,
            user: user.to_string(),
        })
    }

    fn known_hosts_key(&self) -> String {
        format!("[{}]:{}", self.host, self.port)
    }
}

/// TOFU host-key store: `"[host]:port" -> openssh public key`, persisted
/// 0600. First connect adds; later connects must match exactly.
#[derive(Default)]
struct HostKeyStore {
    path: PathBuf,
    inner: Mutex<HashMap<String, String>>,
    /// The store EXISTS but is unreadable/corrupt — fail closed: every
    /// verification rejects until it is repaired, rather than silently
    /// re-pinning whatever key shows up next (the loss that matters).
    corrupt: bool,
}

impl HostKeyStore {
    fn load(dir: &Path) -> Self {
        let path = dir.join(KNOWN_HOSTS_FILE);
        let (inner, corrupt) = match std::fs::read_to_string(&path) {
            Ok(raw) => match serde_json::from_str::<HashMap<String, String>>(&raw) {
                Ok(map) => (map, false),
                Err(e) => {
                    tracing::warn!(
                        path = %path.display(),
                        error = %e,
                        "known_hosts.json corrupt — failing closed on host-key checks"
                    );
                    (HashMap::new(), true)
                }
            },
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => (HashMap::new(), false),
            Err(e) => {
                tracing::warn!(
                    path = %path.display(),
                    error = %e,
                    "known_hosts.json unreadable — failing closed on host-key checks"
                );
                (HashMap::new(), true)
            }
        };
        Self {
            path,
            inner: Mutex::new(inner),
            corrupt,
        }
    }

    /// Returns Ok(true) if the key is known and matches; Ok(true) and stores
    /// it on first contact; Err(HostKeyChanged) if a DIFFERENT key shows up
    /// or the store is corrupt.
    fn verify_or_add(&self, key: &str, pubkey: &PublicKey) -> Result<bool, SshError> {
        if self.corrupt {
            return Err(SshError::HostKeyChanged(key.to_string()));
        }
        let stored = pubkey
            .to_openssh()
            .map_err(|e| SshError::Key(e.to_string()))?
            .to_string();
        let mut map = self.inner.lock();
        match map.get(key) {
            Some(existing) if *existing == stored => Ok(true),
            Some(_) => Err(SshError::HostKeyChanged(key.to_string())),
            None => {
                map.insert(key.to_string(), stored);
                let json = serde_json::to_vec(&*map)
                    .map_err(|e| SshError::Io(std::io::Error::other(e.to_string())))?;
                freehold_core::futil::write_0600_atomic(&self.path, &json)?;
                Ok(true)
            }
        }
    }
}

struct HostKeyHandler {
    store: Arc<HostKeyStore>,
    key: String,
}

impl client::Handler for HostKeyHandler {
    type Error = SshError;

    async fn check_server_key(&mut self, public: &PublicKey) -> Result<bool, Self::Error> {
        self.store.verify_or_add(&self.key, public)
    }
}

/// Per-target persistent SSH connections. The runner owns connection
/// lifetime; a second exec on the same target reuses the open session.
type Conn = Arc<tokio::sync::Mutex<client::Handle<HostKeyHandler>>>;

/// Connection-lane health — surfaced by the runner's `status` tool so the
/// CP/TUI can ALERT when the lane keeps dropping or can't reconnect.
#[derive(Debug, Clone, Default)]
pub struct PoolHealth {
    pub drops: u64,
    pub reconnects: u64,
    pub last_drop: Option<String>,
    pub last_error: Option<String>,
}

pub struct SshPool {
    known_hosts: Arc<HostKeyStore>,
    /// per target: a small conn VEC — concurrent execs each get their OWN
    /// connection (the parallel LXC boots really parallelize instead of
    /// queueing on one channel); beyond the cap they share.
    conns: Mutex<HashMap<String, Vec<Conn>>>,
    health: Mutex<PoolHealth>,
}

/// How many concurrent connections we'll hold per target. Two covers the
/// parallel LXC boots; everything else runs one exec at a time.
const MAX_CONNS_PER_TARGET: usize = 2;

impl SshPool {
    pub fn new(state_dir: &Path) -> Self {
        Self {
            known_hosts: Arc::new(HostKeyStore::load(state_dir)),
            conns: Mutex::new(HashMap::new()),
            health: Mutex::new(PoolHealth::default()),
        }
    }

    /// The lane's health snapshot (for the `status` tool / alerting).
    pub fn health(&self) -> PoolHealth {
        self.health.lock().clone()
    }

    async fn connect(
        &self,
        target: &SshTarget,
        key: &keys::PrivateKey,
    ) -> Result<client::Handle<HostKeyHandler>, SshError> {
        // Liveness at the transport level: ping the server when idle and
        // close on unanswered keepalives — a silently-dead link is detected
        // in ~30s instead of hanging the lane forever.
        let config = Arc::new(client::Config {
            keepalive_interval: Some(Duration::from_secs(10)),
            keepalive_max: 3,
            ..Default::default()
        });
        let handler = HostKeyHandler {
            store: self.known_hosts.clone(),
            key: target.known_hosts_key(),
        };
        let mut handle =
            client::connect(config, (target.host.as_str(), target.port), handler).await?;
        let auth = handle
            .authenticate_publickey(
                &target.user,
                PrivateKeyWithHashAlg::new(Arc::new(key.clone()), None),
            )
            .await?;
        if !auth.success() {
            let methods = match &auth {
                russh::client::AuthResult::Failure {
                    remaining_methods, ..
                } => {
                    format!("{remaining_methods:?}")
                }
                _ => "unknown".to_string(),
            };
            return Err(SshError::Auth(methods));
        }
        Ok(handle)
    }

    /// Borrow-or-connect the pooled connection for `target`. Connect
    /// failures RETRY with backoff (1s/2s/4s) — a transient drop must
    /// self-heal, not surface as an immediate exec failure.
    async fn handle(&self, target: &SshTarget, key: &keys::PrivateKey) -> Result<Conn, SshError> {
        // Parallel-safe pool: reuse an IDLE connection (serial execs stay
        // pooled); open a fresh one only when every existing conn is busy
        // (the two LXC boots get their own lane). Each helper locks in a
        // single sync statement — no lock is ever held across an await.
        // A connect can hang on a blackholed host (SYN dropped) — bound it
        // so a dead target costs ~15s, not the kernel's ~127s, and counts
        // as a transient (retried) rather than stalling forever.
        let connect_timeout = || {
            let t = target.clone();
            let k = key.clone();
            async move { tokio::time::timeout(Duration::from_secs(15), self.connect(&t, &k)).await }
        };
        let mut at_cap = false;
        for attempt in 0..3usize {
            if let Some(c) = self.try_reuse(&target.name) {
                return Ok(c);
            }
            if self.can_open(&target.name) {
                match connect_timeout().await {
                    Ok(Ok(h)) => {
                        let c: Conn = Arc::new(tokio::sync::Mutex::new(h));
                        self.push_conn(&target.name, &c);
                        self.health.lock().reconnects += 1;
                        return Ok(c);
                    }
                    Ok(Err(e)) => {
                        self.health.lock().last_error = Some(e.to_string());
                        // auth / changed-host-key failures are PERMANENT — no
                        // point retrying (wrong key, MITM); only transient
                        // connection errors get backoff.
                        if matches!(e, SshError::Auth(_) | SshError::HostKeyChanged(_)) {
                            return Err(e);
                        }
                        // sleep BETWEEN attempts only — never after the final
                        // one (a fast-refusing host must cost ~3s, not 7s,
                        // or the CP's 4s readiness probe reads the whole
                        // runner as unreachable).
                        if attempt < 2 {
                            tokio::time::sleep(Duration::from_secs(1 << attempt)).await;
                        }
                    }
                    Err(_) => {
                        self.health.lock().last_error = Some("connect timeout (15s)".into());
                        if attempt < 2 {
                            tokio::time::sleep(Duration::from_secs(1 << attempt)).await;
                        }
                    }
                }
            } else {
                at_cap = true;
                // at the cap and all busy: wait briefly, re-check for a free
                // one.
                tokio::time::sleep(Duration::from_millis(150)).await;
            }
        }
        // ONLY when the lane was BUSY (never after pure connect failures —
        // a fast-refusing host must still error in ~3s, not +15s) SHARE:
        // hold for a free lane (bounded — the caller's exec timeout is the
        // outer bound; a busy lane frees in a moment).
        if at_cap {
            for _ in 0..15usize {
                if let Some(c) = self.try_reuse(&target.name) {
                    return Ok(c);
                }
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
            Err(SshError::Russh(
                "all connections to this target are busy".into(),
            ))
        } else {
            Err(SshError::Russh("connect failed after 3 attempts".into()))
        }
    }

    /// Reuse an idle pooled connection if one exists (sync only).
    fn try_reuse(&self, target: &str) -> Option<Conn> {
        let conns = self.conns.lock();
        let vec = conns.get(target)?;
        vec.iter().find(|c| c.try_lock().is_ok()).cloned()
    }

    fn can_open(&self, target: &str) -> bool {
        let mut conns = self.conns.lock();
        conns.entry(target.to_string()).or_default().len() < MAX_CONNS_PER_TARGET
    }

    fn push_conn(&self, target: &str, c: &Conn) {
        self.conns
            .lock()
            .entry(target.to_string())
            .or_default()
            .push(c.clone());
    }

    /// Execute `cmd` verbatim over the target's connection. Parses key PEM
    /// from the decrypted secret each call (cheap; keeps zero copies around).
    pub async fn exec(
        &self,
        target: &SshTarget,
        key_pem: &str,
        cmd: &str,
        timeout_s: Option<u64>,
    ) -> Result<ExecResult, SshError> {
        let key = decode_secret_key(key_pem, None)?;
        let conn = self.handle(target, &key).await?;

        // Serialize per target: only one command at a time on the pooled
        // connection. Lock acquisition is OUTSIDE the command timeout — a
        // busy target must not surface as a spurious timeout for a healthy
        // host (and must not evict the in-use connection).
        let handle = conn.lock().await;

        let run = async move {
            let mut channel = handle.channel_open_session().await?;
            channel.exec(false, cmd).await?;
            let mut stdout = Vec::new();
            let mut stderr = Vec::new();
            let mut exit_code = None;
            loop {
                match channel.wait().await {
                    Some(russh::ChannelMsg::Data { data }) => stdout.extend_from_slice(&data),
                    Some(russh::ChannelMsg::ExtendedData { data, ext: 1 }) => {
                        stderr.extend_from_slice(&data)
                    }
                    Some(russh::ChannelMsg::ExtendedData { data, .. }) => {
                        stdout.extend_from_slice(&data)
                    }
                    Some(russh::ChannelMsg::ExitStatus { exit_status }) => {
                        exit_code = Some(exit_status as i32)
                    }
                    Some(russh::ChannelMsg::Close) | None => break,
                    _ => {}
                }
            }
            Ok::<ExecResult, SshError>(ExecResult {
                stdout: String::from_utf8_lossy(&stdout).into_owned(),
                stderr: String::from_utf8_lossy(&stderr).into_owned(),
                exit_code,
                timed_out: false,
            })
        };

        let result = match timeout_s {
            Some(t) => match tokio::time::timeout(Duration::from_secs(t), run).await {
                Ok(r) => r,
                Err(_) => Ok(ExecResult {
                    stdout: String::new(),
                    stderr: String::new(),
                    exit_code: None,
                    timed_out: true,
                }),
            },
            None => run.await,
        };
        // On ANY suspect outcome the connection is dropped (reconnect next
        // time): an ERROR *and* a TIMEOUT. The timeout case is the wedge —
        // a hung lane that returned Ok(timed_out) used to STAY pooled and
        // stall every later exec on the target.
        match &result {
            Err(e) => {
                self.conns.lock().remove(&target.name);
                let mut h = self.health.lock();
                h.drops += 1;
                h.last_drop = Some(format!("exec error: {e}"));
            }
            Ok(r) if r.timed_out => {
                self.conns.lock().remove(&target.name);
                let mut h = self.health.lock();
                h.drops += 1;
                h.last_drop = Some("exec timed out — lane dropped".into());
            }
            _ => {}
        }
        result
    }

    /// Upload a LOCAL file to `remote_path` over the SAME pooled connection
    /// via the sftp subsystem — raw binary streaming (no base64, no command
    /// size limits, no per-chunk round trips; the SSH transport compresses).
    /// Returns the uploaded byte count for the caller's size verify.
    pub async fn upload(
        &self,
        target: &SshTarget,
        key_pem: &str,
        local_path: &Path,
        remote_path: &str,
    ) -> Result<u64, SshError> {
        use tokio::io::AsyncWriteExt;
        let data = std::fs::read(local_path)?;
        let key = decode_secret_key(key_pem, None)?;
        let conn = self.handle(target, &key).await?;
        let handle = conn.lock().await;
        let channel = handle.channel_open_session().await?;
        channel.request_subsystem(true, "sftp").await?;
        let sftp = russh_sftp::client::SftpSession::new(channel.into_stream()).await?;
        let mut file = sftp.create(remote_path).await?;
        // file.set_metadata(Some(russh_sftp::protocol::FileAttributes { permissions: ... }))
        for chunk in data.chunks(256 * 1024) {
            file.write_all(chunk).await?;
        }
        file.flush().await?;
        file.close().await?;
        Ok(data.len() as u64)
    }

    /// Runner's OWN self-check against the target: run a trivial command.
    pub async fn self_check(&self, target: &SshTarget, key_pem: &str) -> Result<bool, SshError> {
        let r = self.exec(target, key_pem, "uname -s", Some(10)).await?;
        Ok(r.exit_code == Some(0))
    }
}
