//! Caller authentication (Phase D — coarse grants). Shared protocol code:
//! the RUNNER verifies, and every client (the operator CLI, the CP console)
//! signs with the same primitives.
//!
//! Every privileged MCP call must be signed by a GRANTED agent pubkey
//! (whitelist shipped in the SecretPackage). Signatures are BIP-340
//! (`core::audit`) over the canonical string `{unix_ts}|{raw_request_body}` —
//! the RAW body as received, so verification is byte-exact and never depends
//! on JSON-re-serialization order. A 60-second window bounds replays.
//!
//! The runner FAILS CLOSED: no grants shipped (or an unreadable package)
//! means nobody may call.

use crate::audit::{self, SignedEvent};
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

/// Sign an outgoing request body (client side). `runner_pubkey` names the
/// target runner (the AUDIENCE): a signature produced for runner X fails
/// against runner Y, closing cross-runner replay for agents granted on
/// several runners. The signer holds the agent private key; only the pubkey
/// travels in headers.
pub fn sign_body(
    agent_secret: &[u8; 32],
    runner_pubkey: &str,
    ts: i64,
    raw_body: &str,
) -> SignedEvent {
    let content = signed_content(runner_pubkey, ts, raw_body);
    audit::sign_event(agent_secret, &content).expect("audit signing is infallible")
}

fn signed_content(runner_pubkey: &str, ts: i64, raw_body: &str) -> String {
    format!("{runner_pubkey}|{ts}|{raw_body}")
}

/// Verify a request: pubkey granted, signature valid over
/// `runner_pubkey|ts|raw_body`, timestamp within the window. Returns the
/// agent pubkey on success (the audit record's caller).
pub fn verify_body(
    grants: &[String],
    runner_pubkey: &str,
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
        content: signed_content(runner_pubkey, ts, raw_body),
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
        let runner = pubkey_of(&agent());
        let grants = vec![pubkey_of(&secret)];
        let body = r#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exec","arguments":{}}}"#;
        let ts = now_secs();
        let ev = sign_body(&secret, &runner, ts, body);
        let got = verify_body(
            &grants,
            &runner,
            Some(&ev.pubkey),
            Some(&ev.sig),
            Some(&ts.to_string()),
            body,
        )
        .unwrap();
        assert_eq!(got, ev.pubkey);
    }

    #[test]
    fn unsigned_is_rejected() {
        let secret = agent();
        let runner = pubkey_of(&agent());
        let grants = vec![pubkey_of(&secret)];
        let err = verify_body(&grants, &runner, None, None, None, "body").unwrap_err();
        assert_eq!(err, AuthError::MissingHeaders);
    }

    #[test]
    fn ungranted_pubkey_is_rejected() {
        let secret = agent();
        let other = agent();
        let runner = pubkey_of(&agent());
        let grants = vec![pubkey_of(&other)];
        let body = r#"{"method":"tools/call"}"#;
        let ts = now_secs();
        let ev = sign_body(&secret, &runner, ts, body);
        assert!(matches!(
            verify_body(
                &grants,
                &runner,
                Some(&ev.pubkey),
                Some(&ev.sig),
                Some(&ts.to_string()),
                body
            ),
            Err(AuthError::NotGranted(_))
        ));
    }

    #[test]
    fn stale_request_is_rejected() {
        let secret = agent();
        let runner = pubkey_of(&agent());
        let grants = vec![pubkey_of(&secret)];
        let body = r#"{"method":"tools/call"}"#;
        let ts = now_secs() - 120;
        let ev = sign_body(&secret, &runner, ts, body);
        assert_eq!(
            verify_body(
                &grants,
                &runner,
                Some(&ev.pubkey),
                Some(&ev.sig),
                Some(&ts.to_string()),
                body
            ),
            Err(AuthError::Stale)
        );
    }

    #[test]
    fn tampered_body_is_rejected() {
        let secret = agent();
        let runner = pubkey_of(&agent());
        let grants = vec![pubkey_of(&secret)];
        let ts = now_secs();
        let ev = sign_body(&secret, &runner, ts, r#"{"method":"tools/call","a":1}"#);
        assert_eq!(
            verify_body(
                &grants,
                &runner,
                Some(&ev.pubkey),
                Some(&ev.sig),
                Some(&ts.to_string()),
                r#"{"method":"tools/call","a":2}"#
            ),
            Err(AuthError::BadSignature)
        );
    }

    #[test]
    fn wrong_audience_is_rejected() {
        let secret = agent();
        let grants = vec![pubkey_of(&secret)];
        let runner_a = pubkey_of(&agent());
        let runner_b = pubkey_of(&agent());
        let body = r#"{"method":"tools/call"}"#;
        let ts = now_secs();
        // A signature produced FOR runner A must fail against runner B.
        let ev = sign_body(&secret, &runner_a, ts, body);
        assert_eq!(
            verify_body(
                &grants,
                &runner_b,
                Some(&ev.pubkey),
                Some(&ev.sig),
                Some(&ts.to_string()),
                body
            ),
            Err(AuthError::BadSignature)
        );
    }
}
