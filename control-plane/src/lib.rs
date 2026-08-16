//! Freehold control plane (Chunk 1, Phase B).
//!
//! The management layer for ONE relay scope. Phase B = the secret
//! PROVISIONER: generate runner identities, seal credentials to the runner's
//! encryption pubkey, ship ciphertext-only packages, and track runners +
//! secrets locally. No master key: the CP never holds a private key or a
//! plaintext credential.

pub mod provisioner;
pub mod state;
