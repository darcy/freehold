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
//! - The struct's secret copies are zeroized on drop; intermediate buffers
//!   (decoded bytes, raw file text, serialized JSON) are `Zeroizing`-wrapped.
//!   The hex-string getters (`nostr_secret_hex`/`enc_secret_hex`) return
//!   short-lived plain `String`s that are NOT wiped — keep them out of logs.
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
use zeroize::{Zeroize, ZeroizeOnDrop};

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

/// The serialized form holds both private keys in cleartext — wipe on drop
/// covers the load path AND the struct `write_to_dir` serializes.
#[derive(Serialize, Deserialize, ZeroizeOnDrop)]
struct IdentityFile {
    nostr_secret_hex: String,
    enc_secret_hex: String,
}

impl Identity {
    /// Generate a fresh identity: random Nostr secret + random X25519 static secret.
    pub fn generate() -> Self {
        // Routed through the validating constructor so the invariant in
        // `nostr_pubkey_hex` is literal: a random 32-byte scalar that
        // `SecretKey::from_slice` rejects has probability ≈ 2⁻¹²⁸ (same order
        // as an RNG failure).
        Self::from_secrets(random_bytes(), random_bytes())
            .expect("random secp256k1 scalar is valid")
    }

    /// Build from raw secrets. Validates the Nostr scalar so every constructor
    /// path guarantees `nostr_pubkey_hex`'s invariant — invalid scalars
    /// (all-zero, >= group order) are rejected here, not later.
    pub fn from_secrets(
        nostr_secret: [u8; SECRET_LEN],
        enc_secret: [u8; SECRET_LEN],
    ) -> Result<Self, IdentityError> {
        SecretKey::from_slice(&nostr_secret).map_err(|_| IdentityError::InvalidNostrSecret)?;
        Ok(Self {
            nostr_secret,
            enc_secret,
        })
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
        // Zeroizing: the raw file text holds both secrets (this runs on every
        // `runner serve`).
        let raw = zeroize::Zeroizing::new(fs::read_to_string(&path)?);
        let parsed: IdentityFile = serde_json::from_str(&raw)?;
        Self::from_hex(&parsed.nostr_secret_hex, &parsed.enc_secret_hex)
    }

    fn from_hex(nostr: &str, enc: &str) -> Result<Self, IdentityError> {
        // Zeroizing: decoded bytes are secret material between here and the
        // struct copy.
        let n = zeroize::Zeroizing::new(hex::decode(nostr)?);
        let e = zeroize::Zeroizing::new(hex::decode(enc)?);
        if n.len() != SECRET_LEN {
            return Err(IdentityError::BadLength { which: "nostr", got: n.len() });
        }
        if e.len() != SECRET_LEN {
            return Err(IdentityError::BadLength { which: "encryption", got: e.len() });
        }
        let mut n_arr = zeroize::Zeroizing::new([0u8; SECRET_LEN]);
        let mut e_arr = zeroize::Zeroizing::new([0u8; SECRET_LEN]);
        n_arr.copy_from_slice(&n);
        e_arr.copy_from_slice(&e);
        // Scalar validity is enforced in from_secrets (single validation point).
        // The Zeroizing wrappers wipe on EVERY exit — including the `?`
        // invalid-scalar path, which is exactly the malformed-NSEC case.
        Self::from_secrets(*n_arr, *e_arr)
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
        // Zeroizing: the serialized form holds both private keys in cleartext.
        let json = zeroize::Zeroizing::new(serde_json::to_string_pretty(&IdentityFile {
            nostr_secret_hex: self.nostr_secret_hex(),
            enc_secret_hex: self.enc_secret_hex(),
        })?);

        let wrote = (|| -> std::io::Result<()> {
            let mut f = open_secret_temp(&tmp)?;
            f.write_all(json.as_bytes())?;
            f.sync_all()
        })();

        if wrote.is_err() {
            // Don't leave secret material in a stray temp file.
            let _ = fs::remove_file(&tmp);
            wrote?;
        }
        if let Err(e) = fs::rename(&tmp, &path) {
            // Same rule: a failed rename must not strand both private keys.
            let _ = fs::remove_file(&tmp);
            return Err(e.into());
        }
        Ok(path)
    }

    /// Rotate a copy of the current `identity.json` (`.bak`, then `.bak.N` —
    /// a second `--force` must never clobber the ORIGINAL key's backup).
    ///
    /// The destination is born at 0600 (unix) and lands via atomic rename —
    /// the backup's private keys are never exposed in a loose-permission or
    /// partially-written file, matching `write_to_dir`'s discipline. (`fs::copy`
    /// would open the destination 0666&~umask → 0644, copy the secrets, then
    /// apply the source's bits — exactly the short bad window this avoids.)
    pub fn backup_identity(dir: &Path) -> Result<PathBuf, IdentityError> {
        let path = dir.join(IDENTITY_FILE);
        if !path.exists() {
            return Err(IdentityError::NotFound);
        }
        // The backup must capture the OLD key, so it runs before write_to_dir
        // tightens the dir — tighten here so the backup never lands in a
        // world-traversable directory either.
        create_state_dir(dir)?;
        // TOCTOU note: the rotate-and-copy here is not exclusive against a
        // concurrent `keys init --force`, but both runs copy the SAME source
        // (identity.json) and rename is atomic last-wins, so a collision
        // produces an identical backup, never mixed content.
        let mut target = dir.join(format!("{IDENTITY_FILE}.bak"));
        let mut i = 1;
        while target.exists() {
            target = dir.join(format!("{IDENTITY_FILE}.bak.{i}"));
            i += 1;
        }
        copy_secret_file(&path, &target)?;
        Ok(target)
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

/// Open a temp file for secret material, born 0600 (unix) via O_CREAT|O_EXCL.
/// `mode()` only applies at creation, so a temp stranded by a killed run must
/// never be reused with its (perhaps loose) existing bits — if it exists,
/// remove it and retry once. Non-unix continues with the umask caveat (unix is
/// the appliance target).
fn open_secret_temp(tmp: &Path) -> std::io::Result<fs::File> {
    let open = || {
        let mut opts = OpenOptions::new();
        opts.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            opts.mode(0o600);
        }
        opts.open(tmp)
    };
    match open() {
        Ok(f) => Ok(f),
        // A stranded temp from a killed run is not something to write secrets
        // into.
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {
            fs::remove_file(tmp)?;
            open()
        }
        Err(e) => Err(e),
    }
}

/// Copy a secret-bearing file to `dst` with the destination born at 0600
/// (unix) + atomic rename — no window where the contents sit loose or partial.
fn copy_secret_file(src: &Path, dst: &Path) -> Result<(), IdentityError> {
    let name = dst
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or("backup");
    let tmp = dst.with_file_name(format!("{name}.tmp"));
    let copied = (|| -> std::io::Result<()> {
        let mut src_f = fs::File::open(src)?;
        let mut dst_f = open_secret_temp(&tmp)?;
        std::io::copy(&mut src_f, &mut dst_f)?;
        dst_f.sync_all()
    })();
    if copied.is_err() {
        let _ = fs::remove_file(&tmp);
        copied?;
    }
    if let Err(e) = fs::rename(&tmp, dst) {
        let _ = fs::remove_file(&tmp);
        return Err(e.into());
    }
    Ok(())
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
        // Only tighten a pre-existing LOOSE dir (a user-supplied --state-dir
        // pointing at a shared dir shouldn't be silently chmodded if it's
        // already tight). Later phases store more secret material here.
        let mode = fs::metadata(dir)?.permissions().mode();
        if mode & 0o077 != 0 {
            tracing::warn!(
                dir = %dir.display(),
                mode = format_args!("{mode:o}"),
                "tightening state dir to 0700 (holds secret material)"
            );
            fs::set_permissions(dir, fs::Permissions::from_mode(0o700))?;
        }
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

    #[cfg(unix)]
    #[test]
    fn backup_is_0600_even_from_loose_source() {
        use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
        let base = tempfile::tempdir().unwrap();
        let dir = base.path().join("state");
        fs::create_dir_all(&dir).unwrap();
        // Restore-from-tarball case: identity.json came back 0644.
        let path = dir.join(IDENTITY_FILE);
        OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o644)
            .open(&path)
            .unwrap()
            .write_all(b"old-secret-bytes")
            .unwrap();
        let bak = Identity::backup_identity(&dir).unwrap();
        let mode = fs::metadata(&bak).unwrap().permissions().mode();
        assert_eq!(mode & 0o077, 0, "backup must be 0600 regardless of source bits");
    }

    #[cfg(unix)]
    #[test]
    fn backup_rotates_instead_of_clobbering() {
        use std::os::unix::fs::OpenOptionsExt;
        let base = tempfile::tempdir().unwrap();
        let dir = base.path().join("state");
        fs::create_dir_all(&dir).unwrap();
        let path = dir.join(IDENTITY_FILE);
        OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(&path)
            .unwrap()
            .write_all(b"key-v1")
            .unwrap();
        let first = Identity::backup_identity(&dir).unwrap();
        let second = Identity::backup_identity(&dir).unwrap();
        assert_ne!(first, second, "second backup must not clobber the first");
        assert_eq!(fs::read_to_string(&first).unwrap(), "key-v1");
        assert_eq!(fs::read_to_string(&second).unwrap(), "key-v1");
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
