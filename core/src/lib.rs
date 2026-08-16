//! Freehold shared core.
//!
//! Everything both the control plane and the runner need, with no product
//! logic: identity keypairs, sealed-box crypto, secret packaging, and the
//! atomic-0600 file discipline used everywhere secret material touches disk.

pub mod audit;
pub mod auth;
pub mod crypto;
pub mod futil;
pub mod identity;
pub mod secrets;
