//! Targets this runner can reach (the `list` tool).
//!
//! `local` is always present — a process spawned on the runner's own host.
//! SSH targets (Phase C1) come from the shipped SecretPackage's target
//! metadata; each is a runner flavor over the same `exec` primitive.

use std::collections::BTreeMap;

use freehold_core::secrets::TargetMeta;
use serde::Serialize;

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct Target {
    pub name: String,
    pub kind: String,
}

/// All targets currently reachable by this runner.
pub fn registered(targets: &BTreeMap<String, TargetMeta>) -> Vec<Target> {
    let mut out = vec![Target {
        name: "local".into(),
        kind: "local".into(),
    }];
    for (name, meta) in targets {
        if meta.kind != "local" {
            out.push(Target {
                name: name.clone(),
                kind: meta.kind.clone(),
            });
        }
    }
    out
}
