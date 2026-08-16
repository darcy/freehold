//! Provisioner (Phase B): the engine room's secret plumbing.
//!
//! **B1** — given an existing service + credential, generate the runner
//! identity + encryption keypair, seal the credential TO the runner's pubkey,
//! ship the package (identity.json + ciphertext-only secrets.json) to the
//! runner's state dir, and record ONLY pubkeys + ciphertext in CP state.
//!
//! **B2** (rotate) — re-seal the credential's NEW value to the same runner
//! key, re-ship the package, update state: the old value is gone everywhere
//! (the "erase" lever).
//!
//! **B3** (revoke) — cut the runner off: revoked runners cannot be
//! re-provisioned or rotated and show as revoked in `list`.
//!
//! NO master key: the CP never retains a private key (the Identity is dropped
//! and zeroized) and never retains plaintext (the secret is sealed and
//! dropped).

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use freehold_core::{crypto, identity, secrets::SecretPackage};
use thiserror::Error;

use crate::state::{now_secs, RunnerRecord, RunnerStatus, SecretRecord, StateError, StateStore};

#[derive(Debug, Error)]
pub enum ProvisionError {
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
    #[error("state error: {0}")]
    State(#[from] StateError),
    #[error("crypto error: {0}")]
    Crypto(#[from] crypto::CryptoError),
    #[error("identity error: {0}")]
    Identity(#[from] identity::IdentityError),
    #[error("bad hex: {0}")]
    Hex(#[from] hex::FromHexError),
    #[error("pubkey must be 32 bytes, got {0}")]
    BadKeyLen(usize),
    #[error("invalid runner name {0:?}: must be a bare name (no '/', no leading '.')")]
    InvalidName(String),
    #[error("runner {0} already exists")]
    RunnerExists(String),
    #[error("runner {0} is revoked — provision a new service or restore it")]
    RunnerRevoked(String),
    #[error("secret {0} not found")]
    SecretNotFound(String),
}

#[derive(Debug, Clone)]
pub struct ProvisionResult {
    pub name: String,
    pub nostr_pubkey: String,
    pub enc_pubkey: String,
    pub package_dir: PathBuf,
}

pub struct ProvisionRequest<'a> {
    pub name: &'a str,
    pub kind: &'a str,
    pub address: &'a str,
    /// Plaintext credential. Sealed immediately, then dropped — never stored,
    /// never logged.
    pub secret: &'a [u8],
    /// Where the runner package (identity.json + secrets.json) is shipped.
    pub runner_dir: &'a Path,
}

/// B1 — the algolia-style happy path: existing service + credential → runner
/// with the secret encrypted to its key, readiness 🟢 next phase.
pub fn provision_runner(
    store: &StateStore,
    req: &ProvisionRequest,
) -> Result<ProvisionResult, ProvisionError> {
    // The name flows into the package dir path — keep it a bare name.
    if req.name.is_empty() || req.name.contains('/') || req.name.starts_with('.') {
        return Err(ProvisionError::InvalidName(req.name.to_string()));
    }
    match store.get_runner(req.name) {
        Some(r) if r.status == RunnerStatus::Revoked => {
            return Err(ProvisionError::RunnerRevoked(req.name.to_string()))
        }
        Some(_) => return Err(ProvisionError::RunnerExists(req.name.to_string())),
        None => {}
    }

    let id = identity::Identity::generate();
    let enc_pub = hex_to_arr(&id.enc_pubkey_hex())?;
    let sealed = crypto::seal(&enc_pub, req.secret)?;
    let ciphertext_hex = hex::encode(&sealed);
    let nostr_pubkey = id.nostr_pubkey_hex();
    let enc_pubkey = id.enc_pubkey_hex();

    // Ship the runner package: identity.json holds BOTH private keys (the
    // runner's injected material); secrets.json holds ciphertext only.
    id.write_to_dir(req.runner_dir)?;
    let pkg = SecretPackage {
        secrets: BTreeMap::from([(req.name.to_string(), ciphertext_hex.clone())]),
    };
    pkg.write_to_dir(req.runner_dir)?;

    // Record pubkeys + ciphertext ONLY. `id` is dropped here and zeroized;
    // the plaintext `req.secret` was sealed and never stored.
    let now = now_secs();
    store.insert_runner(
        req.name,
        RunnerRecord {
            nostr_pubkey: nostr_pubkey.clone(),
            enc_pubkey: enc_pubkey.clone(),
            status: RunnerStatus::Active,
            package_dir: req.runner_dir.to_path_buf(),
            created_at: now,
        },
    );
    store.insert_secret(
        req.name,
        SecretRecord {
            runner: req.name.to_string(),
            kind: req.kind.to_string(),
            address: req.address.to_string(),
            ciphertext_hex,
            created_at: now,
            rotated_at: None,
        },
    );
    store.save()?;

    Ok(ProvisionResult {
        name: req.name.to_string(),
        nostr_pubkey,
        enc_pubkey,
        package_dir: req.runner_dir.to_path_buf(),
    })
}

/// B2 — rotate a secret: re-seal the NEW value to the runner's existing
/// encryption key, re-ship the package, update state. `new_secret` (the fresh
/// credential) replaces the old one everywhere; the old value is gone.
pub fn rotate_secret(
    store: &StateStore,
    name: &str,
    new_secret: &[u8],
) -> Result<SecretRecord, ProvisionError> {
    let runner = store
        .get_secret(name)
        .map(|s| s.runner)
        .ok_or_else(|| ProvisionError::SecretNotFound(name.to_string()))?;
    let runner_rec = store
        .get_runner(&runner)
        .ok_or_else(|| StateError::RunnerNotFound(runner.clone()))?;
    if runner_rec.status == RunnerStatus::Revoked {
        return Err(ProvisionError::RunnerRevoked(runner));
    }

    let enc_pub = hex_to_arr(&runner_rec.enc_pubkey)?;
    let sealed = crypto::seal(&enc_pub, new_secret)?;
    let ciphertext_hex = hex::encode(&sealed);

    // Re-ship the package (Chunk 1 invariant: one secret per runner).
    let pkg = SecretPackage {
        secrets: BTreeMap::from([(name.to_string(), ciphertext_hex.clone())]),
    };
    pkg.write_to_dir(&runner_rec.package_dir)?;

    store.update_secret_ciphertext(name, &ciphertext_hex, now_secs())?;
    store.save()?;

    store
        .get_secret(name)
        .ok_or_else(|| ProvisionError::SecretNotFound(name.to_string()))
}

/// B3 — the cut-off lever. Revoked runners refuse re-provision/rotate and read
/// as revoked in `list`. (With the relay, this becomes Nostr membership
/// revocation; rotation above is the separate "erase" lever.)
pub fn revoke_runner(store: &StateStore, name: &str) -> Result<RunnerRecord, ProvisionError> {
    if store.get_runner(name).is_none() {
        return Err(StateError::RunnerNotFound(name.to_string()).into());
    }
    store.set_runner_status(name, RunnerStatus::Revoked)?;
    store.save()?;
    store
        .get_runner(name)
        .ok_or_else(|| StateError::RunnerNotFound(name.to_string()).into())
}

/// Service-at-a-glance snapshot for the console / CLI.
pub fn snapshot(store: &StateStore) -> crate::state::ControlPlaneState {
    store.snapshot()
}

fn hex_to_arr(s: &str) -> Result<[u8; 32], ProvisionError> {
    let bytes = hex::decode(s)?;
    if bytes.len() != 32 {
        return Err(ProvisionError::BadKeyLen(bytes.len()));
    }
    let mut arr = [0u8; 32];
    arr.copy_from_slice(&bytes);
    Ok(arr)
}
