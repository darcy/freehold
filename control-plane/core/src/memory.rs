//! Agent memory (Phase D3): relay-persisted, NIP-44-encrypted.
//!
//! The CPA's memory survives runs as kind-30174 engram events on the relay.
//! Buzz REQUIRES engram content to be a valid NIP-44 v2 payload (verified
//! live: other shapes get 400s). The value is therefore encrypted with
//! NIP-44 v2 SELF-encryption (conversation key derived from the agent's own
//! nostr keypair — sender == receiver == the agent): the relay only ever
//! stores ciphertext, and only the agent's key can decrypt. Plaintext never
//! touches the relay, the runner, or the CP state.

use nostr::nips::nip44::{self, Version};
use nostr::{PublicKey, SecretKey};
use thiserror::Error;

pub const MEMORY_KIND: u32 = 30174;

#[derive(Debug, Error)]
pub enum MemoryError {
    #[error("memory key error: {0}")]
    Key(String),
    #[error("memory encrypt: {0}")]
    Encrypt(String),
    #[error("memory decrypt: {0}")]
    Decrypt(String),
}

/// The agent's public key (as rust-nostr expects it).
fn agent_keys(nostr_secret: &[u8; 32]) -> Result<(SecretKey, PublicKey), MemoryError> {
    let sk = SecretKey::from_slice(nostr_secret).map_err(|e| MemoryError::Key(e.to_string()))?;
    let secp_sk = secp256k1::SecretKey::from_slice(&sk.secret_bytes())
        .map_err(|e| MemoryError::Key(e.to_string()))?;
    let kp = secp256k1::Keypair::from_secret_key(&secp256k1::Secp256k1::new(), &secp_sk);
    let (xonly, _parity) = kp.x_only_public_key();
    let pk =
        PublicKey::from_slice(&xonly.serialize()).map_err(|e| MemoryError::Key(e.to_string()))?;
    Ok((sk, pk))
}

/// Encrypt a memory value for the relay: a NIP-44 v2 payload (base64) —
/// the exact engram content shape the buzz ingest validates.
pub fn seal_memory(nostr_secret: &[u8; 32], value: &str) -> Result<String, MemoryError> {
    let (sk, pk) = agent_keys(nostr_secret)?;
    nip44::encrypt(&sk, &pk, value, Version::V2).map_err(|e| MemoryError::Encrypt(e.to_string()))
}

/// Decrypt a stored memory value with the agent's own key. Wrong key /
/// tampered payload -> Err (fail closed, never a partial value).
pub fn open_memory(nostr_secret: &[u8; 32], sealed: &str) -> Result<String, MemoryError> {
    let (sk, pk) = agent_keys(nostr_secret)?;
    nip44::decrypt(&sk, &pk, sealed).map_err(|e| MemoryError::Decrypt(e.to_string()))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn secret() -> [u8; 32] {
        [3u8; 32]
    }

    #[test]
    fn nip44_self_encrypt_roundtrip() {
        use base64::Engine as _;
        let sealed = seal_memory(&secret(), "remember this").unwrap();
        assert!(
            !sealed.contains("remember"),
            "plaintext never in the payload"
        );
        assert_eq!(open_memory(&secret(), &sealed).unwrap(), "remember this");
        // The payload is a valid NIP-44 v2 base64 (what buzz validates).
        assert!(
            base64::engine::general_purpose::STANDARD
                .decode(&sealed)
                .is_ok()
        );
        assert!(sealed.len() >= 32, "v2 payload minimum length");
    }

    #[test]
    fn wrong_key_and_tamper_fail_closed() {
        let sealed = seal_memory(&secret(), "top secret").unwrap();
        assert!(open_memory(&[9u8; 32], &sealed).is_err());
        let mut tampered = sealed.clone().into_bytes();
        let last = tampered.len() - 1;
        tampered[last] = if tampered[last] == b'A' { b'B' } else { b'A' };
        assert!(open_memory(&secret(), String::from_utf8(tampered).unwrap().as_str()).is_err());
    }
}
