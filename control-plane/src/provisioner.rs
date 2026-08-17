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

use freehold_core::{
    crypto, identity,
    secrets::{SecretPackage, TargetMeta},
};
use thiserror::Error;

use crate::state::{RunnerRecord, RunnerStatus, SecretRecord, StateError, StateStore, now_secs};

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
    #[error("invalid agent pubkey {0:?}: must be 64 hex chars")]
    InvalidGrant(String),
    #[error("package dir {0} already holds a runner — refusing to clobber")]
    PackageDirInUse(PathBuf),
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
    /// AGENT pubkeys granted to call this runner (Phase D). Empty ships a
    /// fail-closed package: nobody may call until a `grant` lands.
    pub grants: &'a [String],
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
    // `local` is the runner's own target; a shipped target must not shadow it.
    if req.name == "local" {
        return Err(ProvisionError::InvalidName(req.name.to_string()));
    }
    // Grants must be real pubkeys or the runner silently denies forever.
    for g in req.grants {
        if !is_pubkey(g) {
            return Err(ProvisionError::InvalidGrant(g.clone()));
        }
    }
    // Same-name cases first — clearer diagnostics than the dir guard below.
    match store.get_runner(req.name) {
        Some(r) if r.status == RunnerStatus::Revoked => {
            return Err(ProvisionError::RunnerRevoked(req.name.to_string()));
        }
        Some(_) => return Err(ProvisionError::RunnerExists(req.name.to_string())),
        None => {}
    }
    // `runner_dir` is caller-supplied (and FREEHOLD_RUNNER_STATE_DIR applies
    // to EVERY provision) — shipping a second runner into a dir that already
    // holds a package silently destroys the first runner's private key and
    // ciphertext while CP state still lists it active with undecryptable
    // ciphertext. Refuse instead.
    if req.runner_dir.join(identity::IDENTITY_FILE).exists() {
        return Err(ProvisionError::PackageDirInUse(
            req.runner_dir.to_path_buf(),
        ));
    }
    for rec in store.snapshot().runners.values() {
        if rec.package_dir == req.runner_dir {
            return Err(ProvisionError::PackageDirInUse(
                req.runner_dir.to_path_buf(),
            ));
        }
    }

    let id = identity::Identity::generate();
    let enc_pub = hex_to_arr(&id.enc_pubkey_hex())?;
    // aad = secret NAME: the blob is cryptographically pinned to the entry it
    // will be filed under, so package entries can't be swapped between names.
    let sealed = crypto::seal(&enc_pub, req.name.as_bytes(), req.secret)?;
    let ciphertext_hex = hex::encode(&sealed);
    let nostr_pubkey = id.nostr_pubkey_hex();
    let enc_pubkey = id.enc_pubkey_hex();

    // Ship the runner package: identity.json holds BOTH private keys (the
    // runner's injected material); secrets.json holds ciphertext only.
    id.write_to_dir(req.runner_dir)?;
    let pkg = SecretPackage {
        secrets: BTreeMap::from([(req.name.to_string(), ciphertext_hex.clone())]),
        targets: BTreeMap::from([(
            req.name.to_string(),
            TargetMeta {
                kind: req.kind.to_string(),
                address: req.address.to_string(),
                secret: req.name.to_string(),
            },
        )]),
        grants: req.grants.to_vec(),
    };
    if let Err(e) = pkg.write_to_dir(req.runner_dir) {
        // identity.json already landed — remove it so the PackageDirInUse
        // guard doesn't lock the dir against the CP's own leftovers on retry.
        let _ = std::fs::remove_file(req.runner_dir.join(identity::IDENTITY_FILE));
        return Err(e.into());
    }

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
            mcp_addr: None,
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
    if let Err(e) = store.save() {
        // Roll back disk AND memory: the dir was EMPTY before (the
        // PackageDirInUse guard), so removing what we wrote is safe and leaves
        // no orphan runner with a credential the CP has no record of. The
        // in-memory revert keeps a long-lived `serve` from persisting a
        // phantom record on its next successful save.
        let _ = std::fs::remove_file(req.runner_dir.join(identity::IDENTITY_FILE));
        let _ = std::fs::remove_file(req.runner_dir.join(freehold_core::secrets::SECRETS_FILE));
        store.remove_runner(req.name);
        store.remove_secret(req.name);
        return Err(e.into());
    }

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
    let before = store
        .get_secret(name)
        .ok_or_else(|| ProvisionError::SecretNotFound(name.to_string()))?;
    let runner = before.runner.clone();
    let runner_rec = store
        .get_runner(&runner)
        .ok_or_else(|| StateError::RunnerNotFound(runner.clone()))?;
    if runner_rec.status == RunnerStatus::Revoked {
        return Err(ProvisionError::RunnerRevoked(runner));
    }

    let enc_pub = hex_to_arr(&runner_rec.enc_pubkey)?;
    let sealed = crypto::seal(&enc_pub, name.as_bytes(), new_secret)?;
    let ciphertext_hex = hex::encode(&sealed);

    // Re-ship the package (Chunk 1 invariant: one secret per runner).
    // Re-ship the package with its target metadata + grants preserved
    // (rotate must not drop how the runner reaches this service, nor who may
    // call it).
    let pkg = SecretPackage {
        secrets: BTreeMap::from([(name.to_string(), ciphertext_hex.clone())]),
        targets: BTreeMap::from([(
            name.to_string(),
            TargetMeta {
                kind: before.kind.clone(),
                address: before.address.clone(),
                secret: name.to_string(),
            },
        )]),
        grants: current_grants(&runner_rec.package_dir)?,
    };
    pkg.write_to_dir(&runner_rec.package_dir)?;

    store.update_secret_ciphertext(name, &ciphertext_hex, now_secs())?;
    if let Err(e) = store.save() {
        // Roll back memory (serve must not persist the phantom ciphertext
        // later) and re-ship the OLD package, so the runner doesn't end up
        // with a credential the CP never recorded. Each branch says exactly
        // what happened — never claim a rollback that didn't land.
        let _ = store.set_secret_ciphertext(name, &before.ciphertext_hex, before.rotated_at);
        let old_pkg = SecretPackage {
            secrets: BTreeMap::from([(name.to_string(), before.ciphertext_hex.clone())]),
            targets: BTreeMap::from([(
                name.to_string(),
                TargetMeta {
                    kind: before.kind.clone(),
                    address: before.address.clone(),
                    secret: name.to_string(),
                },
            )]),
            grants: current_grants(&runner_rec.package_dir).unwrap_or_default(),
        };
        match old_pkg.write_to_dir(&runner_rec.package_dir) {
            Ok(()) => tracing::warn!(
                name,
                error = %e,
                "rotate: save failed — rolled the package back to the previous \
                 credential, which may already be invalid upstream"
            ),
            Err(restore_err) => tracing::warn!(
                name,
                error = %e,
                restore_error = %restore_err,
                "rotate: save failed AND the previous package could not be restored — \
                 the runner keeps the NEW credential that the CP never recorded"
            ),
        }
        return Err(e.into());
    }

    store
        .get_secret(name)
        .ok_or_else(|| ProvisionError::SecretNotFound(name.to_string()))
}

/// B3 — the cut-off lever. Revoked runners refuse re-provision/rotate and read
/// as revoked in `list`. The shipped `secrets.json` is removed so the stored
/// credential capability dies with membership; `identity.json` stays (the
/// runner's own identity — with the relay, membership revocation is the real
/// cut and lands in Chunk 2). If the credential was exposed, rotate it at the
/// service provider: rotation inside the CP is blocked for revoked runners.
pub fn revoke_runner(store: &StateStore, name: &str) -> Result<RunnerRecord, ProvisionError> {
    // State flip FIRST: blocking provision/rotate is the guarantee revoke can
    // actually make. The unlink below is best-effort — the CP may not own the
    // package dir (read-only mount, runner's uid), and a cleanup failure must
    // not keep the runner active.
    let prior = store
        .get_runner(name)
        .ok_or_else(|| StateError::RunnerNotFound(name.to_string()))?;
    if prior.status == RunnerStatus::Revoked {
        // Idempotent on the STATE: skip the flip + save (a failed save can
        // never undo a Revoked record), but still re-attempt the best-effort
        // cleanup — re-running revoke is the natural retry after a transient
        // unlink failure, and secrets.json may have reappeared via config
        // management.
        remove_shipped_secrets(&prior);
        return Ok(prior);
    }
    store.set_runner_status(name, RunnerStatus::Revoked)?;
    if let Err(e) = store.save() {
        // Restore the PRIOR status (not a hardcoded Active): a failed save
        // must never GRANT capability to a runner that was already revoked.
        let _ = store.set_runner_status(name, prior.status);
        return Err(e.into());
    }

    remove_shipped_secrets(&prior);
    store
        .get_runner(name)
        .ok_or_else(|| StateError::RunnerNotFound(name.to_string()).into())
}

/// Best-effort removal of the shipped credential capability. Failure is
/// warned, never fatal — the CP may not own the package dir (read-only
/// mount, runner's uid), and the status flip is the guarantee that matters.
fn remove_shipped_secrets(rec: &RunnerRecord) {
    let secrets_path = rec.package_dir.join(freehold_core::secrets::SECRETS_FILE);
    match std::fs::remove_file(&secrets_path) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => tracing::warn!(
            error = %e,
            path = %secrets_path.display(),
            "revoke: could not remove shipped secrets.json (best-effort)"
        ),
    }
}

/// Load grants strictly: an unreadable package is an ERROR (silently shipping
/// a runner nobody may call hides the reason).
fn current_grants(dir: &std::path::Path) -> Result<Vec<String>, ProvisionError> {
    Ok(SecretPackage::load(dir).map_err(ProvisionError::Io)?.grants)
}

/// D2: grant another agent pubkey to call a runner — re-ship the package so
/// the runner's live grant check picks it up without a restart. Idempotent.
pub fn grant_agent(
    store: &StateStore,
    name: &str,
    agent_pubkey: &str,
) -> Result<Vec<String>, ProvisionError> {
    if !is_pubkey(agent_pubkey) {
        return Err(ProvisionError::InvalidGrant(agent_pubkey.to_string()));
    }
    let rec = store
        .get_runner(name)
        .ok_or_else(|| StateError::RunnerNotFound(name.to_string()))?;
    if rec.status == RunnerStatus::Revoked {
        return Err(ProvisionError::RunnerRevoked(name.to_string()));
    }
    let mut pkg = SecretPackage::load(&rec.package_dir).map_err(ProvisionError::Io)?;
    if !pkg.grants.iter().any(|g| g == agent_pubkey) {
        pkg.grants.push(agent_pubkey.to_string());
        pkg.write_to_dir(&rec.package_dir)?;
    }
    Ok(pkg.grants)
}

/// D2 mirror: revoke an agent pubkey from a runner — re-ship the package
/// minus that grant so the runner's live grant check drops it without a
/// restart. Idempotent (absent grant = no-op). The last grant revoked leaves
/// the package fail-closed: nobody may call until a new grant lands.
pub fn revoke_grant(
    store: &StateStore,
    name: &str,
    agent_pubkey: &str,
) -> Result<Vec<String>, ProvisionError> {
    if !is_pubkey(agent_pubkey) {
        return Err(ProvisionError::InvalidGrant(agent_pubkey.to_string()));
    }
    let rec = store
        .get_runner(name)
        .ok_or_else(|| StateError::RunnerNotFound(name.to_string()))?;
    if rec.status == RunnerStatus::Revoked {
        return Err(ProvisionError::RunnerRevoked(name.to_string()));
    }
    let mut pkg = SecretPackage::load(&rec.package_dir).map_err(ProvisionError::Io)?;
    if pkg.grants.iter().any(|g| g == agent_pubkey) {
        pkg.grants.retain(|g| g != agent_pubkey);
        pkg.write_to_dir(&rec.package_dir)?;
    }
    Ok(pkg.grants)
}

/// Phase D: publish the runner's CURRENT grant list to the relay as a
/// kind-30180 event (addressable, d-tag = runner pubkey — a re-publish
/// REPLACES). Called after grant/revoke so the relay's list and the shipped
/// package agree; the runner reads the relay live. Relay errors surface
/// (never silently swallowed) — the local package is still updated.
pub fn publish_grants(
    store: &StateStore,
    relay_url: &str,
    name: &str,
    grants: &[String],
    state_dir: &std::path::Path,
) -> Result<(), ProvisionError> {
    let rec = store
        .get_runner(name)
        .ok_or_else(|| StateError::RunnerNotFound(name.to_string()))?;
    let console = crate::console::Console::load_or_create(state_dir)
        .map_err(|e| StateError::Io(std::io::Error::other(e.to_string())))?;
    freehold_core::relay_http::publish_grants(
        relay_url,
        &console.identity.secret_seed(),
        &rec.nostr_pubkey,
        grants,
    )
    .map_err(|e| ProvisionError::Io(std::io::Error::other(format!("relay publish: {e}"))))
}

fn is_pubkey(s: &str) -> bool {
    s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit())
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
