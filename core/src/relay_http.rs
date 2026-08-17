//! Relay HTTP bridge access (Phase D — grants on the relay).
//!
//! The buzz HTTP bridge (BUZZ_SURFACE §4) requires NIP-98 auth on `/events`
//! and `/query`. This module owns the two operations the grant port needs:
//! reading a runner's CURRENT grant list (kind 30180, addressable,
//! `d`-tag = runner pubkey — a new event with the same d-tag REPLACES the
//! list, so revocation never appends history) and publishing it.
//!
//! Fail-closed contract: any query failure is an Err; callers (the runner)
//! treat it as "no grants" — the relay is authoritative once configured.
//!
//! The kind lives in the addressable range (30000–39999, NIP-16 replaceable)
//! per BUZZ_SURFACE §5/§9: grants are CURRENT STATE, so a revoke must
//! REPLACE, not append (an append-only kind would fail-stale on revoke).

use crate::nip98::{GRANTS_KIND, nip98_auth};
use serde_json::Value;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

/// Bounded, status-as-response agent: 4xx/5xx come back as responses (so the
/// relay's rejection REASON can be surfaced) and every request is time-bounded
/// (a wedged relay must fail closed, never hang the runner forever).
fn agent() -> ureq::Agent {
    ureq::Agent::config_builder()
        .http_status_as_error(false)
        .timeout_global(Some(Duration::from_secs(30)))
        .build()
        .into()
}

fn now_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Parse the content of a stored kind-30180 grant event:
/// `{"grants": ["<agent hex>", ...], "schema": 1}`.
pub fn parse_grants_content(content: &str) -> Result<Vec<String>, String> {
    let v: Value = serde_json::from_str(content).map_err(|e| format!("grant content: {e}"))?;
    let grants = v
        .get("grants")
        .and_then(Value::as_array)
        .ok_or_else(|| "grant event content has no grants array".to_string())?;
    grants
        .iter()
        .map(|g| {
            let s = g
                .as_str()
                .ok_or_else(|| "grant entry is not a string".to_string())?;
            if s.len() != 64 || !s.chars().all(|c| c.is_ascii_hexdigit()) {
                return Err(format!("grant entry is not 64-hex: {s:?}"));
            }
            Ok(s.to_string())
        })
        .collect()
}

/// Fetch the CURRENT grant list for a runner from the relay.
/// NIP-98 auth (signer = the caller's secret), filter kind 30180 / max
/// created_at, then match the `d`-tag to `runner_pubkey_hex`.
pub fn query_grants(
    relay_url: &str,
    runner_pubkey_hex: &str,
    auth_secret: &[u8; 32],
) -> Result<Vec<String>, String> {
    let url = format!(
        "{relay}/query?kinds={kind}&limit=100",
        relay = relay_url.trim_end_matches('/'),
        kind = GRANTS_KIND,
    );
    let headers = nip98_auth(auth_secret, "GET", &url, now_secs()).map_err(|e| e.to_string())?;
    let mut resp = agent()
        .get(&url)
        .header("Authorization", &headers)
        .call()
        .map_err(|e| format!("grant query request failed: {e}"))?;
    let body = resp
        .body_mut()
        .read_to_string()
        .map_err(|e| format!("grant query read: {e}"))?;
    let events: Vec<Value> =
        serde_json::from_str(&body).map_err(|e| format!("grant query parse: {e}: {body}"))?;
    let mut best: Option<(i64, Vec<String>)> = None;
    for ev in events {
        let d_tags: Vec<&str> = ev["tags"]
            .as_array()
            .map(|tags| {
                tags.iter()
                    .filter(|t| {
                        t.as_array()
                            .is_some_and(|t| t.first().is_some_and(|k| k == "d"))
                    })
                    .filter_map(|t| t.as_array().and_then(|t| t.get(1)).and_then(Value::as_str))
                    .collect()
            })
            .unwrap_or_default();
        if !d_tags.contains(&runner_pubkey_hex) {
            continue;
        }
        let created_at = ev["created_at"].as_i64().unwrap_or(0);
        let content = ev["content"].as_str().unwrap_or("");
        let grants = parse_grants_content(content)?;
        // Later event wins, INCLUDING on a created_at tie: the bridge returns
        // events in insertion order, and a replace-publish in the same second
        // otherwise wouldn't supersede the list it replaces (revokes would
        // silently not land).
        if best.as_ref().is_none_or(|(t, _)| created_at >= *t) {
            best = Some((created_at, grants));
        }
    }
    Ok(best.map(|(_, g)| g).unwrap_or_default())
}

/// Publish (or replace) the runner's grant list on the relay as a
/// kind-30180 event with `d`-tag = runner pubkey. Same d-tag + newer
/// created_at = replacement (revocation shrinks the list; it never appends).
pub fn publish_grants(
    relay_url: &str,
    console_secret: &[u8; 32],
    runner_pubkey_hex: &str,
    grants: &[String],
) -> Result<(), String> {
    let url = format!("{relay}/events", relay = relay_url.trim_end_matches('/'));
    let auth = nip98_auth(console_secret, "POST", &url, now_secs()).map_err(|e| e.to_string())?;
    let ts = now_secs();
    let content = serde_json::json!({ "grants": grants, "schema": 1 }).to_string();
    let (pubkey, id, sig) = crate::nip98::sign_event(
        console_secret,
        GRANTS_KIND,
        ts,
        vec![vec!["d".into(), runner_pubkey_hex.into()]],
        &content,
    )
    .map_err(|e| e.to_string())?;
    let event = serde_json::json!({
        "id": id,
        "pubkey": pubkey,
        "created_at": ts,
        "kind": GRANTS_KIND,
        "tags": [["d", runner_pubkey_hex]],
        "content": content,
        "sig": sig,
    });
    let mut resp = agent()
        .post(&url)
        .header("Authorization", &auth)
        .header("Content-Type", "application/json")
        .send(event.to_string())
        .map_err(|e| format!("grant publish request failed: {e}"))?;
    if !resp.status().is_success() {
        let body = resp
            .body_mut()
            .read_to_string()
            .unwrap_or_else(|_| "(unreadable body)".into());
        return Err(format!(
            "grant publish returned HTTP {}: {body}",
            resp.status()
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_grants_content_accepts_and_rejects() {
        assert_eq!(
            parse_grants_content(&format!(
                r#"{{"grants":["{}"],"schema":1}}"#,
                "a".repeat(64)
            ))
            .unwrap(),
            vec!["a".repeat(64)]
        );
        assert!(parse_grants_content(r#"{"grants":[42]}"#).is_err());
        assert!(parse_grants_content(r#"{"nope":1}"#).is_err());
        assert!(parse_grants_content(r#"{"grants":["short"]}"#).is_err());
    }
}
