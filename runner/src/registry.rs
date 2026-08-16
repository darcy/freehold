//! Targets this runner can reach (the `list` tool).
//!
//! Chunk 1 ships the `local` target — a process spawned on the runner's own
//! host. Phase C adds the connector targets (ssh, vultr, b2), each a runner
//! flavor over the same `exec` primitive.

use serde::Serialize;

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct Target {
    pub name: String,
    pub kind: String,
}

/// All targets currently reachable by this runner.
pub fn registered() -> Vec<Target> {
    vec![Target {
        name: "local".into(),
        kind: "local".into(),
    }]
}
