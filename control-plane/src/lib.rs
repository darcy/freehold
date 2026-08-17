//! Freehold control plane (Chunk 1, Phase B; Phase C OPERATE mode).
//!
//! The management layer for ONE relay scope. Phase B = the secret
//! PROVISIONER: generate runner identities, seal credentials to the runner's
//! encryption pubkey, ship ciphertext-only packages, and track runners +
//! secrets locally. No master key: the CP never holds a private key or a
//! plaintext credential.
//!
//! ## Data source posture (Chunk 2, C4)
//! After the Phase D identity port, the RELAY is authoritative for
//! membership and grants; local `state.json` is a cache mirror — readable
//! offline, write-through to the relay. Until D lands, `state.json` remains
//! the source of record and every route reads it directly.

pub mod console;
pub mod provisioner;
pub mod state;
pub mod web;

use std::net::IpAddr;

/// C3 (Chunk 2): the console is loopback-only BY DESIGN — the HTTP surface
/// has no authentication (loopback is the authn), so a non-loopback bind is
/// an open admin endpoint. Refuse it outright; remote operator access is an
/// SSH tunnel (`ssh -L 8080:127.0.0.1:8080 <box>`), not a network bind.
/// Accepts `127.*`, `localhost`, `::1` with optional `[v6]:port` brackets.
pub fn validate_loopback_bind(addr: &str) -> Result<(), String> {
    // STRICT: the whole string must be `host:port` with a parseable u16 port —
    // `127.0.0.1:8080; rm -rf /` fails at the port parse and is never
    // interpolated (the address is operator-supplied today, CPA-driven later).
    let (host, port) = addr
        .rsplit_once(':')
        .ok_or_else(|| format!("{addr:?} is not a host:port address"))?;
    port.parse::<u16>()
        .map_err(|_| format!("{addr:?} has a non-numeric or absent port"))?;
    let host = host.trim_start_matches('[').trim_end_matches(']');
    let loopback = host == "localhost"
        || host == "::1"
        || host
            .parse::<IpAddr>()
            .map(|ip| ip.is_loopback())
            .unwrap_or(false);
    if loopback {
        Ok(())
    } else {
        Err(format!(
            "refusing to bind the console to {addr:?}: loopback-only (no authn/TLS on the HTTP \
             surface); reach it from elsewhere with an SSH tunnel \
             (ssh -L 8080:127.0.0.1:8080 <box>)"
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::validate_loopback_bind;

    #[test]
    fn loopback_binds_are_accepted() {
        for addr in [
            "127.0.0.1:8080",
            "127.0.0.2:9000",
            "localhost:8080",
            "[::1]:8080",
            "::1:8080",
        ] {
            assert!(validate_loopback_bind(addr).is_ok(), "{addr} must bind");
        }
    }

    #[test]
    fn non_loopback_binds_are_refused() {
        for addr in [
            "0.0.0.0:8080",
            "192.168.30.224:8080",
            "10.0.0.1:80",
            ":::8080",
            "[::]:8080",
        ] {
            let err = validate_loopback_bind(addr).unwrap_err();
            assert!(
                err.contains("loopback-only") || err.contains("non-numeric"),
                "{addr}: {err}"
            );
        }
    }

    #[test]
    fn injection_tails_are_refused() {
        // a `; <shell>` tail past the port must NEVER pass the guard
        for addr in [
            "127.0.0.1:8080; rm -rf /",
            "127.0.0.1:8080 && id",
            "localhost:8080:extra",
            "127.0.0.1:99999",
            "127.0.0.1:80 80",
        ] {
            assert!(
                validate_loopback_bind(addr).is_err(),
                "{addr} must be refused"
            );
        }
    }

    #[test]
    fn missing_port_is_refused() {
        for addr in ["127.0.0.1", "localhost", ""] {
            assert!(
                validate_loopback_bind(addr).is_err(),
                "{addr:?} must be refused"
            );
        }
    }
}
