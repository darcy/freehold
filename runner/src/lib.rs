//! Freehold runner — the privileged connector bridge.
//!
//! Agent = brain; runner = dumb privileged hands. The runner owns the connections
//! and executes the agent's commands verbatim (one generic primitive). Chunk 1
//! scope: no Buzz, no k8s — identity stand-in + MCP tool server skeleton.

pub mod identity;
pub mod mcp;
pub mod registry;
