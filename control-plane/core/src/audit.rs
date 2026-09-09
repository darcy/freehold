//! Runner audit records (Phase A6).
//!
//! The runner signs a Nostr-style event for every executed command (agent
//! pubkey, target, command, result) — an identity-scoped audit trail. Chunk 1
//! has no relay yet, so events are appended to a local 0600 log file by the
//! runner; the same signed event shape ports to the relay in Chunk 2.

use secp256k1::{Keypair, Secp256k1, SecretKey, XOnlyPublicKey, schnorr::Signature};
use serde::{Deserialize, Serialize};
use thiserror::Error;
use zeroize::Zeroizing;

#[derive(Debug, Error)]
pub enum AuditError {
    #[error("invalid secret key: {0}")]
    SecretKey(#[from] secp256k1::Error),
    #[error("invalid hex: {0}")]
    Hex(#[from] hex::FromHexError),
    #[error("audit message too large for signing ({0} bytes; max 64)")]
    MessageTooLarge(usize),
    #[error("audit event build failed: {0}")]
    Message(String),
}

/// A signed audit record: `content` is opaque JSON produced by the runner,
/// `sig` is a BIP-340 schnorr signature over `content` by `pubkey`.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SignedEvent {
    pub pubkey: String,
    pub content: String,
    pub sig: String,
}

/// Sign `content` with the runner's Nostr secret key (32 bytes).
/// BIP-340 schnorr requires a 32-byte tagged message; the content is hashed
/// with SHA-256 first (the standard "sign what you hash" shape — the signed
/// bytes are `sha256(content)`, recorded verbatim so verification is
/// unambiguous).
pub fn sign_event(nostr_secret: &[u8; 32], content: &str) -> Result<SignedEvent, AuditError> {
    let secp = Secp256k1::new();
    let sk = SecretKey::from_slice(nostr_secret)?;
    let kp = Keypair::from_secret_key(&secp, &sk);

    use sha2::{Digest, Sha256};
    let msg = Sha256::digest(content.as_bytes());
    // Deterministic (no aux rand): audit records are public, per-content
    // messages — reproducibility beats randomized signatures here.
    let sig = secp.sign_schnorr_no_aux_rand(&msg, &kp);

    Ok(SignedEvent {
        pubkey: hex::encode(kp.x_only_public_key().0.serialize()),
        content: content.to_string(),
        sig: hex::encode(sig.to_byte_array()),
    })
}

/// Verify a signed event: `sig` must be a valid BIP-340 signature by `pubkey`
/// over `sha256(content)`.
pub fn verify_event(event: &SignedEvent) -> Result<(), AuditError> {
    let secp = Secp256k1::new();

    let pubkey_bytes = hex::decode(&event.pubkey)?;
    let xonly = XOnlyPublicKey::from_slice(&pubkey_bytes)?;
    let sig_bytes = hex::decode(&event.sig)?;
    let sig = Signature::from_slice(&sig_bytes)?;

    use sha2::{Digest, Sha256};
    let msg = Sha256::digest(event.content.as_bytes());

    secp.verify_schnorr(&sig, &msg, &xonly)
        .map_err(AuditError::SecretKey)?;
    Ok(())
}

/// Build an audit event without touching secp256k1 state repeatedly.
pub struct Auditor {
    secret: Zeroizing<[u8; 32]>,
}

impl Auditor {
    pub fn new(nostr_secret: [u8; 32]) -> Self {
        Self {
            secret: Zeroizing::new(nostr_secret),
        }
    }

    /// Sign a content string; zeroize-safe wrapper over `sign_event`.
    pub fn sign(&self, content: &str) -> Result<SignedEvent, AuditError> {
        sign_event(&self.secret, content)
    }

    /// Build a kind-N NIP-01 audit event (Phase D5): id over the canonical
    /// event serialization, BIP-340 signature over the id — the SAME event
    /// that gets spooled locally AND published to the relay (kind 48001 for
    /// exec audit; the relay's own verifier accepts exactly this shape).
    pub fn event(
        &self,
        kind: u32,
        tags: Vec<Vec<String>>,
        content: &str,
    ) -> Result<serde_json::Value, AuditError> {
        let created_at = crate::auth::now_secs();
        let (pubkey, id, sig) =
            crate::nip98::sign_event(&self.secret, kind, created_at, tags.clone(), content)
                .map_err(|e| AuditError::Message(e.to_string()))?;
        Ok(serde_json::json!({
            "id": id,
            "pubkey": pubkey,
            "created_at": created_at,
            "kind": kind,
            "tags": tags,
            "content": content,
            "sig": sig,
        }))
    }

    /// The raw secret seed (copied out of the zeroized holder) — needed for
    /// relay NIP-98 publish auth; temporary, zeroized on drop.
    pub fn secret(&self) -> [u8; 32] {
        *self.secret
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use rand::RngCore;

    fn random_secret() -> [u8; 32] {
        let mut b = [0u8; 32];
        rand::rng().fill_bytes(&mut b);
        b
    }

    #[test]
    fn sign_then_verify_roundtrip() {
        let secret = random_secret();
        let event = sign_event(&secret, r#"{"cmd":"uname -a","target":"local","exit":0}"#).unwrap();
        assert_eq!(event.pubkey.len(), 64);
        assert_eq!(event.sig.len(), 128);
        assert!(verify_event(&event).is_ok());
    }

    #[test]
    fn tampered_content_fails_verification() {
        let secret = random_secret();
        let mut event = sign_event(&secret, "original content").unwrap();
        event.content = "tampered content".to_string();
        assert!(verify_event(&event).is_err());
    }

    #[test]
    fn wrong_key_fails_verification() {
        let event = sign_event(&random_secret(), "content").unwrap();
        assert!(verify_event(&event).is_ok(), "its own key verifies");

        // Sign the same content with a DIFFERENT key and verify against the
        // original pubkey.
        let other = sign_event(&random_secret(), "content").unwrap();
        let mut forged = event.clone();
        forged.sig = other.sig;
        assert!(
            verify_event(&forged).is_err(),
            "signature from another key must fail"
        );
    }

    #[test]
    fn auditor_signs() {
        let a = Auditor::new(random_secret());
        let event = a.sign("exec logged").unwrap();
        assert!(verify_event(&event).is_ok());
    }
}
