//! Freehold orchestrator — the scripted CPA stand-in.
//!
//! Holds the AGENT identity (private key), signs every MCP call (Phase D
//! grants), and drives the engine room: onboarding (provision → ship →
//! self-check 🟢 → grant → report), readiness, exec, and scripted demo
//! steps. It is NOT a reasoning agent — the script walks the flows the
//! acceptance criteria demand, proving plumbing rather than judgment.

pub mod client;
pub mod flows;
