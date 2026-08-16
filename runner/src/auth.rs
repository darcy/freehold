//! Caller authentication (Phase D — coarse grants).
//!
//! Every privileged MCP call must be signed by a GRANTED agent pubkey
//! (whitelist shipped in the SecretPackage). Signatures are BIP-340
//! (`core::audit`) over the canonical string `{unix_ts}|{raw_request_body}` —
//! the RAW body as received, so verification is byte-exact and never depends
//! on JSON-re-serialization order. A 60-second window bounds replays.
//!
//! The runner FAILS CLOSED: no grants shipped (or an unreadable package)
//! means nobody may call.

use freehold_core::audit::{self, SignedEvent};
use thiserror::Error;

pub const TS_WINDOW_SECS: i64 = 60;
pub const PUBKEY_HEADER: &str = "x-freehold-pubkey";
pub const SIG_HEADER: &str = "x-freehold-sig";
pub const TS_HEADER: &str = "x-freehold-ts";

#[derive(Debug, Error, PartialEq, Eq)]
pub enum AuthError {
    #[error("missing auth headers (x-freehold-pubkey/sig/ts)")]
    MissingHeaders,
    #[error("signature is not valid hex: {0}")]
    BadSignatureFormat(String),
    #[error("timestamp is not a valid integer: {0}")]
    BadTimestamp(String),
    #[error("request is stale (timestamp outside the {TS_WINDOW_SECS}s window)")]
    Stale,
    #[error("pubkey {0} is not granted to call this runner")]
    NotGranted(String),
    #[error("signature does not verify for this request")]
    BadSignature,
}

/// Sign an outgoing request body (client side). The signer holds the agent
/// private key; only the pubkey travels in headers.
pub fn sign_body(agent_secret: &[u8; 32], ts: i64, raw_body: &str) -> SignedEvent {
    let content = signed_content(ts, raw_body);
    audit::sign_event(agent_secret, &content).expect("audit signing is infallible")
}

fn signed_content(ts: i64, raw_body: &str) -> String {
    format!("{ts}|{raw_body}")
}

/// Verify a request: pubkey granted, signature valid over `ts|raw_body`,
/// timestamp within the window. Returns the agent pubkey on success (the
/// audit record's caller).
pub fn verify_body(
    grants: &[String],
    pubkey_header: Option<&str>,
    sig_header: Option<&str>,
    ts_header: Option<&str>,
    raw_body: &str,
) -> Result<String, AuthError> {
    let (Some(pubkey), Some(sig), Some(ts)) = (pubkey_header, sig_header, ts_header) else {
        return Err(AuthError::MissingHeaders);
    };
    let ts: i64 = ts
        .parse()
        .map_err(|_| AuthError::BadTimestamp(ts.to_string()))?;
    let now = now_secs();
    if (now - ts).abs() > TS_WINDOW_SECS {
        return Err(AuthError::Stale);
    }
    if !grants.iter().any(|g| g == pubkey) {
        return Err(AuthError::NotGranted(pubkey.to_string()));
    }
    let event = SignedEvent {
        pubkey: pubkey.to_string(),
        content: signed_content(ts, raw_body),
        sig: sig.to_string(),
    };
    audit::verify_event(&event).map_err(|_| AuthError::BadSignature)?;
    Ok(pubkey.to_string())
}

pub fn now_secs() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use rand::RngCore;

    fn agent() -> [u8; 32] {
        let mut b = [0u8; 32];
        rand::rng().fill_bytes(&mut b);
        b
    }

    fn pubkey_of(secret: &[u8; 32]) -> String {
        audit::sign_event(secret, "probe").unwrap().pubkey
    }

    #[test]
    fn signed_request_verifies() {
        let secret = agent();
        let grants = vec![pubkey_of(&secret)];
        let body = r#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exec","arguments":{}}}"#;
        let ev = sign_body(&secret, now_secs(), body);
        assert_eq!(
            verify_body(
                &grants,
                Some(&ev.pubkey),
                Some(&ev.sig),
                Some(&now_secs().to_string()),
                body
            )
            .unwrap(),
            ev.pubkey
        );
    }

    #[test]
    fn unsigned_is_rejected() {
        let secret = agent();
        let grants = vec![pubkey_of(&secret)];
        let err = verify_body(&grants, None, None, None, "body").unwrap_err();
        assert_eq!(err, AuthError::MissingHeaders);
    }

    #[test]
    fn ungranted_pubkey_is_rejected() {
        let secret = agent();
        let other = agent();
        let grants = vec![pubkey_of(&other)];
        let body = "{\"method\":\"tools/call\"}";
        let ev = sign_body(&secret, now_secs(), body);
        assert!(matches!(
            verify_body(
                &grants,
                Some(&ev.pubkey),
                Some(&ev.sig),
                Some(&now_secs().to_string()),
                body
            ),
            Err(AuthError::NotGranted(_))
        ));
    }

    #[test]
    fn stale_request_is_rejected() {
        let secret = agent();
        let grants = vec![pubkey_of(&secret)];
        let body = "{\"method\":\"tools/call\"}";
        let ev = sign_body(&secret, now_secs() - 120, body);
        assert_eq!(
            verify_body(
                &grants,
                Some(&ev.pubkey),
                Some(&ev.sig),
                Some(&(now_secs() - 120).to_string()),
                body
            ),
            Err(AuthError::Stale)
        );
    }

    #[test]
    fn tampered_body_is_rejected() {
        let secret = agent();
        let grants = vec![pubkey_of(&secret)];
        let ts = now_secs();
        let ev = sign_body(&secret, ts, "{\"method\":\"tools/call\",\"a\":1}");
        assert_eq!(
            verify_body(
                &grants,
                Some(&ev.pubkey),
                Some(&ev.sig),
                Some(&ts.to_string()),
                "{\"method\":\"tools/call\",\"a\":2}"
            ),
            Err(AuthError::BadSignature)
        );
    }
}
