//! The runner's on-disk secret config: name -> sealed ciphertext (hex).
//!
//! WRITTEN by the control plane at provision/rotate; READ by the runner
//! (Phase A4+), which opens each ciphertext with its injected encryption key.
//! Plaintext never sits in this file, in the CP's records, or in agent
//! context — the runner references secrets BY NAME only.

use std::collections::BTreeMap;
use std::path::Path;

use serde::{Deserialize, Serialize};

use crate::futil;

pub const SECRETS_FILE: &str = "secrets.json";

/// Non-secret per-target metadata the CP ships alongside the ciphertext so
/// the runner can enumerate what it reaches and how to connect.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct TargetMeta {
    pub kind: String,
    /// Connector-specific address (e.g. `user@host:port` for ssh).
    pub address: String,
    /// Which entry in `secrets` holds this target's credential.
    pub secret: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct SecretPackage {
    /// secret name -> sealed-box ciphertext (hex)
    pub secrets: BTreeMap<String, String>,
    /// target name -> connector metadata (local is implicit; kind: ssh, …)
    #[serde(default)]
    pub targets: BTreeMap<String, TargetMeta>,
    /// AGENT pubkeys (Nostr x-only, hex) allowed to call this runner.
    /// Enforced locally at the MCP boundary; ports to relay membership in
    /// Chunk 2. Empty = nobody may call (fail closed).
    #[serde(default)]
    pub grants: Vec<String>,
}

impl SecretPackage {
    /// Atomic 0600 write into `dir` (creates the dir 0700).
    pub fn write_to_dir(&self, dir: &Path) -> std::io::Result<()> {
        futil::ensure_private_dir(dir)?;
        let json = serde_json::to_vec_pretty(self)
            .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e))?;
        futil::write_0600_atomic(&dir.join(SECRETS_FILE), &json)
    }

    /// Load a package previously shipped to `dir` for the runner.
    pub fn load(dir: &Path) -> std::io::Result<Self> {
        let raw = std::fs::read_to_string(dir.join(SECRETS_FILE))?;
        serde_json::from_str(&raw)
            .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn package_roundtrip() {
        let dir = tempfile::tempdir().unwrap();
        let pkg = SecretPackage {
            secrets: BTreeMap::from([("vultr".into(), "ciphertext-hex".into())]),
            targets: BTreeMap::from([(
                "vultr".into(),
                TargetMeta {
                    kind: "vultr".into(),
                    address: "api.vultr.com".into(),
                    secret: "vultr".into(),
                },
            )]),
            grants: vec!["agent-pubkey".into()],
        };
        pkg.write_to_dir(dir.path()).unwrap();
        let loaded = SecretPackage::load(dir.path()).unwrap();
        assert_eq!(loaded, pkg);
    }

    #[cfg(unix)]
    #[test]
    fn package_is_0600() {
        use std::os::unix::fs::PermissionsExt;
        let dir = tempfile::tempdir().unwrap();
        SecretPackage::default().write_to_dir(dir.path()).unwrap();
        let mode = std::fs::metadata(dir.path().join(SECRETS_FILE))
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(
            mode & 0o077,
            0,
            "secrets.json must not be group/other readable"
        );
    }
}
