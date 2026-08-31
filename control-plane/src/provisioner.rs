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
    /// Runner risk class override (safe | risky-install | risky-host); the
    /// kind-based default applies when None (POC_CHUNK3 §0.02).
    pub risk_level: Option<&'a str>,
}

/// Kind-based risk default: API-scoped targets are safe; ssh is the
/// install class by default (the PVE-host runner is marked risky-host by an
/// explicit override at provision); unknown kinds = no label.
fn default_risk(kind: &str) -> Option<&'static str> {
    match kind {
        "vultr" | "b2" | "hetzner" | "github" | "websearch" | "litellm" => Some("safe"),
        "ssh" => Some("risky-install"),
        "local" => Some("safe"),
        _ => None,
    }
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
            risk_level: req
                .risk_level
                .map(String::from)
                .or_else(|| default_risk(req.kind).map(String::from)),
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
/// ADOPT an EXISTING runner into the console registry — no credential is
/// shipped and nothing is re-sealed: the runner's own package directory
/// (identity.json + secrets.json) already holds its keypair and the sealed
/// credential. The record is rebuilt from that package so the console can
/// list the runner, probe its LIVE readiness over MCP, and grant/revoke it
/// (grants still re-ship into the SAME package dir — the runner re-reads
/// them per call).
pub fn adopt_runner(
    store: &StateStore,
    name: &str,
    kind: &str,
    address: &str,
    package_dir: &std::path::Path,
    mcp_addr: Option<String>,
    risk_level: Option<String>,
) -> Result<RunnerRecord, ProvisionError> {
    if name.is_empty() || name.contains('/') || name.starts_with('.') {
        return Err(ProvisionError::InvalidName(name.to_string()));
    }
    if store.get_runner(name).is_some() {
        return Err(ProvisionError::RunnerExists(name.to_string()));
    }
    let id = identity::Identity::load(package_dir)?;
    let pkg = freehold_core::secrets::SecretPackage::load(package_dir)?;
    // The record's ciphertext = the sealed credential the package holds for
    // its (first) target — the same blob rotate would re-seal on top of.
    let ciphertext_hex = pkg
        .targets
        .values()
        .next()
        .and_then(|t| pkg.secrets.get(&t.secret).cloned())
        .unwrap_or_default();
    let created_at = now_secs();
    let runner = RunnerRecord {
        nostr_pubkey: id.nostr_pubkey_hex(),
        enc_pubkey: id.enc_pubkey_hex(),
        status: RunnerStatus::Active,
        package_dir: package_dir.to_path_buf(),
        created_at,
        mcp_addr,
        risk_level,
    };
    // The Chunk-1 model: one SecretRecord per runner, carrying the target
    // kind/address and the sealed credential (rotate re-seals on top of it).
    let secret = SecretRecord {
        runner: name.to_string(),
        kind: kind.to_string(),
        address: address.to_string(),
        ciphertext_hex,
        created_at,
        rotated_at: None,
    };
    store.insert_runner(name, runner.clone());
    store.insert_secret(name, secret);
    store.save()?;
    Ok(runner)
}

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

/// Chunk 2.6.1: sync the runner's NIP-29 channel to the relay — create the
/// private channel (kind 9007, owner = the CP console), add the RUNNER
/// itself as a member (kind 9000; the runner reads its own roster back as
/// its whitelist), and publish the runner's profile as the channel's group
/// METADATA (kind 39000 — the rebuildable projection a respawned CP folds
/// from). Idempotent: a re-run re-asserts the same h channel + membership
/// and REPLACES the meta. Provision/adopt (active) and rotate (rotated_at
/// flip) both travel here; revoke uses `revoke_runner_channel`.
pub fn sync_runner_channel(
    store: &StateStore,
    relay_url: &str,
    name: &str,
    state_dir: &std::path::Path,
) -> Result<(), ProvisionError> {
    // Privileged writes: a FRESH/wrong state dir must not mint a new console
    // key that signs publishes nobody recognizes — load, don't create.
    let console = crate::console::Console::load(state_dir)
        .map_err(|e| StateError::Io(std::io::Error::other(e.to_string())))?;
    let secret = console.identity.secret_seed();
    let profile = runner_meta(store, name)?;
    let rpk = profile.nostr_pubkey.clone();
    freehold_core::relay_http::create_runner_channel(relay_url, &secret, &rpk, name)
        .map_err(|e| relay_err("create channel", e))?;
    freehold_core::relay_http::put_user(relay_url, &secret, &rpk, &rpk)
        .map_err(|e| relay_err("member runner", e))?;
    freehold_core::relay_http::publish_runner_meta(relay_url, &secret, &profile)
        .map_err(|e| relay_err("publish meta", e))
}

/// Chunk 2.6.1: grant — add the agent's pubkey as a member of the runner's
/// channel (kind 9000). The relay re-publishes the roster; the runner's next
/// whitelist read (per call) includes the agent WITHOUT a restart.
pub fn put_user_membership(
    store: &StateStore,
    relay_url: &str,
    name: &str,
    agent_pubkey: &str,
    state_dir: &std::path::Path,
) -> Result<(), ProvisionError> {
    let rec = store
        .get_runner(name)
        .ok_or_else(|| StateError::RunnerNotFound(name.to_string()))?;
    let console = crate::console::Console::load(state_dir)
        .map_err(|e| StateError::Io(std::io::Error::other(e.to_string())))?;
    freehold_core::relay_http::put_user(
        relay_url,
        &console.identity.secret_seed(),
        &rec.nostr_pubkey,
        agent_pubkey,
    )
    .map_err(|e| relay_err("put-user", e))
}

/// Chunk 2.6.1: revoke-grant — remove the agent's pubkey from the runner's
/// channel (kind 9001).
pub fn remove_user_membership(
    store: &StateStore,
    relay_url: &str,
    name: &str,
    agent_pubkey: &str,
    state_dir: &std::path::Path,
) -> Result<(), ProvisionError> {
    let rec = store
        .get_runner(name)
        .ok_or_else(|| StateError::RunnerNotFound(name.to_string()))?;
    let console = crate::console::Console::load(state_dir)
        .map_err(|e| StateError::Io(std::io::Error::other(e.to_string())))?;
    freehold_core::relay_http::remove_user(
        relay_url,
        &console.identity.secret_seed(),
        &rec.nostr_pubkey,
        agent_pubkey,
    )
    .map_err(|e| relay_err("remove-user", e))
}

/// Chunk 2.6.1: revoke (cut-off) ON the relay — remove the RUNNER itself
/// from its channel (its roster read then fails/returns empty — the
/// enforcement-point denial), best-effort remove the shipped-package grants,
/// then REPLACE the meta with the revoked status (the fold's visible
/// record). Idempotent: missing members are no-ops; a revoked re-run just
/// re-asserts the empty-ish roster + revoked meta.
pub fn revoke_runner_channel(
    store: &StateStore,
    relay_url: &str,
    name: &str,
    state_dir: &std::path::Path,
) -> Result<(), ProvisionError> {
    let rec = store
        .get_runner(name)
        .ok_or_else(|| StateError::RunnerNotFound(name.to_string()))?;
    let console = crate::console::Console::load(state_dir)
        .map_err(|e| StateError::Io(std::io::Error::other(e.to_string())))?;
    let secret = console.identity.secret_seed();
    let rpk = rec.nostr_pubkey.clone();
    // The cut-off: the runner must no longer be able to read its whitelist.
    freehold_core::relay_http::remove_user(relay_url, &secret, &rpk, &rpk)
        .map_err(|e| relay_err("remove runner", e))?;
    // Agents granted via the shipped package — best-effort: the package may
    // already be gone (a prior revoke removed secrets.json), and each
    // individual failure must not block the status flip.
    if let Ok(pkg) = SecretPackage::load(&rec.package_dir) {
        for g in &pkg.grants {
            if let Err(e) = freehold_core::relay_http::remove_user(relay_url, &secret, &rpk, g) {
                tracing::warn!(agent = %g, error = %e, "revoke: best-effort member removal failed");
            }
        }
    }
    let profile = runner_meta(store, name)?;
    freehold_core::relay_http::publish_runner_meta(relay_url, &secret, &profile)
        .map_err(|e| relay_err("publish revoked meta", e))
}

/// Build the runner's CURRENT profile from state (the kind-39000 meta
/// content): identity pubkeys, connector kind/address, status, secret NAME
/// only — never ciphertext/plaintext (the relay is not a holder of secret
/// material).
fn runner_meta(
    store: &StateStore,
    name: &str,
) -> Result<freehold_core::relay_http::RunnerProfile, ProvisionError> {
    let rec = store
        .get_runner(name)
        .ok_or_else(|| StateError::RunnerNotFound(name.to_string()))?;
    let sec = store
        .get_secret(name)
        .ok_or_else(|| StateError::SecretNotFound(name.to_string()))?;
    Ok(freehold_core::relay_http::RunnerProfile {
        name: name.to_string(),
        kind: sec.kind,
        address: sec.address,
        status: match rec.status {
            RunnerStatus::Active => "active",
            RunnerStatus::Revoked => "revoked",
        }
        .to_string(),
        nostr_pubkey: rec.nostr_pubkey,
        enc_pubkey: rec.enc_pubkey,
        secret: name.to_string(), // secret NAME only — never material
        created_at: rec.created_at,
        rotated_at: sec.rotated_at,
        risk: rec.risk_level.clone(),
    })
}

/// Add an EXTRA named secret to an existing runner's package (C0: the
/// litellm runner carries the target credential — the master key — PLUS the
/// provider key and the postgres password as additional `secrets` entries).
/// The new value is sealed to the runner's OWN enc key under aad = its NAME
/// (the same discipline the target credential uses), the package is re-shipped
/// with ALL entries preserved, and a SecretRecord is recorded so rotate/list
/// see it. The Chunk-1 "one secret per runner" phrasing loosens here: the
/// TARGET stays single (its own credential), extras are named companions.
pub fn add_secret(
    store: &StateStore,
    runner: &str,
    secret_name: &str,
    value: &[u8],
) -> Result<SecretRecord, ProvisionError> {
    if secret_name.is_empty() || secret_name.contains('/') || secret_name.starts_with('.') {
        return Err(ProvisionError::InvalidName(secret_name.to_string()));
    }
    let runner_rec = store
        .get_runner(runner)
        .ok_or_else(|| StateError::RunnerNotFound(runner.to_string()))?;
    if runner_rec.status == RunnerStatus::Revoked {
        return Err(ProvisionError::RunnerRevoked(runner.to_string()));
    }
    let enc_pub = hex_to_arr(&runner_rec.enc_pubkey)?;
    let sealed = crypto::seal(&enc_pub, secret_name.as_bytes(), value)?;
    let ciphertext_hex = hex::encode(&sealed);

    // Re-ship the package with the new entry appended to the existing map.
    let mut pkg = SecretPackage::load(&runner_rec.package_dir)?;
    pkg.secrets
        .insert(secret_name.to_string(), ciphertext_hex.clone());
    pkg.write_to_dir(&runner_rec.package_dir)?;

    let now = now_secs();
    let rec = SecretRecord {
        runner: runner.to_string(),
        kind: "extra".to_string(),
        address: String::new(),
        ciphertext_hex,
        created_at: now,
        rotated_at: None,
    };
    store.insert_secret(secret_name, rec.clone());
    if let Err(e) = store.save() {
        // Roll back the in-memory record AND the package entry — the runner
        // must not end up with a secret the CP never recorded.
        store.remove_secret(secret_name);
        let mut pkg = SecretPackage::load(&runner_rec.package_dir).unwrap_or_default();
        pkg.secrets.remove(secret_name);
        let _ = pkg.write_to_dir(&runner_rec.package_dir);
        return Err(e.into());
    }
    Ok(rec)
}

fn relay_err(op: &str, e: String) -> ProvisionError {
    ProvisionError::Io(std::io::Error::other(format!("relay {op}: {e}")))
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

#[cfg(test)]
mod add_secret_tests {
    use super::*;

    fn provision(name: &str) -> (StateStore, tempfile::TempDir) {
        let tmp = tempfile::tempdir().unwrap();
        let store = StateStore::open(tmp.path()).unwrap();
        let runner_dir = tmp.path().join("runner");
        provision_runner(
            &store,
            &ProvisionRequest {
                name,
                kind: "litellm",
                address: "http://192.168.30.7:31400",
                secret: b"master-key-value",
                runner_dir: &runner_dir,
                grants: &[
                    "a1b2c3d4e5f60718293a4b5c6d7e8f90123456789abcdef0123456789abcdef0".to_string(),
                ],
                risk_level: Some("safe"),
            },
        )
        .unwrap();
        (store, tmp)
    }

    // The litellm package carries THREE named secrets: the target credential
    // (master key) + provider key + postgres pw, all sealed to the runner's
    // OWN enc key — the exact C0 shape. Each decrypts under aad = its name.
    #[test]
    fn litellm_package_holds_three_named_secrets() {
        let (store, tmp) = provision("litellm");
        add_secret(&store, "litellm", "provider-key", b"fw_provider_value").unwrap();
        add_secret(&store, "litellm", "postgres-pw", b"pg-secret").unwrap();

        let pkg = SecretPackage::load(&tmp.path().join("runner")).unwrap();
        assert_eq!(pkg.secrets.len(), 3, "package secrets: {:?}", pkg.secrets);
        for name in ["litellm", "provider-key", "postgres-pw"] {
            assert!(pkg.secrets.contains_key(name), "missing {name}");
        }

        // A record per secret, all pointing at the runner; rotate/list see all.
        let snap = store.snapshot();
        assert_eq!(snap.secrets.len(), 3);
        for name in ["litellm", "provider-key", "postgres-pw"] {
            assert_eq!(snap.secrets[&name.to_string()].runner, "litellm");
        }

        // Decrypt each with the runner's OWN enc key — aad = the name.
        let id = identity::Identity::load(&tmp.path().join("runner")).unwrap();
        let enc = hex::decode(&id.enc_secret_hex()).unwrap();
        let mut enc_arr = [0u8; 32];
        enc_arr.copy_from_slice(&enc);
        let master = crypto::open(
            &enc_arr,
            b"litellm",
            &hex::decode(&pkg.secrets["litellm"]).unwrap(),
        )
        .unwrap();
        assert_eq!(master, b"master-key-value");
        let provider = crypto::open(
            &enc_arr,
            b"provider-key",
            &hex::decode(&pkg.secrets["provider-key"]).unwrap(),
        )
        .unwrap();
        assert_eq!(provider, b"fw_provider_value");
        let pg = crypto::open(
            &enc_arr,
            b"postgres-pw",
            &hex::decode(&pkg.secrets["postgres-pw"]).unwrap(),
        )
        .unwrap();
        assert_eq!(pg, b"pg-secret");
    }

    #[test]
    fn add_secret_preserves_target_and_grants() {
        let (store, tmp) = provision("litellm");
        add_secret(&store, "litellm", "provider-key", b"v").unwrap();
        let pkg = SecretPackage::load(&tmp.path().join("runner")).unwrap();
        assert_eq!(pkg.targets["litellm"].address, "http://192.168.30.7:31400");
        assert_eq!(pkg.targets["litellm"].secret, "litellm");
        assert_eq!(
            pkg.grants,
            vec!["a1b2c3d4e5f60718293a4b5c6d7e8f90123456789abcdef0123456789abcdef0"]
        );
    }

    #[test]
    fn add_secret_unknown_runner_fails() {
        let (store, _) = provision("litellm");
        let err = add_secret(&store, "nope", "provider-key", b"v").unwrap_err();
        assert!(err.to_string().contains("not found"), "{err}");
    }

    #[test]
    fn add_secret_invalid_name_fails() {
        let (store, _) = provision("litellm");
        assert!(add_secret(&store, "litellm", "a/b", b"v").is_err());
        assert!(add_secret(&store, "litellm", ".hidden", b"v").is_err());
        assert!(add_secret(&store, "litellm", "", b"v").is_err());
    }
}
