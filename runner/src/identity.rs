//! Runner identity (Phase A2): Nostr keypair (membership/signing) + a separate
//! X25519 encryption keypair (secrets are encrypted TO this key; the runner
//! decrypts locally, uses in memory, forgets).
//!
//! Private keys are NEVER committed. They are injected via env var
//! (`FREEHOLD_RUNNER_NSEC` / `FREEHOLD_RUNNER_ENC_SECRET`) or a state-dir file
//! written with mode 0600 via an atomic temp+rename.
//!
//! Security posture:
//! - `Debug` prints pubkeys only — raw secrets can never reach logs via `{:?}`.
//! - Secrets are zeroized on drop (in-memory copy hygiene).
//! - `PartialEq` is NOT constant-time; only tests compare identities today.
//!   Swap to `subtle::ConstantTimeEq` if secret equality ever crosses a
//!   security boundary.

use std::fmt;
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};

use rand::RngCore;
use secp256k1::{Keypair, Secp256k1, SecretKey};
use serde::{Deserialize, Serialize};
use thiserror::Error;
use x25519_dalek::{PublicKey as X25519PublicKey, StaticSecret};
use zeroize::Zeroize;

pub const NSEC_ENV: &str = "FREEHOLD_RUNNER_NSEC";
pub const ENC_ENV: &str = "FREEHOLD_RUNNER_ENC_SECRET";
pub const IDENTITY_FILE: &str = "identity.json";
const SECRET_LEN: usize = 32;

#[derive(Debug, Error)]
pub enum IdentityError {
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
    #[error("invalid hex: {0}")]
    Hex(#[from] hex::FromHexError),
    #[error("{which} secret must be {SECRET_LEN} bytes, got {got}")]
    BadLength { which: &'static str, got: usize },
    #[error("invalid secp256k1 secret key (scalar must be in [1, n-1])")]
    InvalidNostrSecret,
    #[error("partial secret env: set both {NSEC_ENV} and {ENC_ENV}, or neither")]
    PartialEnv,
    #[error("malformed identity json: {0}")]
    Serde(#[from] serde_json::Error),
    #[error("missing identity: set {NSEC_ENV}/{ENC_ENV} or run `runner keys init`")]
    NotFound,
}

#[derive(Clone, PartialEq, Eq)]
pub struct Identity {
    nostr_secret: [u8; SECRET_LEN],
    enc_secret: [u8; SECRET_LEN],
}

/// Zeroize secrets when the identity is dropped — the model's "uses in memory,
/// forgets" applies to the runner's injected keys too.
impl Drop for Identity {
    fn drop(&mut self) {
        self.nostr_secret.zeroize();
        self.enc_secret.zeroize();
    }
}

impl fmt::Debug for Identity {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Identity")
            .field("nostr_pubkey", &self.nostr_pubkey_hex())
            .field("enc_pubkey", &self.enc_pubkey_hex())
            .finish_non_exhaustive()
    }
}

#[derive(Serialize, Deserialize)]
struct IdentityFile {
    nostr_secret_hex: String,
    enc_secret_hex: String,
}

impl Identity {
    /// Generate a fresh identity: random Nostr secret + random X25519 static secret.
    pub fn generate() -> Self {
        Self {
            nostr_secret: random_bytes(),
            enc_secret: random_bytes(),
        }
    }

    pub fn from_secrets(nostr_secret: [u8; SECRET_LEN], enc_secret: [u8; SECRET_LEN]) -> Self {
        Self {
            nostr_secret,
            enc_secret,
        }
    }

    /// Load identity: env vars win, else the state-dir file.
    pub fn load(dir: &Path) -> Result<Self, IdentityError> {
        Self::load_with(dir, std::env::var(NSEC_ENV).ok(), std::env::var(ENC_ENV).ok())
    }

    /// Load identity from explicit env values. Split out so tests exercise
    /// every branch without mutating process-global env vars.
    pub fn load_with(
        dir: &Path,
        nsec: Option<String>,
        enc: Option<String>,
    ) -> Result<Self, IdentityError> {
        match (nsec, enc) {
            (Some(n), Some(e)) => Self::from_hex(&n, &e),
            // Half-set env is a config error, not a silent fallback to disk:
            // coming up healthy under the WRONG pubkey is the worst failure mode.
            (Some(_), None) | (None, Some(_)) => Err(IdentityError::PartialEnv),
            (None, None) => Self::load_file(dir),
        }
    }

    fn load_file(dir: &Path) -> Result<Self, IdentityError> {
        let path = dir.join(IDENTITY_FILE);
        if !path.exists() {
            return Err(IdentityError::NotFound);
        }
        let raw = fs::read_to_string(&path)?;
        let parsed: IdentityFile = serde_json::from_str(&raw)?;
        Self::from_hex(&parsed.nostr_secret_hex, &parsed.enc_secret_hex)
    }

    fn from_hex(nostr: &str, enc: &str) -> Result<Self, IdentityError> {
        let n = hex::decode(nostr)?;
        let e = hex::decode(enc)?;
        if n.len() != SECRET_LEN {
            return Err(IdentityError::BadLength { which: "nostr", got: n.len() });
        }
        if e.len() != SECRET_LEN {
            return Err(IdentityError::BadLength { which: "encryption", got: e.len() });
        }
        let mut n_arr = [0u8; SECRET_LEN];
        let mut e_arr = [0u8; SECRET_LEN];
        n_arr.copy_from_slice(&n);
        e_arr.copy_from_slice(&e);
        // Reject invalid secp256k1 scalars at load time so every downstream
        // `expect` on the secret is a genuine invariant, not user-controlled
        // input. All-zeros and values >= group order fail here with a clean
        // IdentityError instead of a panic in `runner serve`.
        SecretKey::from_slice(&n_arr).map_err(|_| IdentityError::InvalidNostrSecret)?;
        Ok(Self::from_secrets(n_arr, e_arr))
    }

    /// Persist private keys to `dir/identity.json` (mode 0600 on unix).
    ///
    /// Atomic: writes to a 0600 temp file, fsyncs, then renames over the target.
    /// A pre-existing looser-permissioned file is replaced by a 0600 inode; a
    /// crash mid-write never leaves a partial identity.
    /// The state dir itself is created 0700 (unix).
    pub fn write_to_dir(&self, dir: &Path) -> Result<PathBuf, IdentityError> {
        create_state_dir(dir)?;
        let path = dir.join(IDENTITY_FILE);
        let tmp = dir.join(format!("{IDENTITY_FILE}.tmp"));
        let json = serde_json::to_string_pretty(&IdentityFile {
            nostr_secret_hex: self.nostr_secret_hex(),
            enc_secret_hex: self.enc_secret_hex(),
        })?;

        let wrote = (|| -> std::io::Result<()> {
            #[cfg(unix)]
            {
                use std::os::unix::fs::OpenOptionsExt;
                let mut f = OpenOptions::new()
                    .write(true)
                    .create(true)
                    .truncate(true)
                    .mode(0o600)
                    .open(&tmp)?;
                f.write_all(json.as_bytes())?;
                f.sync_all()?;
            }
            #[cfg(not(unix))]
            {
                // NOTE: non-unix writes use the default umask; mode 0600 is a
                // unix guarantee. Unix is the appliance target.
                fs::write(&tmp, json.as_bytes())?;
            }
            Ok(())
        })();

        if wrote.is_err() {
            // Don't leave secret material in a stray temp file.
            let _ = fs::remove_file(&tmp);
            wrote?;
        }
        fs::rename(&tmp, &path)?;
        Ok(path)
    }

    pub fn nostr_secret_hex(&self) -> String {
        hex::encode(self.nostr_secret)
    }

    pub fn enc_secret_hex(&self) -> String {
        hex::encode(self.enc_secret)
    }

    /// Nostr x-only public key (hex), the identity used in grants/membership.
    pub fn nostr_pubkey_hex(&self) -> String {
        let secp = Secp256k1::new();
        let sk = SecretKey::from_slice(&self.nostr_secret).expect("identity secrets are validated");
        let kp = Keypair::from_secret_key(&secp, &sk);
        let (xonly, _parity) = kp.x_only_public_key();
        hex::encode(xonly.serialize())
    }

    /// X25519 public key (hex) — secrets are encrypted TO this key.
    pub fn enc_pubkey_hex(&self) -> String {
        let pubk = X25519PublicKey::from(&StaticSecret::from(self.enc_secret));
        hex::encode(pubk.as_bytes())
    }
}

fn random_bytes() -> [u8; SECRET_LEN] {
    let mut b = [0u8; SECRET_LEN];
    rand::rng().fill_bytes(&mut b);
    b
}

fn create_state_dir(dir: &Path) -> Result<(), IdentityError> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
        let mut b = fs::DirBuilder::new();
        b.mode(0o700).recursive(true);
        b.create(dir)?;
        // Enforce 0700 even on a pre-existing looser-permissioned dir — later
        // phases store more secret material here.
        fs::set_permissions(dir, fs::Permissions::from_mode(0o700))?;
    }
    #[cfg(not(unix))]
    fs::create_dir_all(dir)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn nsec_zero() -> String {
        "00".repeat(32)
    }

    #[test]
    fn generated_pubkeys_derive_from_secrets() {
        let id = Identity::generate();
        let rederived = Identity::from_hex(&id.nostr_secret_hex(), &id.enc_secret_hex()).unwrap();
        assert_eq!(id, rederived);
        assert_eq!(id.nostr_pubkey_hex(), rederived.nostr_pubkey_hex());
        assert_eq!(id.enc_pubkey_hex(), rederived.enc_pubkey_hex());
    }

    #[test]
    fn uniqueness() {
        let a = Identity::generate();
        let b = Identity::generate();
        assert_ne!(a.nostr_secret_hex(), b.nostr_secret_hex());
        assert_ne!(a.enc_secret_hex(), b.enc_secret_hex());
    }

    #[test]
    fn debug_redacts_secrets() {
        let id = Identity::generate();
        let rendered = format!("{id:?}");
        assert!(!rendered.contains(&id.nostr_secret_hex()));
        assert!(!rendered.contains(&id.enc_secret_hex()));
        assert!(rendered.contains(&id.nostr_pubkey_hex()));
    }

    #[test]
    fn write_load_roundtrip() {
        let dir = tempfile::tempdir().unwrap();
        let id = Identity::generate();
        id.write_to_dir(dir.path()).unwrap();
        let loaded = Identity::load(dir.path()).unwrap();
        assert_eq!(id, loaded);
        // No stray temp file left behind.
        assert_eq!(fs::read_dir(dir.path()).unwrap().count(), 1);
    }

    #[cfg(unix)]
    #[test]
    fn file_and_dir_permissions() {
        use std::os::unix::fs::PermissionsExt;
        let base = tempfile::tempdir().unwrap();
        // A state dir WE create must come out 0700 (tempdir root is 0755, so
        // use a subdir to prove our creation path, not tempfile's).
        let dir = base.path().join("state");
        let id = Identity::generate();
        let path = id.write_to_dir(&dir).unwrap();
        let file_mode = fs::metadata(&path).unwrap().permissions().mode();
        assert_eq!(file_mode & 0o077, 0, "private keys must not be group/other readable");
        let dir_mode = fs::metadata(&dir).unwrap().permissions().mode();
        assert_eq!(dir_mode & 0o077, 0, "state dir should be 0700");
    }

    #[cfg(unix)]
    #[test]
    fn existing_loose_state_dir_is_tightened() {
        use std::os::unix::fs::PermissionsExt;
        let base = tempfile::tempdir().unwrap();
        let dir = base.path().join("state");
        fs::create_dir_all(&dir).unwrap();
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o755)).unwrap();
        let id = Identity::generate();
        id.write_to_dir(&dir).unwrap();
        let mode = fs::metadata(&dir).unwrap().permissions().mode();
        assert_eq!(mode & 0o077, 0, "pre-existing loose state dir must be tightened to 0700");
    }

    #[cfg(unix)]
    #[test]
    fn rewrite_fixes_looser_permissions() {
        use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
        let dir = tempfile::tempdir().unwrap();
        let id = Identity::generate();
        // Simulate a pre-existing world-readable identity from an old bug/manual edit.
        let path = dir.path().join(IDENTITY_FILE);
        OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o644)
            .open(&path)
            .unwrap()
            .write_all(b"old")
            .unwrap();
        id.write_to_dir(dir.path()).unwrap();
        let mode = fs::metadata(&path).unwrap().permissions().mode();
        assert_eq!(mode & 0o077, 0, "rewrite must restore 0600 on a looser file");
    }

    #[test]
    fn env_pairs_win_over_file() {
        let dir = tempfile::tempdir().unwrap();
        let file_id = Identity::generate();
        file_id.write_to_dir(dir.path()).unwrap();
        let env_id = Identity::generate();
        let loaded = Identity::load_with(
            dir.path(),
            Some(env_id.nostr_secret_hex()),
            Some(env_id.enc_secret_hex()),
        )
        .unwrap();
        assert_eq!(loaded, env_id);
    }

    #[test]
    fn file_fallback_when_no_env() {
        let dir = tempfile::tempdir().unwrap();
        let id = Identity::generate();
        id.write_to_dir(dir.path()).unwrap();
        let loaded = Identity::load_with(dir.path(), None, None).unwrap();
        assert_eq!(loaded, id);
    }

    #[test]
    fn missing_identity_errors() {
        let dir = tempfile::tempdir().unwrap();
        assert!(matches!(Identity::load(dir.path()), Err(IdentityError::NotFound)));
    }

    #[test]
    fn partial_env_is_an_error_not_a_fallback() {
        let dir = tempfile::tempdir().unwrap();
        let id = Identity::generate();
        id.write_to_dir(dir.path()).unwrap();
        let err = Identity::load_with(dir.path(), Some(id.nostr_secret_hex()), None).unwrap_err();
        assert!(matches!(err, IdentityError::PartialEnv), "got {err:?}");
        let err = Identity::load_with(dir.path(), None, Some(id.enc_secret_hex())).unwrap_err();
        assert!(matches!(err, IdentityError::PartialEnv), "got {err:?}");
    }

    #[test]
    fn bad_length_reports_which_secret() {
        let id = Identity::generate();
        let err = Identity::from_hex(&"ab".repeat(16), &id.enc_secret_hex()).unwrap_err();
        match err {
            IdentityError::BadLength { which: "nostr", got: 16 } => {}
            other => panic!("expected nostr/16, got {other:?}"),
        }
        let err = Identity::from_hex(&id.nostr_secret_hex(), &"ab".repeat(17)).unwrap_err();
        match err {
            IdentityError::BadLength { which: "encryption", got: 17 } => {}
            other => panic!("expected encryption/17, got {other:?}"),
        }
    }

    #[test]
    fn invalid_nostr_scalar_rejected_at_load() {
        let id = Identity::generate();
        // All-zero nsec is valid hex + valid length but NOT a valid secp256k1 key.
        let err = Identity::from_hex(&nsec_zero(), &id.enc_secret_hex()).unwrap_err();
        assert!(matches!(err, IdentityError::InvalidNostrSecret), "got {err:?}");
    }
}
