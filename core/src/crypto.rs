//! Sealed-box encryption TO a runner's X25519 public key.
//!
//! Scheme: ephemeral X25519 + HKDF-SHA256 + ChaCha20-Poly1305. The control
//! plane seals a credential to the runner's encryption pubkey; the runner
//! opens it with its injected static secret. There is NO master key: the CP
//! holds only ciphertext + public keys, the runner holds ciphertext + its own
//! private key. Derivation matches the age-style "encrypt to a pubkey" model
//! the architecture locks in — crypto tool is orthogonal, and X25519 was
//! already the runner's identity key type.
//!
//! Wire format (versioned):
//! ```text
//! [0]       version byte (currently 1)
//! [1..33]   ephemeral X25519 public key
//! [33..45]  12-byte nonce
//! [45..]    ChaCha20-Poly1305 ciphertext (tag appended)
//! ```

use chacha20poly1305::aead::{Aead, KeyInit};
use chacha20poly1305::{ChaCha20Poly1305, Nonce};
use hkdf::Hkdf;
use rand::RngCore;
use sha2::Sha256;
use thiserror::Error;
use x25519_dalek::{PublicKey as X25519PublicKey, StaticSecret};

pub const FORMAT_VERSION: u8 = 1;
pub const KEY_LEN: usize = 32;
const EPHEMERAL_PUB_LEN: usize = 32;
const NONCE_LEN: usize = 12;
const SALT: &[u8] = b"freehold-sealed-box-v1";

#[derive(Debug, Error)]
pub enum CryptoError {
    #[error("unsupported sealed-box format version {0}")]
    BadVersion(u8),
    #[error("sealed blob too short to parse ({0} bytes)")]
    BadLength(usize),
    #[error("kdf error: {0}")]
    Kdf(#[from] hkdf::InvalidLength),
    #[error("decryption failed: {0}")]
    Decrypt(#[from] chacha20poly1305::Error),
}

/// Seal `plaintext` to `recipient_pubkey` (the runner's X25519 encryption
/// pubkey). Returns the versioned blob; the caller stores/ships ciphertext
/// only and drops the plaintext.
pub fn seal(recipient_pubkey: &[u8; KEY_LEN], plaintext: &[u8]) -> Result<Vec<u8>, CryptoError> {
    // Fresh ephemeral key per seal: nonce reuse is impossible across seals
    // even if the random nonce ever repeated.
    let eph_secret = StaticSecret::from(random_bytes());
    let eph_public = X25519PublicKey::from(&eph_secret);
    let recipient = X25519PublicKey::from(*recipient_pubkey);
    let shared = eph_secret.diffie_hellman(&recipient);

    let ikm = zeroize::Zeroizing::new(*shared.as_bytes());
    let mut key = zeroize::Zeroizing::new([0u8; KEY_LEN]);
    Hkdf::<Sha256>::new(Some(SALT), ikm.as_ref()).expand(&[], &mut *key)?;

    let nonce = random_nonce();
    let cipher = ChaCha20Poly1305::new(&(*key).into());
    let ct = cipher.encrypt(&Nonce::from(nonce), plaintext)?;

    let mut out = Vec::with_capacity(1 + EPHEMERAL_PUB_LEN + NONCE_LEN + ct.len());
    out.push(FORMAT_VERSION);
    out.extend_from_slice(eph_public.as_bytes());
    out.extend_from_slice(&nonce);
    out.extend_from_slice(&ct);
    Ok(out)
}

/// Open a sealed blob with the runner's static secret. The ONLY key that can
/// decrypt a blob is the private half of the pubkey it was sealed to.
pub fn open(recipient_secret: &[u8; KEY_LEN], blob: &[u8]) -> Result<Vec<u8>, CryptoError> {
    let header = 1 + EPHEMERAL_PUB_LEN + NONCE_LEN;
    if blob.len() < header {
        return Err(CryptoError::BadLength(blob.len()));
    }
    if blob[0] != FORMAT_VERSION {
        return Err(CryptoError::BadVersion(blob[0]));
    }
    let eph_public = X25519PublicKey::from(<[u8; EPHEMERAL_PUB_LEN]>::try_from(&blob[1..33]).unwrap());
    let nonce = Nonce::from(<[u8; NONCE_LEN]>::try_from(&blob[33..45]).unwrap());
    let secret = StaticSecret::from(*recipient_secret);
    let shared = secret.diffie_hellman(&eph_public);

    let ikm = zeroize::Zeroizing::new(*shared.as_bytes());
    let mut key = zeroize::Zeroizing::new([0u8; KEY_LEN]);
    Hkdf::<Sha256>::new(Some(SALT), ikm.as_ref()).expand(&[], &mut *key)?;

    let cipher = ChaCha20Poly1305::new(&(*key).into());
    Ok(cipher.decrypt(&nonce, &blob[45..])?)
}

fn random_bytes() -> [u8; KEY_LEN] {
    let mut b = [0u8; KEY_LEN];
    rand::rng().fill_bytes(&mut b);
    b
}

fn random_nonce() -> [u8; NONCE_LEN] {
    let mut b = [0u8; NONCE_LEN];
    rand::rng().fill_bytes(&mut b);
    b
}

#[cfg(test)]
mod tests {
    use super::*;

    // x25519-dalek pins rand_core 0.6; rand 0.9 is rand_core 0.9, so
    // random_from_rng is off the table — we fill bytes ourselves.
    fn random_secret() -> StaticSecret {
        let mut b = [0u8; KEY_LEN];
        rand::rng().fill_bytes(&mut b);
        StaticSecret::from(b)
    }

    #[test]
    fn roundtrip() {
        let secret = random_secret();
        let pubkey = X25519PublicKey::from(&secret);
        let blob = seal(pubkey.as_bytes(), b"vultr-api-key-123").unwrap();
        let opened = open(secret.to_bytes().as_slice().try_into().unwrap(), &blob).unwrap();
        assert_eq!(opened, b"vultr-api-key-123");
    }

    #[test]
    fn wrong_key_fails() {
        let secret = random_secret();
        let other = random_secret();
        let blob = seal(X25519PublicKey::from(&secret).as_bytes(), b"secret").unwrap();
        let err = open(other.to_bytes().as_slice().try_into().unwrap(), &blob).unwrap_err();
        assert!(matches!(err, CryptoError::Decrypt(_)), "got {err:?}");
    }

    #[test]
    fn tampered_fails() {
        let secret = random_secret();
        let pubkey = X25519PublicKey::from(&secret);
        let mut blob = seal(pubkey.as_bytes(), b"secret").unwrap();
        let last = blob.len() - 1;
        blob[last] ^= 0x01;
        let err = open(secret.to_bytes().as_slice().try_into().unwrap(), &blob).unwrap_err();
        assert!(matches!(err, CryptoError::Decrypt(_)), "got {err:?}");
    }

    #[test]
    fn bad_version_and_short_blob() {
        let secret = random_secret();
        let pubkey = X25519PublicKey::from(&secret);
        let mut blob = seal(pubkey.as_bytes(), b"secret").unwrap();
        blob[0] = 99;
        assert!(matches!(
            open(secret.to_bytes().as_slice().try_into().unwrap(), &blob),
            Err(CryptoError::BadVersion(99))
        ));
        assert!(matches!(open(&[0u8; 32], b"short"), Err(CryptoError::BadLength(_))));
    }

    #[test]
    fn seals_are_unique_per_call() {
        let secret = random_secret();
        let pubkey = X25519PublicKey::from(&secret);
        let a = seal(pubkey.as_bytes(), b"same").unwrap();
        let b = seal(pubkey.as_bytes(), b"same").unwrap();
        assert_ne!(a, b, "ephemeral keys must make every seal unique");
    }
}
