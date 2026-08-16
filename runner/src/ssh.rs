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
}

impl HostKeyStore {
    fn load(dir: &Path) -> Self {
        let path = dir.join(KNOWN_HOSTS_FILE);
        let inner = std::fs::read_to_string(&path)
            .ok()
            .and_then(|raw| serde_json::from_str::<HashMap<String, String>>(&raw).ok())
            .unwrap_or_default();
        Self {
            path,
            inner: Mutex::new(inner),
        }
    }

    /// Returns Ok(true) if the key is known and matches; Ok(true) and stores
    /// it on first contact; Err(HostKeyChanged) if a DIFFERENT key shows up.
    fn verify_or_add(&self, key: &str, pubkey: &PublicKey) -> Result<bool, SshError> {
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

pub struct SshPool {
    known_hosts: Arc<HostKeyStore>,
    conns: Mutex<HashMap<String, Conn>>,
}

impl SshPool {
    pub fn new(state_dir: &Path) -> Self {
        Self {
            known_hosts: Arc::new(HostKeyStore::load(state_dir)),
            conns: Mutex::new(HashMap::new()),
        }
    }

    async fn connect(
        &self,
        target: &SshTarget,
        key: &keys::PrivateKey,
    ) -> Result<client::Handle<HostKeyHandler>, SshError> {
        let config = Arc::new(client::Config::default());
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

    /// Borrow-or-connect the pooled connection for `target`.
    async fn handle(&self, target: &SshTarget, key: &keys::PrivateKey) -> Result<Conn, SshError> {
        if let Some(c) = self.conns.lock().get(&target.name) {
            return Ok(c.clone());
        }
        let h = self.connect(target, key).await?;
        let c: Conn = Arc::new(tokio::sync::Mutex::new(h));
        self.conns.lock().insert(target.name.clone(), c.clone());
        Ok(c)
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
        let run_conn = conn.clone();

        let run = async move {
            let handle = run_conn.lock().await;
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
                Err(_) => Err(SshError::TimedOut(t)),
            },
            None => run.await,
        };
        // On any failure the connection is suspect — drop it, reconnect next
        // time. On success it stays pooled for the next command.
        if result.is_err() {
            self.conns.lock().remove(&target.name);
        }
        result
    }

    /// Runner's OWN self-check against the target: run a trivial command.
    pub async fn self_check(&self, target: &SshTarget, key_pem: &str) -> Result<bool, SshError> {
        let r = self.exec(target, key_pem, "uname -s", Some(10)).await?;
        Ok(r.exit_code == Some(0))
    }
}
