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

/// Deterministic 64-hex engram address for (agent pubkey, memory key) —
/// buzz requires 64 lowercase hex chars for engram d-tags (verified live).
fn memory_d_tag(agent_pk: &str, key: &str) -> String {
    use sha2::{Digest as _, Sha256};
    let mut h = Sha256::new();
    h.update(agent_pk.as_bytes());
    h.update(b"#");
    h.update(key.as_bytes());
    hex::encode(h.finalize())
}

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
    expected_author: &str,
    auth_secret: &[u8; 32],
) -> Result<Vec<String>, String> {
    // Wire: POST /query with a NIP-01 filter BODY (kinds/#d/limit) — the
    // buzz bridge is POST-only (GET /query is 405). The #d filter means the
    // runner's own events can't fall off a 100-row page of unrelated kinds.
    let url = format!("{relay}/query", relay = relay_url.trim_end_matches('/'));
    // Buzz /query takes an ARRAY of NIP-01 filters (raw_filters: Vec<Value>).
    let filters = serde_json::json!([{
        "kinds": [GRANTS_KIND],
        "#d": [runner_pubkey_hex],
        "limit": 100,
    }]);
    let headers = nip98_auth(auth_secret, "POST", &url, now_secs()).map_err(|e| e.to_string())?;
    let mut resp = agent()
        .post(&url)
        .header("Authorization", &headers)
        .header("Content-Type", "application/json")
        .send(filters.to_string())
        .map_err(|e| format!("grant query request failed: {e}"))?;
    let body = resp
        .body_mut()
        .read_to_string()
        .map_err(|e| format!("grant query read: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!(
            "grant query returned HTTP {}: {body}",
            resp.status()
        ));
    }
    let events: Vec<Value> =
        serde_json::from_str(&body).map_err(|e| format!("grant query parse: {e}: {body}"))?;

    // Trust: only events by `expected_author` (the console/owner, D1's named
    // grant publisher) whose Schnorr signature verifies LOCALLY are
    // candidates. Addressable events are keyed (kind, author, d-tag) — a
    // rogue member could otherwise publish {"grants":[self]} with a newer
    // created_at and take over exec; the author gate is the trust anchor.
    // Pick the NEWEST trusted candidate, then parse ONLY the winner — a
    // malformed NON-winner warns and skips (it must never lock the runner
    // out); a malformed winner fails closed (only the trusted author could
    // have written it).
    let mut best: Option<(i64, &Value)> = None;
    for ev in events.iter() {
        let author = ev["pubkey"].as_str().unwrap_or("");
        if author != expected_author {
            continue;
        }
        let created_at = ev["created_at"].as_i64().unwrap_or(0);
        let tags: Vec<Vec<String>> = ev["tags"]
            .as_array()
            .map(|t| {
                t.iter()
                    .map(|a| {
                        a.as_array()
                            .map(|x| {
                                x.iter()
                                    .filter_map(Value::as_str)
                                    .map(String::from)
                                    .collect()
                            })
                            .unwrap_or_default()
                    })
                    .collect()
            })
            .unwrap_or_default();
        let content = ev["content"].as_str().unwrap_or("");
        if crate::nip98::verify_event(
            author,
            created_at,
            GRANTS_KIND,
            &tags,
            content,
            ev["sig"].as_str().unwrap_or(""),
        )
        .is_err()
        {
            tracing::warn!(
                author,
                created_at,
                "grant event failed local signature verify — skipped"
            );
            continue;
        }
        // Later event wins, INCLUDING on a created_at tie: the bridge returns
        // events in insertion order, and a replace-publish in the same second
        // otherwise wouldn't supersede the list it replaces (revokes would
        // silently not land).
        if best.is_none_or(|(t, _)| created_at >= t) {
            best = Some((created_at, ev));
        }
    }
    match best {
        Some((_, winner)) => parse_grants_content(winner["content"].as_str().unwrap_or("")),
        None => Ok(Vec::new()),
    }
}

/// Publish an ALREADY-SIGNED Nostr event JSON to the bridge (POST /events,
/// NIP-98 auth as `auth_secret`). The event bytes are exactly what the
/// sender spooled — the relay copy and the local copy are the SAME event
/// (kind 48001 audit rows travel this way).
pub fn publish_event_json(
    relay_url: &str,
    auth_secret: &[u8; 32],
    event_json: &str,
) -> Result<(), String> {
    let url = format!("{relay}/events", relay = relay_url.trim_end_matches('/'));
    let auth = nip98_auth(auth_secret, "POST", &url, now_secs()).map_err(|e| e.to_string())?;
    let mut resp = agent()
        .post(&url)
        .header("Authorization", &auth)
        .header("Content-Type", "application/json")
        .send(event_json.to_string())
        .map_err(|e| format!("event publish request failed: {e}"))?;
    if !resp.status().is_success() {
        let body = resp
            .body_mut()
            .read_to_string()
            .unwrap_or_else(|_| "(unreadable body)".into());
        return Err(format!(
            "event publish returned HTTP {}: {body}",
            resp.status()
        ));
    }
    Ok(())
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

/// Phase D3: write (replace) one encrypted memory engram for an agent.
/// Kind 30174 (NATIVE engram — accepted by the stock buzz ingest, no patch
/// gate), `d`-tag = `<agent pubkey>#<key>` (per-agent + per-key current
/// value; replaceable — a re-write supersedes, nothing appends). Content is
/// the sealed envelope; the relay only ever sees ciphertext.
pub fn write_memory(
    relay_url: &str,
    agent_nostr_secret: &[u8; 32],
    key: &str,
    value: &str,
) -> Result<(), String> {
    let (pk, _id, _sig) = crate::nip98::sign_event(agent_nostr_secret, 1, now_secs(), vec![], "")
        .map_err(|e| e.to_string())?;
    let content =
        crate::memory::seal_memory(agent_nostr_secret, value).map_err(|e| e.to_string())?;
    // Buzz's engram validation: d-tag MUST be 64 lowercase hex (the memory
    // address) and exactly one `p` tag (the owner counterparty) — both
    // verified live. The address is derived from (agent, key) so per-agent +
    // per-key current values stay replaceable and unique.
    let d_tag = memory_d_tag(&pk, key);
    // ONE created_at for the signature AND the event JSON — a boundary-
    // crossed timestamp makes id/sig disagree with the fields (intermittent
    // 400 or a read-side verify drop).
    let ts = now_secs();
    let (pubkey, id, sig) = crate::nip98::sign_event(
        agent_nostr_secret,
        crate::memory::MEMORY_KIND,
        ts,
        vec![
            vec!["d".into(), d_tag.clone()],
            vec!["p".into(), pk.clone()],
        ],
        &content,
    )
    .map_err(|e| e.to_string())?;
    let event = serde_json::json!({
        "id": id,
        "pubkey": pubkey,
        "created_at": ts,
        "kind": crate::memory::MEMORY_KIND,
        "tags": [["d", d_tag], ["p", pk]],
        "content": content,
        "sig": sig,
    });
    publish_event_json(relay_url, agent_nostr_secret, &event.to_string())
}

/// Read the agent's OWN memory value for `key` (kind-30174, newest wins).
/// The author is verified locally; the d-tag is agent-scoped so no other
/// member's event can collide, and the payload decrypts only with the
/// agent's enc key.
pub fn read_memory(
    relay_url: &str,
    agent_nostr_secret: &[u8; 32],
    key: &str,
) -> Result<Option<String>, String> {
    let url = format!("{relay}/query", relay = relay_url.trim_end_matches('/'));
    let (pk, _id, _sig) = crate::nip98::sign_event(agent_nostr_secret, 1, now_secs(), vec![], "")
        .map_err(|e| e.to_string())?;
    let d_tag = memory_d_tag(&pk, key);
    let filters = serde_json::json!([{
        "kinds": [crate::memory::MEMORY_KIND],
        "#d": [d_tag],
        "authors": [pk],
        "limit": 20,
    }]);
    let headers =
        nip98_auth(agent_nostr_secret, "POST", &url, now_secs()).map_err(|e| e.to_string())?;
    let mut resp = agent()
        .post(&url)
        .header("Authorization", &headers)
        .header("Content-Type", "application/json")
        .send(filters.to_string())
        .map_err(|e| format!("memory query request failed: {e}"))?;
    let body = resp
        .body_mut()
        .read_to_string()
        .map_err(|e| format!("memory query read: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!(
            "memory query returned HTTP {}: {body}",
            resp.status()
        ));
    }
    let events: Vec<Value> =
        serde_json::from_str(&body).map_err(|e| format!("memory query parse: {e}: {body}"))?;
    let mut best: Option<(i64, String)> = None;
    for ev in events.iter() {
        let author = ev["pubkey"].as_str().unwrap_or("");
        if author != pk {
            continue;
        }
        let created_at = ev["created_at"].as_i64().unwrap_or(0);
        // Local signature verify (id + BIP-340) over the event fields.
        let tags: Vec<Vec<String>> = ev["tags"]
            .as_array()
            .map(|t| {
                t.iter()
                    .map(|a| {
                        a.as_array()
                            .map(|x| {
                                x.iter()
                                    .filter_map(Value::as_str)
                                    .map(String::from)
                                    .collect()
                            })
                            .unwrap_or_default()
                    })
                    .collect()
            })
            .unwrap_or_default();
        let content = ev["content"].as_str().unwrap_or("");
        if crate::nip98::verify_event(
            author,
            created_at,
            crate::memory::MEMORY_KIND,
            &tags,
            content,
            ev["sig"].as_str().unwrap_or(""),
        )
        .is_err()
        {
            continue;
        }
        if best.as_ref().is_none_or(|(t, _)| created_at >= *t) {
            best = Some((created_at, content.to_string()));
        }
    }
    match best {
        Some((_, sealed)) => crate::memory::open_memory(agent_nostr_secret, &sealed)
            .map(Some)
            .map_err(|e| e.to_string()),
        None => Ok(None),
    }
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
