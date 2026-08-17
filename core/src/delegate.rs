//! Delegation (Phase E): the CPA hands a task to a relay-addressable peer
//! agent instead of calling a runner directly.
//!
//! Wire: NIP-29-style channel messages (kind 9) in a CPA-created OPEN
//! channel (kind 9007 create, `visibility=public` — any member may post;
//! the BUZZ private-channel roster gap is sidestepped, and 9007/9 pass the
//! stock ingest — no patch gate). A request and its result are correlated
//! by an opaque `id` echoed in the content envelopes:
//! `{"type":"job-request","id":..,"task":..}` -> peer ->
//! `{"type":"job-result","id":..,"ok":..,"out":..}`.
//! The PEER executes the task via its OWN runner-direct path (the delegation
//! mode transition: CPA -> peer -> runner, versus CPA -> runner).

use serde_json::{Value, json};

pub const CHANNEL_CREATE_KIND: u32 = 9007;
pub const STREAM_MSG_KIND: u32 = 9;

/// The delegation envelopes.
pub const JOB_REQUEST: &str = "job-request";
pub const JOB_RESULT: &str = "job-result";

/// A parsed delegation envelope (request or result).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Envelope {
    pub ty: String,
    pub id: String,
    pub task: Option<String>,
    pub ok: Option<bool>,
    pub out: Option<String>,
}

pub fn request_content(id: &str, task: &str) -> String {
    json!({ "type": JOB_REQUEST, "id": id, "task": task }).to_string()
}

pub fn result_content(id: &str, ok: bool, out: &str) -> String {
    json!({ "type": JOB_RESULT, "id": id, "ok": ok, "out": out }).to_string()
}

pub fn parse_envelope(content: &str) -> Option<Envelope> {
    let v: Value = serde_json::from_str(content).ok()?;
    Some(Envelope {
        ty: v.get("type")?.as_str()?.to_string(),
        id: v.get("id")?.as_str()?.to_string(),
        task: v.get("task").and_then(Value::as_str).map(String::from),
        ok: v.get("ok").and_then(Value::as_bool),
        out: v.get("out").and_then(Value::as_str).map(String::from),
    })
}

/// Create (idempotently) the auto-ops OPEN channel: kind-9007 with
/// `h`/`name`/`visibility=public`. Re-publishing with the same h is a no-op
/// for our purposes (creator == the CPA, auto-member).
pub fn ensure_channel(
    relay_url: &str,
    secret: &[u8; 32],
    channel_id: &str,
    name: &str,
) -> Result<(), String> {
    let event = signed_event_json(
        secret,
        CHANNEL_CREATE_KIND,
        vec![
            vec!["h".into(), channel_id.into()],
            vec!["name".into(), name.into()],
            vec!["visibility".into(), "open".into()],
        ],
        "",
    )?;
    crate::relay_http::publish_event_json(relay_url, secret, &event)
}

/// Post a channel message (kind 9), p-tagging the counterparty.
pub fn post_message(
    relay_url: &str,
    secret: &[u8; 32],
    channel_id: &str,
    mention_pubkey: &str,
    content: &str,
) -> Result<(), String> {
    let event = signed_event_json(
        secret,
        STREAM_MSG_KIND,
        vec![
            vec!["h".into(), channel_id.into()],
            vec!["p".into(), mention_pubkey.into()],
        ],
        content,
    )?;
    crate::relay_http::publish_event_json(relay_url, secret, &event)
}

/// Poll the channel for messages newer than `since`, NO p-filter (the real
/// relay serves an unfiltered kind-9 query fine, but a #p-FILTERED kind-9
/// query HANGS for some identities — verified live, so requester + executor
/// filter by author/id client-side). Every message is locally
/// signature-verified; returns (created_at, content, author) sorted.
pub fn poll_stream(
    relay_url: &str,
    secret: &[u8; 32],
    channel_id: &str,
    since: i64,
) -> Result<Vec<(i64, String, String)>, String> {
    let events = crate::relay_http::query_events(
        relay_url,
        secret,
        json!([{
            "kinds": [STREAM_MSG_KIND],
            "#h": [channel_id],
            "since": since,
            "limit": 100,
        }]),
    )?;
    collect_verified(events)
}

/// Local signature verify + sort for a poll result set (unfiltered and
/// p-filtered polls share it).
fn collect_verified(events: Vec<Value>) -> Result<Vec<(i64, String, String)>, String> {
    let mut out: Vec<(i64, String, String)> = Vec::new();
    for ev in events {
        let author = ev["pubkey"].as_str().unwrap_or("");
        let created_at = ev["created_at"].as_i64().unwrap_or(0);
        // Local signature verify over the event fields (the reader trusts
        // only genuinely-signed messages, whatever the bridge did).
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
            STREAM_MSG_KIND,
            &tags,
            content,
            ev["sig"].as_str().unwrap_or(""),
        )
        .is_err()
        {
            continue;
        }
        out.push((created_at, content.to_string(), author.to_string()));
    }
    out.sort_by_key(|(ts, _, _)| *ts);
    Ok(out)
}

/// Poll the channel for messages by `mention_pubkey` (p-tag) newer than
/// `since` — the p-FILTERED kind-9 query WORKS for the executor identity on
/// the live relay (it hung only for the long-lived requester identity); the
/// requester uses [`poll_stream`] instead. Locally signature-verified;
/// returns (created_at, content, author) sorted.
pub fn poll_stream_p(
    relay_url: &str,
    secret: &[u8; 32],
    channel_id: &str,
    mention_pubkey: &str,
    since: i64,
) -> Result<Vec<(i64, String, String)>, String> {
    let events = crate::relay_http::query_events(
        relay_url,
        secret,
        json!([{
            "kinds": [STREAM_MSG_KIND],
            "#h": [channel_id],
            "#p": [mention_pubkey],
            "since": since,
            "limit": 100,
        }]),
    )?;
    collect_verified(events)
}

/// Build a signed event JSON object (shared by the channel helpers).
fn signed_event_json(
    secret: &[u8; 32],
    kind: u32,
    tags: Vec<Vec<String>>,
    content: &str,
) -> Result<String, String> {
    let ts = crate::auth::now_secs();
    let (pubkey, id, sig) = crate::nip98::sign_event(secret, kind, ts, tags.clone(), content)
        .map_err(|e| e.to_string())?;
    Ok(json!({
        "id": id,
        "pubkey": pubkey,
        "created_at": ts,
        "kind": kind,
        "tags": tags,
        "content": content,
        "sig": sig,
    })
    .to_string())
}
