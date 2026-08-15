//! Runner identity (Phase A2): Nostr keypair (membership/signing) + a separate
//! X25519 encryption keypair (secrets are encrypted TO this key; the runner
//! decrypts locally, uses in memory, forgets).
//!
//! Private keys are NEVER committed. They are injected via env var
//! (`FREEFOLD_RUNNER_NSEC` / `FREEFOLD_RUNNER_ENC_SECRET`) or a state-dir file
//! written with mode 0600.

use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};

use rand::RngCore;
use secp256k1::{Keypair, Secp256k1, SecretKey};
use serde::{Deserialize, Serialize};
use thiserror::Error;
use x25519_dalek::{PublicKey as X25519PublicKey, StaticSecret};

pub const NSEC_ENV: &str = "FREEFOLD_RUNNER_NSEC";
pub const ENC_ENV: &str = "FREEFOLD_RUNNER_ENC_SECRET";
pub const IDENTITY_FILE: &str = "identity.json";
const SECRET_LEN: usize = 32;

#[derive(Debug, Error)]
pub enum IdentityError {
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
    #[error("invalid hex: {0}")]
    Hex(#[from] hex::FromHexError),
    #[error("secret must be {SECRET_LEN} bytes, got {0}")]
    BadLength(usize),
    #[error("malformed identity json: {0}")]
    Serde(#[from] serde_json::Error),
    #[error("missing identity: set {NSEC_ENV}/{ENC_ENV} or run `runner keys init`")]
    NotFound,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Identity {
    nostr_secret: [u8; SECRET_LEN],
    enc_secret: [u8; SECRET_LEN],
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
        match (std::env::var(NSEC_ENV), std::env::var(ENC_ENV)) {
            (Ok(n), Ok(e)) => Self::from_hex(&n, &e),
            _ => {
                let path = dir.join(IDENTITY_FILE);
                if !path.exists() {
                    return Err(IdentityError::NotFound);
                }
                let raw = fs::read_to_string(&path)?;
                let parsed: IdentityFile = serde_json::from_str(&raw)?;
                Self::from_hex(&parsed.nostr_secret_hex, &parsed.enc_secret_hex)
            }
        }
    }

    fn from_hex(nostr: &str, enc: &str) -> Result<Self, IdentityError> {
        let n = hex::decode(nostr)?;
        let e = hex::decode(enc)?;
        if n.len() != SECRET_LEN || e.len() != SECRET_LEN {
            return Err(IdentityError::BadLength(std::cmp::max(n.len(), e.len())));
        }
        let mut n_arr = [0u8; SECRET_LEN];
        let mut e_arr = [0u8; SECRET_LEN];
        n_arr.copy_from_slice(&n);
        e_arr.copy_from_slice(&e);
        Ok(Self::from_secrets(n_arr, e_arr))
    }

    /// Persist private keys to `dir/identity.json` with mode 0600 (unix).
    pub fn write_to_dir(&self, dir: &Path) -> Result<PathBuf, IdentityError> {
        fs::create_dir_all(dir)?;
        let path = dir.join(IDENTITY_FILE);
        let json = serde_json::to_string_pretty(&IdentityFile {
            nostr_secret_hex: self.nostr_secret_hex(),
            enc_secret_hex: self.enc_secret_hex(),
        })?;

        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            let mut f = OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o600)
                .open(&path)?;
            f.write_all(json.as_bytes())?;
            f.sync_all()?;
        }
        #[cfg(not(unix))]
        fs::write(&path, json)?;
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
        let sk = SecretKey::from_slice(&self.nostr_secret).expect("32-byte identity secret");
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

#[cfg(test)]
mod tests {
    use super::*;

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
    fn write_load_roundtrip() {
        let dir = tempfile::tempdir().unwrap();
        let id = Identity::generate();
        id.write_to_dir(dir.path()).unwrap();
        let loaded = Identity::load(dir.path()).unwrap();
        assert_eq!(id, loaded);
    }

    #[cfg(unix)]
    #[test]
    fn file_is_0600() {
        use std::os::unix::fs::PermissionsExt;
        let dir = tempfile::tempdir().unwrap();
        let id = Identity::generate();
        let path = id.write_to_dir(dir.path()).unwrap();
        let mode = fs::metadata(&path).unwrap().permissions().mode();
        assert_eq!(mode & 0o077, 0, "private keys must not be group/other readable");
    }

    #[test]
    fn env_overrides_file() {
        let dir = tempfile::tempdir().unwrap();
        let file_id = Identity::generate();
        file_id.write_to_dir(dir.path()).unwrap();

        let env_id = Identity::generate();
        unsafe {
            std::env::set_var(NSEC_ENV, env_id.nostr_secret_hex());
            std::env::set_var(ENC_ENV, env_id.enc_secret_hex());
        }
        let loaded = Identity::load(dir.path()).unwrap();
        unsafe {
            std::env::remove_var(NSEC_ENV);
            std::env::remove_var(ENC_ENV);
        }
        assert_eq!(loaded, env_id);
    }

    #[test]
    fn missing_identity_errors() {
        let dir = tempfile::tempdir().unwrap();
        assert!(matches!(Identity::load(dir.path()), Err(IdentityError::NotFound)));
    }
}
