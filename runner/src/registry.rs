//! Targets this runner can reach (the `list` tool).
//!
//! Phase A3 ships an empty registry + the wire shape. Phase C adds the real
//! connectors (ssh, vultr, b2), each a runner flavor over the same core.

use serde::Serialize;

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct Target {
    pub name: String,
    pub kind: String,
}

/// All targets currently reachable by this runner.
pub fn registered() -> Vec<Target> {
    Vec::new()
}
