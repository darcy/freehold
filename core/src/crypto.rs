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
//! Context binding: the recipient keypair is bound through HKDF `info`
//! (`eph_pk‖recip_pk`), and the caller-supplied `aad` (the secret NAME the
//! blob is filed under) is bound through AEAD associated data. A blob can
//! only be opened with the same secret name it was sealed with, so entries in
//! a multi-secret package cannot be swapped. NOT bound: an epoch — blobs stay
//! valid indefinitely once sealed. Rotation/revoke erase the CP's own copies;
//! a blob another party kept still opens. Epoch pinning needs the runner-side
//! read (Phase A4) to reject stale blobs and is recorded as a Chunk-1 gap.
//!
//! Wire format (versioned):
//! ```text
//! [0]       version byte (currently 1)
//! [1..33]   ephemeral X25519 public key
//! [33..45]  12-byte nonce
//! [45..]    ChaCha20-Poly1305 ciphertext (tag appended)
//! ```

use chacha20poly1305::aead::{Aead, KeyInit, Payload};
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
    #[error("non-contributory X25519 exchange (low-order point)")]
    NonContributory,
    #[error("kdf error: {0}")]
    Kdf(#[from] hkdf::InvalidLength),
    #[error("decryption failed: {0}")]
    Decrypt(#[from] chacha20poly1305::Error),
}

/// Sealed-box key schedule:
/// `AEAD_key = HKDF-SHA256(salt=SALT, ikm=DH(eph_sk, recip_pk), info=eph_pk ‖ recip_pk)`.
/// Binding both public keys into `info` pins a blob to the recipient it was
/// sealed to; rejecting non-contributory DH stops low-order-point forgery
/// (an all-zero shared secret would otherwise yield a PREDICTABLE key to
/// anyone who can write secrets.json, without knowing either private key).
fn derive_key(
    eph_public: &X25519PublicKey,
    recipient_public: &X25519PublicKey,
    shared: &x25519_dalek::SharedSecret,
) -> Result<zeroize::Zeroizing<[u8; KEY_LEN]>, CryptoError> {
    if !shared.was_contributory() {
        return Err(CryptoError::NonContributory);
    }
    let mut info = [0u8; EPHEMERAL_PUB_LEN * 2];
    info[..EPHEMERAL_PUB_LEN].copy_from_slice(eph_public.as_bytes());
    info[EPHEMERAL_PUB_LEN..].copy_from_slice(recipient_public.as_bytes());

    let ikm = zeroize::Zeroizing::new(*shared.as_bytes());
    let mut key = zeroize::Zeroizing::new([0u8; KEY_LEN]);
    Hkdf::<Sha256>::new(Some(SALT), ikm.as_ref()).expand(&info, &mut *key)?;
    Ok(key)
}

/// Seal `plaintext` to `recipient_pubkey` (the runner's X25519 encryption
/// pubkey), bound to `aad` — the secret NAME the blob is filed under. Returns
/// the versioned blob; the caller stores/ships ciphertext only and drops the
/// plaintext.
pub fn seal(
    recipient_pubkey: &[u8; KEY_LEN],
    aad: &[u8],
    plaintext: &[u8],
) -> Result<Vec<u8>, CryptoError> {
    // Fresh ephemeral key per seal: nonce reuse is impossible across seals
    // even if the random nonce ever repeated.
    let eph_secret = StaticSecret::from(random_bytes());
    let eph_public = X25519PublicKey::from(&eph_secret);
    let recipient = X25519PublicKey::from(*recipient_pubkey);
    let shared = eph_secret.diffie_hellman(&recipient);

    let key = derive_key(&eph_public, &recipient, &shared)?;
    let nonce = random_nonce();
    let cipher = ChaCha20Poly1305::new(&(*key).into());
    let ct = cipher.encrypt(&Nonce::from(nonce), Payload { msg: plaintext, aad })?;

    let mut out = Vec::with_capacity(1 + EPHEMERAL_PUB_LEN + NONCE_LEN + ct.len());
    out.push(FORMAT_VERSION);
    out.extend_from_slice(eph_public.as_bytes());
    out.extend_from_slice(&nonce);
    out.extend_from_slice(&ct);
    Ok(out)
}

/// Open a sealed blob with the runner's static secret and the secret NAME it
/// was sealed under. The ONLY key that can decrypt a blob is the private half
/// of the pubkey it was sealed to — the recipient pubkey bound into the key
/// schedule is DERIVED from the secret, so a blob sealed to a different
/// recipient (or forged via a low-order ephemeral point) fails here even
/// against a tampered blob. The `aad` must match the sealing name, so a blob
/// filed under the wrong secret name cannot be swapped into place.
pub fn open(
    recipient_secret: &[u8; KEY_LEN],
    aad: &[u8],
    blob: &[u8],
) -> Result<Vec<u8>, CryptoError> {
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
    let recipient_public = X25519PublicKey::from(&secret);
    let shared = secret.diffie_hellman(&eph_public);

    let key = derive_key(&eph_public, &recipient_public, &shared)?;
    let cipher = ChaCha20Poly1305::new(&(*key).into());
    Ok(cipher.decrypt(&nonce, Payload { msg: &blob[45..], aad })?)
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
        let blob = seal(pubkey.as_bytes(), b"vultr", b"vultr-api-key-123").unwrap();
        let opened = open(secret.to_bytes().as_slice().try_into().unwrap(), b"vultr", &blob).unwrap();
        assert_eq!(opened, b"vultr-api-key-123");
    }

    #[test]
    fn wrong_key_fails() {
        let secret = random_secret();
        let other = random_secret();
        let blob = seal(X25519PublicKey::from(&secret).as_bytes(), b"b2", b"secret").unwrap();
        let err = open(other.to_bytes().as_slice().try_into().unwrap(), b"b2", &blob).unwrap_err();
        assert!(matches!(err, CryptoError::Decrypt(_)), "got {err:?}");
    }

    #[test]
    fn tampered_fails() {
        let secret = random_secret();
        let pubkey = X25519PublicKey::from(&secret);
        // aad must MATCH on both sides here — the assertion is about the
        // bit-flip, not a name mismatch (a mismatch would pass vacuously).
        let mut blob = seal(pubkey.as_bytes(), b"ssh", b"secret").unwrap();
        assert!(
            open(secret.to_bytes().as_slice().try_into().unwrap(), b"ssh", &blob).is_ok(),
            "pre-tamper open must succeed"
        );
        let last = blob.len() - 1;
        blob[last] ^= 0x01;
        let err = open(secret.to_bytes().as_slice().try_into().unwrap(), b"ssh", &blob).unwrap_err();
        assert!(matches!(err, CryptoError::Decrypt(_)), "got {err:?}");
    }

    #[test]
    fn blob_is_pinned_to_its_name() {
        // A blob sealed under one name must not open under another — this is
        // what stops swapping entries in a multi-secret SecretPackage.
        let secret = random_secret();
        let pubkey = X25519PublicKey::from(&secret);
        let blob = seal(pubkey.as_bytes(), b"vultr", b"api-key").unwrap();
        let bytes = secret.to_bytes();
        let key: &[u8; 32] = bytes.as_slice().try_into().unwrap();
        assert!(open(key, b"vultr", &blob).is_ok());
        let err = open(key, b"b2", &blob).unwrap_err();
        assert!(matches!(err, CryptoError::Decrypt(_)), "wrong name must fail: {err:?}");
    }

    #[test]
    fn forged_low_order_ephemeral_is_rejected() {
        // Attacker-writable secret: a blob whose ephemeral pubkey is a
        // low-order point (all-zero). Pre-binding, this yields an all-zero
        // shared secret -> PREDICTABLE AEAD key -> a forgeable credential.
        let secret = random_secret();
        let mut blob = vec![FORMAT_VERSION];
        blob.extend_from_slice(&[0u8; 32]); // low-order ephemeral pubkey
        blob.extend_from_slice(&[0u8; 12]); // nonce
        blob.extend_from_slice(&[0u8; 16]); // ct
        let err = open(secret.to_bytes().as_slice().try_into().unwrap(), b"vultr", &blob).unwrap_err();
        assert!(matches!(err, CryptoError::NonContributory), "got {err:?}");
    }

    #[test]
    fn bad_version_and_short_blob() {
        let secret = random_secret();
        let pubkey = X25519PublicKey::from(&secret);
        let mut blob = seal(pubkey.as_bytes(), b"ssh", b"secret").unwrap();
        blob[0] = 99;
        assert!(matches!(
            open(secret.to_bytes().as_slice().try_into().unwrap(), b"ssh", &blob),
            Err(CryptoError::BadVersion(99))
        ));
        assert!(matches!(open(&[0u8; 32], b"x", b"short"), Err(CryptoError::BadLength(_))));
    }

    #[test]
    fn seals_are_unique_per_call() {
        let secret = random_secret();
        let pubkey = X25519PublicKey::from(&secret);
        let a = seal(pubkey.as_bytes(), b"a", b"same").unwrap();
        let b = seal(pubkey.as_bytes(), b"a", b"same").unwrap();
        assert_ne!(a, b, "ephemeral keys must make every seal unique");
    }
}
