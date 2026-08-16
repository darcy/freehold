//! Freehold runner — the privileged connector bridge.
//!
//! Agent = brain; runner = dumb privileged hands. The runner owns the connections
//! and executes the agent's commands verbatim (one generic primitive). Chunk 1
//! scope: no Buzz, no k8s — identity stand-in + MCP tool server skeleton.
//!
//! Identity, crypto, and secret packaging live in the shared `freehold-core`
//! crate so the control plane and runner speak the same on-disk formats.

pub use freehold_core::{crypto, identity, secrets};

pub mod mcp;
pub mod registry;
