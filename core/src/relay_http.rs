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

use crate::nip98::{GRANTS_KIND, RUNNER_PROFILE_KIND, nip98_auth};
use serde_json::Value;
use std::collections::BTreeMap;
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

/// A runner's current lifecycle snapshot (kind 30181, addressable):
/// identity pubkeys, connector kind/address, status, and the secret NAME
/// only — never material. The relay holds no secrets; the profile is the
/// rebuildable projection a respawned CP folds from.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct RunnerProfile {
    pub name: String,
    /// Connector kind (ssh / vultr-vps / hetzner-vps / api / local) + service
    /// address — the runner's single target.
    pub kind: String,
    pub address: String,
    /// "active" | "revoked" — revocation lands as a REPLACE of this record,
    /// never an append.
    pub status: String,
    pub nostr_pubkey: String,
    pub enc_pubkey: String,
    /// Secret NAME only (`CP = secret provisioner`: agents reference secrets
    /// by name; plaintext/ciphertext never rides this event).
    pub secret: String,
    pub created_at: u64,
    pub rotated_at: Option<u64>,
}

/// Parse the content of a stored kind-30181 runner-profile event.
/// Rejects shape drift loudly (a malformed winner fails the query closed,
/// mirroring the grants contract).
pub fn parse_profile_content(content: &str) -> Result<RunnerProfile, String> {
    let v: Value = serde_json::from_str(content).map_err(|e| format!("profile content: {e}"))?;
    let get = |k: &str| -> Result<String, String> {
        v.get(k)
            .and_then(Value::as_str)
            .map(String::from)
            .ok_or_else(|| format!("profile missing string field {k}"))
    };
    let profile = RunnerProfile {
        name: get("name")?,
        kind: get("kind")?,
        address: get("address")?,
        status: get("status")?,
        nostr_pubkey: get("nostr_pubkey")?,
        enc_pubkey: get("enc_pubkey")?,
        secret: get("secret")?,
        created_at: v
            .get("created_at")
            .and_then(Value::as_u64)
            .ok_or_else(|| "profile missing created_at".to_string())?,
        rotated_at: v.get("rotated_at").and_then(Value::as_u64),
    };
    if profile.status != "active" && profile.status != "revoked" {
        return Err(format!("profile bad status {:?}", profile.status));
    }
    if profile.nostr_pubkey.len() != 64 || profile.enc_pubkey.len() != 64 {
        return Err("profile pubkey not 64-hex".to_string());
    }
    if profile.name.is_empty() {
        return Err("profile empty name".to_string());
    }
    Ok(profile)
}

/// Publish (or replace) a runner's lifecycle snapshot (kind 30181,
/// `d`-tag = the runner's nostr pubkey). Same d-tag + newer created_at =
/// replacement: provision, adopt, rotate (rotated_at flip), and revoke
/// (status flip) all re-publish the SAME d-tag — nothing appends history.
pub fn publish_runner_profile(
    relay_url: &str,
    console_secret: &[u8; 32],
    profile: &RunnerProfile,
) -> Result<(), String> {
    let ts = now_secs();
    let content = serde_json::to_string(profile).map_err(|e| e.to_string())?;
    let (pubkey, id, sig) = crate::nip98::sign_event(
        console_secret,
        RUNNER_PROFILE_KIND,
        ts,
        vec![vec!["d".into(), profile.nostr_pubkey.clone()]],
        &content,
    )
    .map_err(|e| e.to_string())?;
    let event = serde_json::json!({
        "id": id,
        "pubkey": pubkey,
        "created_at": ts,
        "kind": RUNNER_PROFILE_KIND,
        "tags": [["d", profile.nostr_pubkey]],
        "content": content,
        "sig": sig,
    });
    publish_event_json(relay_url, console_secret, &event.to_string())
}

/// Fetch the CURRENT lifecycle snapshot of EVERY runner from the relay
/// (kind 30181). Author-gated + locally signature-verified like grants; per
/// runner pubkey the NEWEST trusted event wins (replaceable semantics), so
/// the result is the deterministic projection a respawned CP folds from.
/// Sorted by name for a stable fold.
pub fn query_runner_profiles(
    relay_url: &str,
    expected_author: &str,
    auth_secret: &[u8; 32],
) -> Result<Vec<RunnerProfile>, String> {
    let url = format!("{relay}/query", relay = relay_url.trim_end_matches('/'));
    let filters = serde_json::json!([{
        "kinds": [RUNNER_PROFILE_KIND],
        "limit": 1000,
    }]);
    let headers = nip98_auth(auth_secret, "POST", &url, now_secs()).map_err(|e| e.to_string())?;
    let mut resp = agent()
        .post(&url)
        .header("Authorization", &headers)
        .header("Content-Type", "application/json")
        .send(filters.to_string())
        .map_err(|e| format!("profile query request failed: {e}"))?;
    let body = resp
        .body_mut()
        .read_to_string()
        .map_err(|e| format!("profile query read: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!(
            "profile query returned HTTP {}: {body}",
            resp.status()
        ));
    }
    let events: Vec<Value> =
        serde_json::from_str(&body).map_err(|e| format!("profile query parse: {e}: {body}"))?;
    merge_runner_profiles(&events, expected_author)
}

/// The trust + dedupe core shared by the HTTP query and its tests: keep
/// only events by `expected_author` (the console) whose Schnorr signature
/// verifies locally, then per runner pubkey (d-tag) keep the NEWEST —
/// replaceable semantics, so a revoke REPLACES, never appends. Deterministic
/// (sorted by name) — the fold a respawned CP replays.
fn merge_runner_profiles(
    events: &[Value],
    expected_author: &str,
) -> Result<Vec<RunnerProfile>, String> {
    let mut best: BTreeMap<String, (i64, RunnerProfile)> = BTreeMap::new();
    for ev in events {
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
            RUNNER_PROFILE_KIND,
            &tags,
            content,
            ev["sig"].as_str().unwrap_or(""),
        )
        .is_err()
        {
            tracing::warn!(
                author,
                created_at,
                "runner profile event failed local signature verify — skipped"
            );
            continue;
        }
        // The d-tag = the runner being profiled; a profile without one
        // can't be attributed to a runner (skip, warn).
        let Some(d) = tags
            .iter()
            .find(|t| t.first().is_some_and(|k| k == "d"))
            .and_then(|t| t.get(1))
        else {
            tracing::warn!(author, created_at, "runner profile missing d-tag — skipped");
            continue;
        };
        // Later (or same-second, later-inserted) event wins per runner.
        if best.get(d).is_none_or(|(t, _)| created_at >= *t) {
            let profile = parse_profile_content(content)?;
            best.insert(d.clone(), (created_at, profile));
        }
    }
    Ok(best.into_values().map(|(_, p)| p).collect())
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

/// Shared bridge query primitive: POST /query with a filters ARRAY (the
/// real bridge shape — verified live), NIP-98 auth, status checked, body
/// parsed as an event array. Returns the raw events; callers verify authors
/// and signatures locally.
pub fn query_events(
    relay_url: &str,
    auth_secret: &[u8; 32],
    filters: serde_json::Value,
) -> Result<Vec<serde_json::Value>, String> {
    let url = format!("{relay}/query", relay = relay_url.trim_end_matches('/'));
    let headers = nip98_auth(auth_secret, "POST", &url, now_secs()).map_err(|e| e.to_string())?;
    let mut resp = agent()
        .post(&url)
        .header("Authorization", &headers)
        .header("Content-Type", "application/json")
        .send(filters.to_string())
        .map_err(|e| format!("query request failed: {e}"))?;
    let body = resp
        .body_mut()
        .read_to_string()
        .map_err(|e| format!("query read: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("query returned HTTP {}: {body}", resp.status()));
    }
    serde_json::from_str(&body).map_err(|e| format!("query parse: {e}: {body}"))
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

/// Publish a profile (kind 0 metadata) for an identity so clients render a
/// NAME instead of a bare hex pubkey. Replaceable per author (NIP-01); the
/// roster stays authoritative for membership — this only gives agents a face.
pub fn publish_profile(
    relay_url: &str,
    secret: &[u8; 32],
    name: &str,
    about: &str,
) -> Result<(), String> {
    let ts = now_secs();
    let content =
        serde_json::json!({ "name": name, "about": about, "display_name": name }).to_string();
    let (pubkey, id, sig) =
        crate::nip98::sign_event(secret, 0, ts, vec![], &content).map_err(|e| e.to_string())?;
    let event = serde_json::json!({
        "id": id,
        "pubkey": pubkey,
        "created_at": ts,
        "kind": 0,
        "tags": [],
        "content": content,
        "sig": sig,
    });
    publish_event_json(relay_url, secret, &event.to_string())
}

/// Join a channel (kind 9021, NIP-29 join request): for an OPEN channel the
/// relay auto-accepts and the member lands on the 39002 roster, so clients
/// list them. Agents join the #freehold channel by default after a deploy
/// (the roster is relay-signed; the relay decides, we only request).
pub fn join_channel(relay_url: &str, secret: &[u8; 32], channel_id: &str) -> Result<(), String> {
    let ts = now_secs();
    let (pubkey, id, sig) = crate::nip98::sign_event(
        secret,
        9021,
        ts,
        vec![vec!["h".into(), channel_id.into()]],
        "",
    )
    .map_err(|e| e.to_string())?;
    let event = serde_json::json!({
        "id": id,
        "pubkey": pubkey,
        "created_at": ts,
        "kind": 9021,
        "tags": [["h", channel_id]],
        "content": "",
        "sig": sig,
    });
    publish_event_json(relay_url, secret, &event.to_string())
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

    #[test]
    fn parse_profile_content_accepts_and_rejects() {
        let pk = "a".repeat(64);
        let enc = "b".repeat(64);
        let ok = format!(
            r#"{{"name":"ssh","kind":"ssh","address":"host:22","status":"active","nostr_pubkey":"{pk}","enc_pubkey":"{enc}","secret":"ssh","created_at":5,"rotated_at":null}}"#
        );
        let p = parse_profile_content(&ok).unwrap();
        assert_eq!(p.name, "ssh");
        assert_eq!(p.status, "active");
        assert_eq!(p.rotated_at, None);
        // A rotate flips rotated_at; the parse must survive the Option.
        let rotated = ok.replace("\"rotated_at\":null", "\"rotated_at\":7");
        assert_eq!(parse_profile_content(&rotated).unwrap().rotated_at, Some(7));
        // Bad status, missing field, short pubkey, empty name — all rejected.
        assert!(parse_profile_content(&ok.replace("\"active\"", "\"banana\"")).is_err());
        assert!(parse_profile_content(r#"{"nope":1}"#).is_err());
        assert!(parse_profile_content(&ok.replace(&pk, "short")).is_err());
        assert!(parse_profile_content(&ok.replace("\"name\":\"ssh\"", "\"name\":\"\"")).is_err());
    }

    #[test]
    fn profile_newest_wins_per_runner_and_rogue_author_is_ignored() {
        // Three trusted events for two runners (runner A twice: active then
        // revoked -> REPLACE), plus a rogue-author event for runner A with a
        // NEWER timestamp -> must NOT win.
        let secret = [7u8; 32];
        let (author, _, _) = crate::nip98::sign_event(&secret, 1, 1, vec![], "").unwrap();
        let mk = |p: &RunnerProfile, t: i64, who: &[u8; 32]| -> Value {
            let content = serde_json::to_string(p).unwrap();
            let (pk, id, sig) = crate::nip98::sign_event(
                who,
                RUNNER_PROFILE_KIND,
                t,
                vec![vec!["d".into(), p.nostr_pubkey.clone()]],
                &content,
            )
            .unwrap();
            serde_json::json!({
                "id": id, "pubkey": pk, "created_at": t,
                "kind": RUNNER_PROFILE_KIND,
                "tags": [["d", p.nostr_pubkey]],
                "content": content, "sig": sig,
            })
        };
        let a = RunnerProfile {
            name: "alpha".into(),
            kind: "ssh".into(),
            address: "h1:22".into(),
            status: "active".into(),
            nostr_pubkey: "a".repeat(64),
            enc_pubkey: "e".repeat(64),
            secret: "alpha".into(),
            created_at: 1,
            rotated_at: None,
        };
        // Distinct d-tag (nostr pubkey) — alpha and beta are different runners.
        let b = RunnerProfile {
            name: "beta".into(),
            nostr_pubkey: "b".repeat(64),
            ..a.clone()
        };
        let rogue = RunnerProfile {
            status: "revoked".into(),
            ..a.clone()
        };

        let mut events = vec![
            mk(&a, 10, &secret),
            mk(&b, 10, &secret),
            mk(&rogue, 999, &[1u8; 32]),
        ];
        // Per-runner newest wins: push a REPLACED (revoked) alpha AFTER.
        let revoked = RunnerProfile {
            status: "revoked".into(),
            ..a.clone()
        };
        events.insert(2, mk(&revoked, 11, &secret));

        let out = merge_runner_profiles(&events, &author).unwrap();
        assert_eq!(out.len(), 2);
        let alpha = out.iter().find(|p| p.name == "alpha").unwrap();
        assert_eq!(
            alpha.status, "revoked",
            "newest alpha must win (replace, not append)"
        );
        assert!(out.iter().any(|p| p.name == "beta" && p.status == "active"));
    }
}
