//! Relay HTTP bridge access (Chunk 2.6.1 — runners as NIP-29 channels).
//!
//! The buzz HTTP bridge (BUZZ_SURFACE §4) requires NIP-98 auth on `/events`
//! and `/query`. The runner lifecycle + grants live on NATIVE NIP-29
//! machinery (no custom kind, no ingest patch — G-1 resolved):
//!
//! - A runner IS a private channel: `h` = sha256(runner nostr pubkey)
//!   (deterministic — the runner self-computes its channel id).
//! - The CP (console) creates the channel (kind 9007) and is the owner;
//!   it adds the runner + granted agents as members (kind 9000 put-user,
//!   kind 9001 remove-user for revoke). The relay EXECUTES membership and
//!   re-publishes the RELAY-SIGNED roster (kind 39002, p-tags = members).
//! - The runner's whitelist = its own roster: one 39002 query, verified
//!   locally against the relay pubkey (the roster's trust anchor).
//! - The runner's profile (kind/address/status/secret NAME) rides a kind-9
//!   CHANNEL MESSAGE marked `t`=fh-profile (live-verified: kind 39000 is
//!   NOT in the stock buzz ingest scope) — the fold a respawned CP replays
//!   ("disposable CP"), newest-per-channel.
//!
//! Fail-closed contract: any query failure is an Err; callers (the runner)
//! treat it as "no grants" — the relay is authoritative once configured.

use crate::nip98::{
    CHANNEL_CREATE_KIND, GROUP_MEMBERS_KIND, PUT_USER_KIND, REMOVE_USER_KIND, nip98_auth,
};
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

/// A runner's current lifecycle snapshot — carried as the channel's group
/// METADATA (kind 39000, replaceable per (author, h)): identity pubkeys,
/// connector kind/address, status, and the secret NAME only — never
/// material. The relay holds no secrets; the meta is the rebuildable
/// projection a respawned CP folds from.
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
    /// safe | risky-install | risky-host — the runner class (POC_CHUNK3).
    /// Absent on profiles published before the field existed = unknown.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub risk: Option<String>,
}

/// Parse the content of a stored kind-39000 runner-metadata event.
/// Rejects shape drift loudly (a malformed winner fails the query closed —
/// only the trusted console author could have written it).
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
        risk: v.get("risk").and_then(Value::as_str).map(String::from),
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

/// Deterministic channel id for a runner: sha256(runner nostr pubkey),
/// TRUNCATED to 16 bytes = 32 lowercase hex — a UUID-parseable value.
/// Pure function of the pubkey: the runner self-computes its channel id for
/// the roster query (its whitelist) with no CP round-trip, and a rebuilt CP
/// re-derives it for every folded record.
///
/// LIVE-VERIFIED (rebuild 2026-08-20): buzz's `extract_channel_id` parses
/// the `h` tag as a `uuid::Uuid` (`Option<Uuid>`) — a 64-hex sha256 is
/// None → `invalid: channel-scoped events must include an h tag` on 9000.
/// A 32-hex value parses; `create_channel_with_id` then honors the
/// CLIENT-SUGGESTED channel id (duplicate → idempotent accept:false), so
/// channels stay deterministic per runner pubkey (128-bit collision space).
pub fn runner_channel_id(runner_nostr_pubkey: &str) -> String {
    use sha2::{Digest as _, Sha256};
    let mut h = Sha256::new();
    h.update(runner_nostr_pubkey.as_bytes());
    let hexs = hex::encode(&h.finalize()[..16]);
    format!(
        "{}-{}-{}-{}-{}",
        &hexs[0..8],
        &hexs[8..12],
        &hexs[12..16],
        &hexs[16..20],
        &hexs[20..32]
    )
}

/// Create (or re-assert) the runner's private channel (kind 9007, NIP-29
/// v2): tags `h` + `name` + `visibility=private` — the exact shape the live
/// delegation flow proved (BUZZ_SURFACE §9.7, minus the `open` visibility).
/// The creator (the CP console) is the owner and auto-member of every
/// runner channel it makes. Idempotent: the relay treats a re-create of an
/// existing `h` as a no-op (membership unchanged), so re-provision +
/// rebuild converge.
pub fn create_runner_channel(
    relay_url: &str,
    console_secret: &[u8; 32],
    runner_nostr_pubkey: &str,
    name: &str,
) -> Result<(), String> {
    let h = runner_channel_id(runner_nostr_pubkey);
    let ts = now_secs();
    let (pubkey, id, sig) = crate::nip98::sign_event(
        console_secret,
        CHANNEL_CREATE_KIND,
        ts,
        vec![
            vec!["h".into(), h.clone()],
            vec!["name".into(), format!("#runner-{name}")],
            vec!["visibility".into(), "private".into()],
        ],
        "",
    )
    .map_err(|e| e.to_string())?;
    let event = serde_json::json!({
        "id": id,
        "pubkey": pubkey,
        "created_at": ts,
        "kind": CHANNEL_CREATE_KIND,
        "tags": [["h", h], ["name", format!("#runner-{name}")], ["visibility", "private"]],
        "content": "",
        "sig": sig,
    });
    publish_event_json(relay_url, console_secret, &event.to_string())
}

/// Add a member to a runner channel (kind 9000 put-user, NIP-29): the
/// granted AGENT (or the runner itself at provision). Idempotent — adding
/// an existing member is a no-op. The relay owns membership (the CP cannot
/// self-author a membership write; it issues the command and the relay
/// executes + re-publishes the relay-signed roster).
pub fn put_user(
    relay_url: &str,
    console_secret: &[u8; 32],
    runner_nostr_pubkey: &str,
    member_pubkey: &str,
) -> Result<(), String> {
    membership_command(
        relay_url,
        console_secret,
        PUT_USER_KIND,
        runner_nostr_pubkey,
        member_pubkey,
    )
}

/// Remove a member from a runner channel (kind 9001 remove-user):
/// grant revocation, and the cut-off (removing the RUNNER itself leaves it
/// unable to read its roster — fail-closed deny).
pub fn remove_user(
    relay_url: &str,
    console_secret: &[u8; 32],
    runner_nostr_pubkey: &str,
    member_pubkey: &str,
) -> Result<(), String> {
    membership_command(
        relay_url,
        console_secret,
        REMOVE_USER_KIND,
        runner_nostr_pubkey,
        member_pubkey,
    )
}

/// Shared 9000/9001 command publish: `h` = the runner's channel, `p` = the
/// member being added/removed. The relay verifies the caller is the channel
/// owner, applies the change, and re-publishes the roster.
fn membership_command(
    relay_url: &str,
    console_secret: &[u8; 32],
    kind: u32,
    runner_nostr_pubkey: &str,
    member_pubkey: &str,
) -> Result<(), String> {
    let h = runner_channel_id(runner_nostr_pubkey);
    let ts = now_secs();
    let (pubkey, id, sig) = crate::nip98::sign_event(
        console_secret,
        kind,
        ts,
        vec![
            vec!["h".into(), h.clone()],
            vec!["p".into(), member_pubkey.into()],
        ],
        "",
    )
    .map_err(|e| e.to_string())?;
    let event = serde_json::json!({
        "id": id,
        "pubkey": pubkey,
        "created_at": ts,
        "kind": kind,
        "tags": [["h", h], ["p", member_pubkey]],
        "content": "",
        "sig": sig,
    });
    publish_event_json(relay_url, console_secret, &event.to_string())
}

/// Marker tag on the runner-profile channel message.
pub const PROFILE_MESSAGE_TAG: &str = "fh-profile";

/// Publish (or replace) the runner's profile as a kind-9 channel message
/// marked `t`=`fh-profile` — the "pinned message" envelope. LIVE-VERIFIED:
/// kind 39000 (group metadata) is NOT in the stock buzz ingest scope
/// (`restricted: unknown event kind`), but kind-9 channel messages ride.
/// Same h + newer created_at = replacement (the fold keeps the newest per
/// channel — nothing appends history, same replace semantics as before).
pub fn publish_runner_meta(
    relay_url: &str,
    console_secret: &[u8; 32],
    profile: &RunnerProfile,
) -> Result<(), String> {
    let h = runner_channel_id(&profile.nostr_pubkey);
    let ts = now_secs();
    let content = serde_json::to_string(profile).map_err(|e| e.to_string())?;
    let (pubkey, id, sig) = crate::nip98::sign_event(
        console_secret,
        9,
        ts,
        vec![
            vec!["h".into(), h.clone()],
            vec!["d".into(), h.clone()],
            vec!["t".into(), PROFILE_MESSAGE_TAG.into()],
        ],
        &content,
    )
    .map_err(|e| e.to_string())?;
    let event = serde_json::json!({
        "id": id,
        "pubkey": pubkey,
        "created_at": ts,
        "kind": 9,
        "tags": [["h", h], ["d", h], ["t", PROFILE_MESSAGE_TAG]],
        "content": content,
        "sig": sig,
    });
    publish_event_json(relay_url, console_secret, &event.to_string())
}

/// Fetch the CURRENT profile of EVERY runner from the relay (kind-9
/// channel messages marked `t`=fh-profile). Author-gated + locally
/// signature-verified like the grant path; per channel (`h` tag) the NEWEST
/// trusted event wins (replaceable semantics), so a revoke REPLACES, never
/// appends. Sorted by name for a stable fold.
pub fn query_runner_metas(
    relay_url: &str,
    expected_author: &str,
    auth_secret: &[u8; 32],
) -> Result<Vec<RunnerProfile>, String> {
    let filters = serde_json::json!([{
        "kinds": [9],
        "#t": [PROFILE_MESSAGE_TAG],
        "limit": 1000,
    }]);
    let events = query_events(relay_url, auth_secret, filters)?;
    merge_runner_metas(&events, expected_author)
}

/// The trust + dedupe core shared by the HTTP query and its tests: keep
/// only events by `expected_author` (the console) whose Schnorr signature
/// verifies locally, then per channel (`h` tag) keep the NEWEST —
/// replaceable semantics, so a revoke REPLACES, never appends. Deterministic
/// (sorted by name) — the fold a respawned CP replays. A meta without an `h`
/// tag can't be attributed to a runner's channel (skip, warn); a malformed
/// WINNER fails the fold closed (only the trusted author could have written
/// it), a malformed non-winner warns and skips.
fn merge_runner_metas(
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
        let (tags, h) = parse_tags(ev);
        let Some(h) = h else {
            tracing::warn!(author, created_at, "runner meta missing h tag — skipped");
            continue;
        };
        let content = ev["content"].as_str().unwrap_or("");
        if crate::nip98::verify_event(
            author,
            created_at,
            9,
            &tags,
            content,
            ev["sig"].as_str().unwrap_or(""),
        )
        .is_err()
        {
            tracing::warn!(
                author,
                created_at,
                "runner meta message failed local signature verify — skipped"
            );
            continue;
        }
        // Later (or same-second, later-inserted) event wins per channel.
        if best.get(&h).is_none_or(|(t, _)| created_at >= *t) {
            let profile = parse_profile_content(content)?;
            best.insert(h, (created_at, profile));
        }
    }
    let mut out: Vec<RunnerProfile> = best.into_values().map(|(_, p)| p).collect();
    out.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(out)
}

/// Parse a relay event's tags into (tag array, `h` tag value).
fn parse_tags(ev: &Value) -> (Vec<Vec<String>>, Option<String>) {
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
    let h = tags
        .iter()
        .find(|t| t.first().is_some_and(|k| k == "h"))
        .and_then(|t| t.get(1))
        .cloned();
    (tags, h)
}

/// Read the runner's CURRENT roster (its exec whitelist) from the relay:
/// kinds [39002] filtered by `#d` = the runner's own channel. The roster is
/// RELAY-SIGNED — the trust anchor is `relay_pubkey` (the relay identity
/// that mints membership snapshots), verified LOCALLY; newest wins. Members
/// = the `p` tags. An absent/empty roster = no members = fail-closed deny
/// at the caller. Any query/verification failure is an Err.
///
/// LIVE-VERIFIED shape (rebuild 2026-08-20): buzz mints 39002 with a `d`
/// tag (the dashed channel UUID) + one `p`-tag per member (pk, "", role)
/// — NOT an `h` tag; NIP-01 filters match tags string-exactly, so the
/// filter is `#d` (dashed), never `#h`.
pub fn query_channel_roster(
    relay_url: &str,
    relay_pubkey: &str,
    runner_nostr_pubkey: &str,
    auth_secret: &[u8; 32],
) -> Result<Vec<String>, String> {
    let chan = runner_channel_id(runner_nostr_pubkey);
    let filters = serde_json::json!([{
        "kinds": [GROUP_MEMBERS_KIND],
        "#d": [chan],
        "limit": 100,
    }]);
    let events = query_events(relay_url, auth_secret, filters)?;
    merge_roster(&events, relay_pubkey, &chan)
}

/// Trust + dedupe core for rosters: keep only events authored by
/// `relay_pubkey` whose Schnorr signature verifies locally (a rogue member
/// cannot mint or clobber a roster — only the relay signs them), then per
/// channel keep the NEWEST; members = the `p` tags, sorted for determinism.
fn merge_roster(
    events: &[Value],
    relay_pubkey: &str,
    channel_id: &str,
) -> Result<Vec<String>, String> {
    let mut newest: Option<(i64, Vec<String>)> = None;
    for ev in events {
        let author = ev["pubkey"].as_str().unwrap_or("");
        if author != relay_pubkey {
            continue;
        }
        let created_at = ev["created_at"].as_i64().unwrap_or(0);
        let (tags, _h) = parse_tags(ev);
        // The roster's channel attribution is its `d` tag (live-verified).
        let d = tags
            .iter()
            .find(|t| t.first().is_some_and(|k| k == "d"))
            .and_then(|t| t.get(1))
            .cloned();
        if d.as_deref() != Some(channel_id) {
            continue;
        }
        let content = ev["content"].as_str().unwrap_or("");
        if crate::nip98::verify_event(
            author,
            created_at,
            GROUP_MEMBERS_KIND,
            &tags,
            content,
            ev["sig"].as_str().unwrap_or(""),
        )
        .is_err()
        {
            tracing::warn!(
                author,
                created_at,
                "roster failed local signature verify — skipped"
            );
            continue;
        }
        let members: Vec<String> = tags
            .iter()
            .filter(|t| t.first().is_some_and(|k| k == "p"))
            .filter_map(|t| t.get(1).cloned())
            .collect();
        if newest.as_ref().is_none_or(|(t, _)| created_at >= *t) {
            newest = Some((created_at, members));
        }
    }
    let mut members = newest.map(|(_, m)| m).unwrap_or_default();
    members.sort();
    members.dedup();
    Ok(members)
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
    fn meta_newest_wins_per_channel_and_rogue_author_is_ignored() {
        // Events for two channels (alpha twice: active then revoked ->
        // REPLACE), plus a rogue-author event for alpha's channel with a
        // NEWER timestamp -> must NOT win. The channel id is derived from
        // the runner pubkey, NEVER the name — two runners with the same
        // name would otherwise clobber each other's profile.
        let secret = [7u8; 32];
        let (author, _, _) = crate::nip98::sign_event(&secret, 1, 1, vec![], "").unwrap();
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
            risk: None,
        };
        let b = RunnerProfile {
            name: "beta".into(),
            nostr_pubkey: "b".repeat(64),
            ..a.clone()
        };
        // Distinct runner pubkey -> distinct channel id (the h tag).
        assert_ne!(
            runner_channel_id(&a.nostr_pubkey),
            runner_channel_id(&b.nostr_pubkey)
        );

        let mk = |p: &RunnerProfile, t: i64, who: &[u8; 32]| -> Value {
            let content = serde_json::to_string(p).unwrap();
            let h = runner_channel_id(&p.nostr_pubkey);
            let (pk, id, sig) = crate::nip98::sign_event(
                who,
                9,
                t,
                vec![
                    vec!["h".into(), h.clone()],
                    vec!["d".into(), h.clone()],
                    vec!["t".into(), PROFILE_MESSAGE_TAG.into()],
                ],
                &content,
            )
            .unwrap();
            serde_json::json!({
                "id": id, "pubkey": pk, "created_at": t,
                "kind": 9,
                "tags": [["h", h], ["d", h], ["t", PROFILE_MESSAGE_TAG]],
                "content": content, "sig": sig,
            })
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
        // Per-channel newest wins: push a REPLACED (revoked) alpha AFTER.
        let revoked = RunnerProfile {
            status: "revoked".into(),
            ..a.clone()
        };
        events.insert(2, mk(&revoked, 11, &secret));

        let out = merge_runner_metas(&events, &author).unwrap();
        assert_eq!(out.len(), 2);
        let alpha = out.iter().find(|p| p.name == "alpha").unwrap();
        assert_eq!(
            alpha.status, "revoked",
            "newest alpha must win (replace, not append)"
        );
        assert!(out.iter().any(|p| p.name == "beta" && p.status == "active"));
        // Deterministic fold: sorted by name regardless of event order.
        assert_eq!(out[0].name, "alpha");
        assert_eq!(out[1].name, "beta");
    }

    #[test]
    fn roster_newest_wins_and_rogue_signed_roster_is_ignored() {
        // The roster's trust anchor is the RELAY pubkey: only relay-authored
        // (Schnorr-verified) events are candidates. A rogue member posting a
        // NEWER "roster" granting themselves is ignored outright.
        let relay_secret = [42u8; 32];
        let (relay_pk, _, _) = crate::nip98::sign_event(&relay_secret, 1, 1, vec![], "").unwrap();
        let mk = |who: &[u8; 32], t: i64, channel: &str, members: &[&str]| -> Value {
            let tags: Vec<Vec<String>> = std::iter::once(vec!["d".into(), channel.into()])
                .chain(members.iter().map(|m| vec!["p".into(), m.to_string()]))
                .collect();
            let (pk, id, sig) =
                crate::nip98::sign_event(who, GROUP_MEMBERS_KIND, t, tags.clone(), "").unwrap();
            serde_json::json!({
                "id": id, "pubkey": pk, "created_at": t,
                "kind": GROUP_MEMBERS_KIND,
                "tags": tags,
                "content": "", "sig": sig,
            })
        };
        let (runner_pk, alice) = ("r".repeat(64), "a".repeat(64));
        let channel = runner_channel_id(&runner_pk);
        let rogue = [1u8; 32];
        let (rogue_pk, _, _) = crate::nip98::sign_event(&rogue, 1, 1, vec![], "").unwrap();

        let events = vec![
            mk(&relay_secret, 10, &channel, &[&runner_pk, &alice]),
            // Same channel: ROSTER REPLACE (revoke bob) — newest wins.
            mk(&relay_secret, 11, &channel, &[&runner_pk]),
            // A rogue "roster" with a NEWER timestamp, and a corrupted-signature
            // relay-signed one — both ignored.
            mk(&rogue, 999, &channel, &[&runner_pk, &rogue_pk]),
        ];
        let out = merge_roster(&events, &relay_pk, &channel).unwrap();
        assert_eq!(out, vec![runner_pk.clone()]);
        assert!(!out.contains(&alice), "alice revoked by the newer roster");
        assert!(
            !out.contains(&rogue_pk),
            "rogue cannot mint a roster — only the relay signs them"
        );

        // Different channel: nothing leaks across.
        let other = runner_channel_id(&("x".repeat(64)));
        let out = merge_roster(&events, &relay_pk, &other).unwrap();
        assert!(out.is_empty());
    }
}
