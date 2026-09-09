//! The console agent — the CP's own identity for ADMIN/ops calls.
//!
//! Grants are whitelists of agent pubkeys; the console is just another agent
//! on the list. It exists so the web UI can do the one thing UI alone can't:
//! ask the runner (a GRANTED peer) for its live readiness through the same
//! signed MCP channel everyone else uses — no passwordless side door, the
//! runner still fails closed.
//!
//! Lazily created the first time the web server starts, persisted 0600 under
//! the CP state dir like any other agent identity. Never leaves the machine,
//! never shows its secret; the UI prints only the pubkey so the operator can
//! grant the console to runners provisioned before the UI existed.

use std::path::Path;

use freehold_core::identity::{self, Identity};
use thiserror::Error;

pub const CONSOLE_DIR: &str = "console";

#[derive(Debug, Error)]
pub enum ConsoleError {
    #[error(
        "console identity missing in {0} (run `control-plane serve` once or point --state-dir at the real CP state)"
    )]
    MissingIdentity(String),
    #[error("identity error: {0}")]
    Identity(#[from] identity::IdentityError),
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
}

#[derive(Clone)]
pub struct Console {
    pub identity: Identity,
}

impl Console {
    pub fn pubkey(&self) -> String {
        self.identity.nostr_pubkey_hex()
    }

    /// Load an EXISTING console identity — hard error when absent. For
    /// privileged writes (relay grant publishing, Phase D): a wrong or fresh
    /// --state-dir must never silently mint a NEW key that signs publishes
    /// nobody recognizes.
    pub fn load(state_dir: &Path) -> Result<Self, ConsoleError> {
        let dir = state_dir.join(CONSOLE_DIR);
        if !dir.join(identity::IDENTITY_FILE).exists() {
            return Err(ConsoleError::MissingIdentity(dir.display().to_string()));
        }
        Ok(Self {
            identity: Identity::load(&dir)?,
        })
    }

    /// Load the console identity, creating it if this is the first run.
    /// The identity file is written 0600 via the standard atomic path.
    pub fn load_or_create(state_dir: &Path) -> Result<Self, ConsoleError> {
        let dir = state_dir.join(CONSOLE_DIR);
        let id = if dir.join(identity::IDENTITY_FILE).exists() {
            Identity::load(&dir)?
        } else {
            let id = Identity::generate();
            id.write_to_dir(&dir)?;
            id
        };
        Ok(Self { identity: id })
    }
}
