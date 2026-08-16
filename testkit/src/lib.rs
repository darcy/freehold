//! Freehold hermetic test fixtures — shared by the connector tests and the
//! Phase G acceptance script. Everything here runs against loopback only;
//! no real Vultr/B2/SSH dependency anywhere.

pub mod mock;
pub mod sshd;
