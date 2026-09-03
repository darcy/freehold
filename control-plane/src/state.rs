//! Control-plane state: runners + secrets.
//!
//! The CP is a SECRET PROVISIONER, not a vault — records hold public keys and
//! ciphertext only. No private keys, no plaintext, no master key. The runner
//! holds its own injected private key + ciphertext and decrypts locally.
//!
//! Chunk 1 invariant: ONE secret per runner (dedicated runner per service =
//! default), so a runner's package is rebuilt from its single secret on rotate.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use parking_lot::RwLock;
use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const STATE_FILE: &str = "state.json";
pub const STATE_DIR_ENV: &str = "FREEHOLD_CP_STATE_DIR";

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum RunnerStatus {
    #[default]
    Active,
    Revoked,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RunnerRecord {
    /// Nostr x-only pubkey — the grant/membership identity.
    pub nostr_pubkey: String,
    /// X25519 pubkey — the CP seals secrets TO this key. The private half is
    /// never stored here; holding it would be a master key.
    pub enc_pubkey: String,
    pub status: RunnerStatus,
    /// Where the runner package (identity.json + secrets.json) was shipped.
    pub package_dir: PathBuf,
    pub created_at: u64,
    /// The runner's MCP listen address (Phase F, console polling). The CP
    /// does not discover this — the operator sets it once in the UI and the
    /// console signs live readiness probes against it. Unset = no probe.
    #[serde(default)]
    pub mcp_addr: Option<String>,
    /// safe | risky-install | risky-host — visible at grant time in the
    /// console (POC_CHUNK3 §0.02). Kind-default at provision, overridable,
    /// preserved through rebuild (rides the fh-profile `risk` field).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub risk_level: Option<String>,
}

/// A named AI agent: the relay-addressable pubkey + when it was stood up.
/// The registry is observability — the agent's availability comes from its
/// recent presence on the relay, never from this row.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct AgentRecord {
    pub pubkey: String,
    pub created_at: u64,
    /// The agent's kind-9 presence CHANNEL (the relay scopes kind-9 reads
    /// by #h) — required for an availability probe.
    #[serde(default)]
    pub channel: Option<String>,
}

/// One explicit DNS record the CP resolver serves (C0: the core-service
/// resolver). `name` is a bare hostname WITHOUT the domain suffix — the
/// resolver config joins the world domain (`litellm.freehold.internal`).
/// Explicit records only; absent = the resolver forwards upstream (never a
/// stale/typo name silently resolving).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DnsRecord {
    /// Target IP (A record). v4 for now; the record shape leaves room for
    /// AAAA if the substrate ever needs it.
    pub ip: String,
    /// Which registration wrote this (e.g. "record_lxc relay", "litellm
    /// apply") — for the read-only UI and teardown bookkeeping.
    pub source: String,
    pub created_at: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DnsWildcard {
    /// Apex domain ALL of whose subdomains resolve to `ip` (e.g.
    /// `freehold-test.darcydev.net`).
    pub apex: String,
    /// The IP every `<sub>.<apex>` resolves to (the Caddy node). v4 for now.
    pub ip: String,
    /// Which registration wrote this (e.g. "record_caddy").
    pub source: String,
    pub created_at: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SecretRecord {
    /// Runner (service) this secret belongs to — one per runner in Chunk 1.
    pub runner: String,
    pub kind: String,
    pub address: String,
    /// Sealed-box ciphertext (hex), encrypted TO the runner's enc_pubkey.
    pub ciphertext_hex: String,
    pub created_at: u64,
    pub rotated_at: Option<u64>,
}

#[derive(Debug, Default, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ControlPlaneState {
    pub runners: BTreeMap<String, RunnerRecord>,
    pub secrets: BTreeMap<String, SecretRecord>,
    /// Explicit DNS records the resolver serves (name -> record). Empty =
    /// the CP resolver still runs, but only forwards upstream.
    #[serde(default)]
    pub dns: BTreeMap<String, DnsRecord>,
    /// The world domain suffix the resolver joins to bare records when set
    /// (e.g. "darcydev.net") — addn-hosts then renders BOTH `<name>` and
    /// `<name>.<domain>`, so guests' search-first (ndots=1) lookups hit the
    /// split-horizon answer instead of leaking upstream. Absent = bare only.
    #[serde(default)]
    pub resolver_domain: Option<String>,
    /// A dnsmasq `address=/.<apex>/<ip>` wildcard apex (e.g. apex
    /// `freehold-test.darcydev.net` -> the Caddy node). Rendered as a
    /// leading-dot `address=` so ALL subdomains of the apex (`relay.`, `cp.`,
    /// `*.`) resolve to the given IP without touching the apex itself. Set =
    /// served; absent = only explicit records + the resolver domain are served.
    #[serde(default)]
    pub resolver_wildcard: Option<DnsWildcard>,
    /// Named AI agents stood up (the CPA records them when it creates one —
    /// e.g. the delegate-peer registers at start). Availability is probed
    /// LIVE against the relay; this table is the registry, not the status.
    #[serde(default)]
    pub agents: BTreeMap<String, AgentRecord>,
    /// The COMMUNITY host the relay serves kind-9/#h under — the relay
    /// serves per-community by `Host`, so the co-located console (LAN URL)
    /// must send it explicitly.
    #[serde(default)]
    pub relay_host: Option<String>,
    /// Console operator/admin whitelist (64-hex Nostr pubkeys). Non-empty
    /// => NIP-98 console auth is ON and the bind guard relaxes (C3.5).
    #[serde(default)]
    pub admins: Vec<String>,
    /// The relay this console operates as (Chunk 2.6.1): set by `serve
    /// --relay-url` (and by provision/rotate/revoke publishing in the CLI);
    /// the web UI drives channel/membership sync against it. The runner
    /// lifecycle + grants live on the relay as NIP-29 channels (see
    /// core::relay_http); state.json is the local mirror, not the source.
    #[serde(default)]
    pub relay_url: Option<String>,
    /// The RELAY's nostr pubkey (Chunk 2.6.1): the trust anchor that signs
    /// membership rosters. Required alongside `relay_url` for the web UI's
    /// verified roster view; the runner enforces with its own copy.
    #[serde(default)]
    pub relay_pubkey: Option<String>,
}
#[derive(Debug, Error)]
pub enum StateError {
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
    #[error("malformed state json: {0}")]
    Serde(#[from] serde_json::Error),
    #[error("runner {0} not found")]
    RunnerNotFound(String),
    #[error("secret {0} not found")]
    SecretNotFound(String),
}

pub struct StateStore {
    dir: PathBuf,
    inner: RwLock<ControlPlaneState>,
}

impl StateStore {
    /// Open (creating if needed) the CP state under `dir` (0700).
    pub fn open(dir: &Path) -> Result<Self, StateError> {
        freehold_core::futil::ensure_private_dir(dir)?;
        let path = dir.join(STATE_FILE);
        let state = if path.exists() {
            let raw = std::fs::read_to_string(&path)?;
            serde_json::from_str(&raw)?
        } else {
            ControlPlaneState::default()
        };
        Ok(Self {
            dir: dir.to_path_buf(),
            inner: RwLock::new(state),
        })
    }

    /// Persist atomically (0600). Call after any mutation.
    ///
    /// TODO(Postgres swap at MVP): atomic per-write is NOT atomic across
    /// open→mutate→save — two concurrent CP invocations on one state dir are
    /// last-writer-wins on the whole file and can drop a record whose keys
    /// were already shipped. Package writes also precede state persistence in
    /// `provision` (in-process rollback on save failure; cross-process
    /// atomicity still wants an O_EXCL lockfile held across the
    /// read-modify-write, or a real store).
    pub fn save(&self) -> Result<(), StateError> {
        let json = serde_json::to_vec(&*self.inner.read())?;
        freehold_core::futil::write_0600_atomic(&self.dir.join(STATE_FILE), &json)?;
        Ok(())
    }

    pub fn snapshot(&self) -> ControlPlaneState {
        self.inner.read().clone()
    }

    pub fn get_runner(&self, name: &str) -> Option<RunnerRecord> {
        self.inner.read().runners.get(name).cloned()
    }

    pub fn get_secret(&self, name: &str) -> Option<SecretRecord> {
        self.inner.read().secrets.get(name).cloned()
    }

    pub fn insert_runner(&self, name: &str, rec: RunnerRecord) {
        self.inner.write().runners.insert(name.to_string(), rec);
    }

    pub fn insert_secret(&self, name: &str, rec: SecretRecord) {
        self.inner.write().secrets.insert(name.to_string(), rec);
    }

    pub fn admins(&self) -> Vec<String> {
        self.inner.read().admins.clone()
    }

    pub fn set_admins(&self, admins: Vec<String>) {
        self.inner.write().admins = admins;
    }

    pub fn set_runner_mcp_addr(&self, name: &str, addr: Option<String>) -> Result<(), StateError> {
        let mut inner = self.inner.write();
        let rec = inner
            .runners
            .get_mut(name)
            .ok_or_else(|| StateError::RunnerNotFound(name.to_string()))?;
        rec.mcp_addr = addr;
        Ok(())
    }

    /// Persist the relay scope for this console (Chunk 2.6.1). The web UI
    /// syncs runner channels against it; a restart keeps it.
    pub fn remove_agent(&self, name: &str) {
        self.inner.write().agents.remove(name);
    }

    pub fn insert_agent(&self, name: &str, rec: AgentRecord) {
        self.inner.write().agents.insert(name.to_string(), rec);
    }

    pub fn relay_host(&self) -> Option<String> {
        self.inner.read().relay_host.clone()
    }

    pub fn set_relay_host(&self, relay_host: Option<String>) -> Result<(), StateError> {
        self.inner.write().relay_host = relay_host;
        self.save()
    }

    pub fn set_resolver_domain(&self, resolver_domain: Option<String>) -> Result<(), StateError> {
        self.inner.write().resolver_domain = resolver_domain;
        self.save()
    }

    pub fn resolver_wildcard(&self) -> Option<DnsWildcard> {
        self.inner.read().resolver_wildcard.clone()
    }

    pub fn set_resolver_wildcard(
        &self,
        wildcard: Option<DnsWildcard>,
    ) -> Result<(), StateError> {
        self.inner.write().resolver_wildcard = wildcard;
        self.save()
    }

    pub fn set_relay_url(&self, relay_url: Option<String>) -> Result<(), StateError> {
        self.inner.write().relay_url = relay_url;
        self.save()
    }

    pub fn set_relay_pubkey(&self, relay_pubkey: Option<String>) -> Result<(), StateError> {
        self.inner.write().relay_pubkey = relay_pubkey;
        self.save()
    }

    pub fn get_dns(&self, name: &str) -> Option<DnsRecord> {
        self.inner.read().dns.get(name).cloned()
    }

    pub fn insert_dns(&self, name: &str, rec: DnsRecord) {
        self.inner.write().dns.insert(name.to_string(), rec);
    }

    pub fn remove_dns(&self, name: &str) {
        self.inner.write().dns.remove(name);
    }

    /// The state dir itself (the console identity + packages live beside it).
    pub fn dir(&self) -> &Path {
        &self.dir
    }

    pub fn set_runner_status(&self, name: &str, status: RunnerStatus) -> Result<(), StateError> {
        let mut inner = self.inner.write();
        let rec = inner
            .runners
            .get_mut(name)
            .ok_or_else(|| StateError::RunnerNotFound(name.to_string()))?;
        rec.status = status;
        Ok(())
    }

    /// Revert helpers for failed `save()` rollbacks: a long-lived `serve`
    /// must not persist a phantom mutation (record that never saved, status
    /// flip that never hit disk) on its NEXT successful write.
    pub fn remove_runner(&self, name: &str) {
        self.inner.write().runners.remove(name);
    }

    pub fn remove_secret(&self, name: &str) {
        self.inner.write().secrets.remove(name);
    }

    /// Restore a secret record's ciphertext + rotation stamp (rollback of
    /// `update_secret_ciphertext`).
    pub fn set_secret_ciphertext(
        &self,
        name: &str,
        ciphertext_hex: &str,
        rotated_at: Option<u64>,
    ) -> Result<(), StateError> {
        let mut inner = self.inner.write();
        let rec = inner
            .secrets
            .get_mut(name)
            .ok_or_else(|| StateError::SecretNotFound(name.to_string()))?;
        rec.ciphertext_hex = ciphertext_hex.to_string();
        rec.rotated_at = rotated_at;
        Ok(())
    }

    /// Chunk 2.6: replace the runner/secret views wholesale — `rebuild`
    /// folds the relay's addressable snapshots into a fresh projection.
    /// Deterministic + idempotent: same input → same state; a re-run
    /// converges (no partial/stale records survive). Restored records
    /// carry NO ciphertext or package path (the relay never holds secret
    /// material) — `adopt` per runner re-arms the package.
    pub fn rebuild_from(
        &self,
        runners: BTreeMap<String, RunnerRecord>,
        secrets: BTreeMap<String, SecretRecord>,
    ) -> Result<(), StateError> {
        {
            let mut inner = self.inner.write();
            inner.runners = runners;
            inner.secrets = secrets;
        }
        self.save()
    }

    pub fn update_secret_ciphertext(
        &self,
        name: &str,
        ciphertext_hex: &str,
        rotated_at: u64,
    ) -> Result<(), StateError> {
        let mut inner = self.inner.write();
        let rec = inner
            .secrets
            .get_mut(name)
            .ok_or_else(|| StateError::SecretNotFound(name.to_string()))?;
        rec.ciphertext_hex = ciphertext_hex.to_string();
        rec.rotated_at = Some(rotated_at);
        Ok(())
    }
}

pub fn now_secs() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}
