//! NIP-98 HTTP auth + Nostr event signing (Phase D — grants on the relay).
//!
//! NIP-98: an HTTP request authenticates as a Nostr pubkey by sending
//! `Authorization: Nostr <base64(event json)>` where the event is
//! kind 27235 (HTTP auth) with a `u` tag = the request URL, signed by the
//! caller. The buzz relay's HTTP bridge (BUZZ_SURFACE §4) enforces this on
//! `/events`, `/query`, `/count`, etc.
//!
//! Event hashing follows the canonical Nostr serialization: an array
//! `["0", pubkey_hex, created_at, tags, content]` JSON-encoded without
//! whitespace, sha256'd. Signatures are BIP-340 Schnorr over the event id,
//! produced with the crate's existing secp256k1 dep (already the identity's
//! curve — the nostr_secret 32-byte seed is the signing key).

use base64::Engine as _;
use secp256k1::{Keypair, Secp256k1, SecretKey, schnorr};
use std::time::{SystemTime, UNIX_EPOCH};

fn now_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

fn random_bytes() -> [u8; 24] {
    let mut b = [0u8; 24];
    rand::RngCore::fill_bytes(&mut rand::rng(), &mut b);
    b
}
use sha2::{Digest, Sha256};

const B64: base64::engine::GeneralPurpose = base64::engine::general_purpose::STANDARD;

pub const KIND_HTTP_AUTH: u32 = 27235;
pub const GRANTS_KIND: u32 = 30180;

/// pkcs7-style canonical event array, JSON without whitespace.
fn canonical_event_bytes(
    pubkey_hex: &str,
    created_at: i64,
    kind: u32,
    tags: &[Vec<String>],
    content: &str,
) -> Vec<u8> {
    // NIP-01 canonical id: `[0, pubkey, created_at, kind, tags, content]`
    // JSON without whitespace. Built with serde_json EXACTLY like rust-nostr's
    // EventId::new — the string/number literals AND the escaping of quoted
    // values (content `{"a":1}` must appear as `"{\"a\":1}"`) must match the
    // wire byte-for-byte; a hand-rolled builder that skips escaping breaks
    // any event whose content (or tag value) contains a quote.
    serde_json::json!([0, pubkey_hex, created_at, kind, tags, content])
        .to_string()
        .into_bytes()
}

/// sha256 of the canonical event id (Nostr `id` field).
pub fn event_id(
    pubkey_hex: &str,
    created_at: i64,
    kind: u32,
    tags: &[Vec<String>],
    content: &str,
) -> [u8; 32] {
    let mut h = Sha256::new();
    h.update(canonical_event_bytes(
        pubkey_hex, created_at, kind, tags, content,
    ));
    h.finalize().into()
}

/// Sign a Nostr event with the identity's 32-byte secret seed; returns the
/// (pubkey_hex, id_hex, sig_hex) tuple.
pub fn sign_event(
    secret: &[u8; 32],
    kind: u32,
    created_at: i64,
    tags: Vec<Vec<String>>,
    content: &str,
) -> Result<(String, String, String), &'static str> {
    let secp = Secp256k1::new();
    let sk = SecretKey::from_slice(secret).map_err(|_| "invalid secret seed")?;
    let kp = Keypair::from_secret_key(&secp, &sk);
    let (xonly, _parity) = kp.x_only_public_key();
    let pubkey_hex = hex::encode(xonly.serialize());
    let id = event_id(&pubkey_hex, created_at, kind, &tags, content);
    let sig = secp.sign_schnorr_no_aux_rand(&id, &kp);
    Ok((
        pubkey_hex,
        hex::encode(id),
        hex::encode(sig.to_byte_array()),
    ))
}

/// Verify a signed event against its own id/signature; returns the pubkey
/// hex on success.
pub fn verify_event(
    pubkey_hex: &str,
    created_at: i64,
    kind: u32,
    tags: &[Vec<String>],
    content: &str,
    sig_hex: &str,
) -> Result<String, String> {
    let expected_id = event_id(pubkey_hex, created_at, kind, tags, content);
    let sig_bytes: [u8; 64] = hex::decode(sig_hex)
        .map_err(|_| "signature is not hex")?
        .try_into()
        .map_err(|_| "signature is not 64 bytes")?;
    let sig = schnorr::Signature::from_slice(&sig_bytes).map_err(|e| e.to_string())?;
    let xonly = secp256k1::XOnlyPublicKey::from_slice(
        hex::decode(pubkey_hex)
            .map_err(|_| "pubkey is not hex")?
            .as_slice(),
    )
    .map_err(|e| e.to_string())?;
    let secp = Secp256k1::new();
    secp.verify_schnorr(&sig, &expected_id, &xonly)
        .map_err(|e| format!("bad signature: {e}"))?;
    Ok(pubkey_hex.to_string())
}

/// Build the NIP-98 `Authorization: Nostr <b64(event)>` value for a request.
/// The `u` tag carries the full URL (scheme://host:port/path?query) and the
/// `t` tag the HTTP method — buzz's bridge REQUIRES both (the signature binds
/// method + exact URL).
pub fn nip98_auth(
    secret: &[u8; 32],
    method: &str,
    url: &str,
    now_secs: i64,
) -> Result<String, &'static str> {
    // A random `nonce` tag makes rapid same-second publishes distinct: the
    // relay's NIP-98 replay set dedupes by event id, so two auth events in
    // the same second with identical fields would collide ("replay
    // detected", verified live). Extra tags don't affect the u/method/sig
    // checks the relay performs. ONE nonce feeds both the signature and the
    // JSON (id/sig must agree with the emitted fields).
    let nonce = hex::encode(random_bytes());
    let (pubkey, id, sig) = sign_event(
        secret,
        KIND_HTTP_AUTH,
        now_secs,
        vec![
            vec!["u".into(), url.into()],
            vec!["method".into(), method.into()],
            vec!["nonce".into(), nonce.clone()],
        ],
        "",
    )?;
    let event = serde_json::json!({
        "id": id,
        "pubkey": pubkey,
        "created_at": now_secs,
        "kind": KIND_HTTP_AUTH,
        "tags": [
            ["u", url],
            ["method", method],
            ["nonce", nonce],
        ],
        "content": "",
        "sig": sig,
    });
    Ok(format!(
        "Nostr {}",
        B64.encode(event.to_string().as_bytes())
    ))
}

/// Parse + verify a NIP-98 header against the expected URL and method;
/// returns the authentic pubkey hex.
pub fn verify_nip98(
    header: &str,
    expected_method: &str,
    expected_url: &str,
) -> Result<String, String> {
    let b64 = header
        .strip_prefix("Nostr ")
        .ok_or_else(|| "missing Nostr scheme".to_string())?;
    let bytes = B64.decode(b64).map_err(|e| format!("bad base64: {e}"))?;
    let ev: serde_json::Value =
        serde_json::from_slice(&bytes).map_err(|e| format!("bad event json: {e}"))?;
    let pubkey = ev["pubkey"]
        .as_str()
        .ok_or_else(|| "no pubkey".to_string())?;
    let created_at = ev["created_at"]
        .as_i64()
        .ok_or_else(|| "no created_at".to_string())?;
    let kind = ev["kind"].as_u64().ok_or_else(|| "no kind".to_string())? as u32;
    let tags: Vec<Vec<String>> = ev["tags"]
        .as_array()
        .ok_or_else(|| "no tags".to_string())?
        .iter()
        .map(|t| {
            t.as_array()
                .map(|a| {
                    a.iter()
                        .filter_map(|v| v.as_str().map(String::from))
                        .collect()
                })
                .unwrap_or_default()
        })
        .collect();
    let content = ev["content"].as_str().unwrap_or("");
    let sig = ev["sig"].as_str().ok_or_else(|| "no sig".to_string())?;
    if kind != KIND_HTTP_AUTH {
        return Err(format!("wrong kind {kind}"));
    }
    // NIP-98 freshness: the auth event must be within the standard ~60s
    // window — a captured header must not replay forever.
    let now = now_secs();
    if now.abs_diff(created_at) > 60 {
        return Err("auth event timestamp outside the 60s window".to_string());
    }
    let url_ok = tags.iter().any(|t| {
        t.first().is_some_and(|k| k == "u") && t.get(1).map(String::as_str) == Some(expected_url)
    });
    let method_ok = tags.iter().any(|t| {
        t.first().is_some_and(|k| k == "method")
            && t.get(1).map(String::as_str) == Some(expected_method)
    });
    if !url_ok {
        return Err(format!("u tag does not match {expected_url}"));
    }
    if !method_ok {
        return Err(format!("method tag does not match {expected_method}"));
    }
    verify_event(pubkey, created_at, kind, &tags, content, sig)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn seed() -> [u8; 32] {
        [7u8; 32]
    }

    #[test]
    fn sign_verify_roundtrip() {
        let (pubkey, _id, sig) = sign_event(
            &seed(),
            GRANTS_KIND,
            1,
            vec![vec!["d".into(), "abc".into()]],
            "{}",
        )
        .unwrap();
        let ok = verify_event(
            &pubkey,
            1,
            GRANTS_KIND,
            &[vec!["d".into(), "abc".into()]],
            "{}",
            &sig,
        );
        assert_eq!(ok, Ok(pubkey.clone()));
        // tamper with the content -> id changes -> verification fails
        let bad = verify_event(
            &pubkey,
            1,
            GRANTS_KIND,
            &[vec!["d".into(), "abc".into()]],
            "{} ",
            &sig,
        );
        assert!(bad.is_err());
    }

    #[test]
    fn nip98_auth_roundtrip() {
        let now = now_secs();
        let auth = nip98_auth(&seed(), "GET", "http://relay:3000/query", now).unwrap();
        assert!(auth.starts_with("Nostr "));
        let pubkey = verify_nip98(&auth, "GET", "http://relay:3000/query").unwrap();
        assert_eq!(pubkey.len(), 64);
        // bound to the URL: a different URL must fail
        assert!(verify_nip98(&auth, "GET", "http://other:3000/query").is_err());
        // bound to the method: a mismatched method must fail
        assert!(verify_nip98(&auth, "POST", "http://relay:3000/query").is_err());
        // freshness: a stale auth event must not replay
        let stale = nip98_auth(&seed(), "GET", "http://relay:3000/query", now - 120).unwrap();
        assert!(verify_nip98(&stale, "GET", "http://relay:3000/query").is_err());
    }

    #[test]
    fn event_verifies_with_the_nostr_crate_the_relay_uses() {
        // Ground truth wire check: buzz verifies NIP-98 with rust-nostr's
        // Event::verify. Our signed event must pass it byte-for-byte.
        let ts = 5;
        let url = "http://relay:3000/events";
        let (pubkey, id, sig) = sign_event(
            &seed(),
            27235,
            ts,
            vec![
                vec!["u".into(), url.into()],
                vec!["method".into(), "POST".into()],
            ],
            "",
        )
        .unwrap();
        let json = format!(
            r#"{{"id":"{id}","pubkey":"{pubkey}","created_at":{ts},"kind":27235,"tags":[["u","{url}"],["method","POST"]],"content":"","sig":"{sig}"}}"#
        );
        let ev: nostr::Event = serde_json::from_str(&json).expect("nostr parses our event");
        assert_eq!(ev.id.to_hex(), id);
        assert_eq!(ev.pubkey.to_hex(), pubkey);
        eprintln!("OUR JSON:   {json}");
        eprintln!("NOSTR JSON: {}", serde_json::to_string(&ev).unwrap());
        ev.verify()
            .expect("rust-nostr verifies our signature AND id");
    }

    #[test]
    fn grant_event_passes_the_nostr_crate_too() {
        // The AUTH event verifies; the GRANT event must as well — same
        // canonicalization, different shape (addressable d-tag, JSON content).
        let rk = "8b31e8f0aa563344fc148e5608c73b6415605aa03eeded3275d25715872a30d8";
        let content = r#"{"grants":["1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"],"schema":1}"#;
        let ts = 5;
        let (pubkey, id, sig) = sign_event(
            &seed(),
            30180,
            ts,
            vec![vec!["d".into(), rk.into()]],
            content,
        )
        .unwrap();
        // EXACTLY how publish_grants builds it: serde_json escapes the JSON
        // content inside the content STRING.
        let json = serde_json::json!({
            "id": id,
            "pubkey": pubkey,
            "created_at": ts,
            "kind": 30180,
            "tags": [["d", rk]],
            "content": content,
            "sig": sig,
        })
        .to_string();
        let ev: nostr::Event = serde_json::from_str(&json).expect("nostr parses grant event");
        assert_eq!(ev.id.to_hex(), id, "nostr recomputes the same id");
        ev.verify()
            .expect("rust-nostr verifies our grant event id + sig");
    }

    #[test]
    fn event_id_is_stable() {
        let a = event_id("aa", 5, 30180, &[vec!["d".into(), "x".into()]], "");
        let b = event_id("aa", 5, 30180, &[vec!["d".into(), "x".into()]], "");
        assert_eq!(a, b);
        // tags must change the id — canonical bytes hand-rolled
        let c = event_id("aa", 5, 30180, &[vec!["d".into(), "x y".into()]], "");
        assert_ne!(a, c);
    }
}
