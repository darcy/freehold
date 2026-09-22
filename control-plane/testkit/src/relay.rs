//! Hermetic fake for the buzz HTTP bridge (Chunk 2.6.1 — runners as
//! NIP-29 channels).
//!
//! Implements the surfaces the runner-lifecycle port uses, with REAL NIP-98
//! auth verification (freehold-core's schnorr signer/verifier roundtrip) so
//! the tests exercise the actual wire contract — not a hand-wave:
//! - `POST /query` with a NIP-01 filter array → the stored events matching
//!   the filters (kinds + `#d`/`#p`/`#h`), only for a valid NIP-98 caller.
//! - `POST /events` → stores a NIP-98-authenticated event, and EXECUTES the
//!   channel semantics of the runner-lifecycle kinds:
//!   - kind 9007 (create): the caller becomes owner + auto-member; a re-create
//!     of an existing `h` is a no-op (idempotent provision/rebuild).
//!   - kind 9000 put-user / 9001 remove-user: ONLY the channel owner may
//!     issue them; each accepted change re-publishes a RELAY-SIGNED roster
//!     (kind 39002, `h` + one `p` tag per member) — the runner's whitelist.
//!   - kind 39002 from a client is refused: rosters are relay-minted.
//! - Channel-scoped roster reads are member-gated: a non-member querying
//!   kind 39002 `#h` gets `403 relay_membership_required` (the LIVE
//!   enforcement observed in the 2.6 smoke).
//!
//! Author/URL binding matches the real bridge: the `u`-tag must equal the
//! request URL (scheme://host:port/path?query).

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use axum::body::Bytes;
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode, header::HOST};
use axum::response::IntoResponse;
use axum::routing::post;
use axum::{Json, Router};
use freehold_core::nip98::{
    CHANNEL_CREATE_KIND, GROUP_MEMBERS_KIND, PUT_USER_KIND, REMOVE_USER_KIND,
};
use parking_lot::Mutex;
use serde_json::{Value, json};
use tokio::net::TcpListener;

/// The fixture relay identity: rosters are signed with this key (modeling
/// BUZZ_RELAY_PRIVATE_KEY) and the runner verifies them locally against
/// `relay_pubkey()`.
pub fn relay_key() -> [u8; 32] {
    [42u8; 32]
}

/// The relay's nostr pubkey — the roster trust anchor configured on runners.
pub fn relay_pubkey() -> String {
    let (pk, _, _) = freehold_core::nip98::sign_event(&relay_key(), 1, 1, vec![], "").unwrap();
    pk
}

/// A channel's ownership + membership (the relay's authority; the stored
/// 39002 events are its signed snapshots of this).
#[derive(Default)]
pub struct Channel {
    pub owner: String,
    pub members: BTreeSet<String>,
}

/// The fake's event store + channel registry + a sample of callers.
pub struct RelayState {
    pub events: Mutex<Vec<Value>>,
    pub authed_callers: Mutex<Vec<String>>,
    pub channels: Mutex<BTreeMap<String, Channel>>,
    /// When true, POST /events returns 500 — exercises the publish-failure
    /// degradation without disturbing grants reads.
    pub block_events: Mutex<bool>,
    /// The canonical base URL set at bind time. NIP-98 `u` verification uses
    /// THIS (like buzz derives it from its configured community domain), not
    /// the request Host — a client may dial a LAN origin while signing the
    /// canonical URL.
    pub canonical_url: Mutex<String>,
}

impl Default for RelayState {
    fn default() -> Self {
        Self {
            events: Mutex::new(Vec::new()),
            authed_callers: Mutex::new(Vec::new()),
            channels: Mutex::new(BTreeMap::new()),
            block_events: Mutex::new(false),
            canonical_url: Mutex::new(String::new()),
        }
    }
}

pub type SharedRelay = Arc<RelayState>;

pub fn shared() -> SharedRelay {
    Arc::new(RelayState::default())
}

fn verify_from_headers(
    state: &SharedRelay,
    headers: &HeaderMap,
    uri: &str,
) -> Result<String, String> {
    let auth = headers
        .get("authorization")
        .and_then(|v| v.to_str().ok())
        .ok_or_else(|| "no authorization header".to_string())?;
    // Prefer the bound canonical URL (buzz keys the community on its configured
    // domain, not the request Host); fall back to the request Host when unset.
    let base = state.canonical_url.lock().clone();
    let url = if base.is_empty() {
        let host = headers
            .get(HOST)
            .and_then(|v| v.to_str().ok())
            .ok_or_else(|| "no host header".to_string())?;
        format!("http://{host}{uri}")
    } else {
        format!("{base}{uri}")
    };
    // Both bridge surfaces are POST on the real relay (GET /query is 405).
    freehold_core::nip98::verify_nip98(auth, "POST", &url)
}

fn tag_vals(e: &Value, letter: &str) -> Vec<String> {
    e["tags"]
        .as_array()
        .map(|t| {
            t.iter()
                .filter(|t| {
                    t.as_array()
                        .is_some_and(|t| t.first().is_some_and(|k| k == letter))
                })
                .filter_map(|t| t.as_array().and_then(|t| t.get(1)).and_then(Value::as_str))
                .map(String::from)
                .collect()
        })
        .unwrap_or_default()
}

/// Relay-sign a kind-39002 roster snapshot for a channel and store it.
/// LIVE-VERIFIED shape (rebuild 2026-08-20): the roster's channel
/// attribution is a `d` tag (the dashed channel UUID) + one `p` tag per
/// member — NOT an `h` tag — and the author is the relay key.
fn publish_roster(state: &SharedRelay, channel_id: &str) {
    let members: Vec<String> = {
        let chans = state.channels.lock();
        chans
            .get(channel_id)
            .map(|c| c.members.iter().cloned().collect())
            .unwrap_or_default()
    };
    let ts = freehold_core::auth::now_secs();
    let tags: Vec<Vec<String>> = std::iter::once(vec!["d".into(), channel_id.into()])
        .chain(members.iter().map(|m| vec!["p".into(), m.clone()]))
        .collect();
    let (pubkey, id, sig) =
        freehold_core::nip98::sign_event(&relay_key(), GROUP_MEMBERS_KIND, ts, tags.clone(), "")
            .expect("sign roster");
    state.events.lock().push(json!({
        "id": id,
        "pubkey": pubkey,
        "created_at": ts,
        "kind": GROUP_MEMBERS_KIND,
        "tags": tags,
        "content": "",
        "sig": sig,
    }));
}

/// Apply the channel semantics of one accepted event (9007/9000/9001). The
/// event is already NIP-98-authenticated; ownership checks happen here.
/// Returns an error response when the relay must REFUSE the command.
fn apply_channel_semantics(
    state: &SharedRelay,
    caller: &str,
    ev: &Value,
) -> Result<(), (StatusCode, Json<Value>)> {
    let kind = ev["kind"].as_u64().unwrap_or(0) as u32;
    let hs = tag_vals(ev, "h");
    let Some(h) = hs.first() else {
        return Ok(()); // non-channel event — stored as-is
    };
    match kind {
        CHANNEL_CREATE_KIND => {
            if !state.channels.lock().contains_key(h) {
                state.channels.lock().insert(
                    h.clone(),
                    Channel {
                        owner: caller.to_string(),
                        members: BTreeSet::from([caller.to_string()]),
                    },
                );
                publish_roster(state, h);
            }
        }
        PUT_USER_KIND | REMOVE_USER_KIND => {
            let mut chans = state.channels.lock();
            let Some(channel) = chans.get_mut(h) else {
                return Err((
                    StatusCode::NOT_FOUND,
                    Json(json!({ "error": "relay: no such channel" })),
                ));
            };
            if channel.owner != caller {
                return Err((
                    StatusCode::FORBIDDEN,
                    Json(json!({ "error": "restricted: not the channel owner" })),
                ));
            }
            let Some(target) = tag_vals(ev, "p").first().cloned() else {
                return Ok(());
            };
            if kind == PUT_USER_KIND {
                channel.members.insert(target);
            } else {
                channel.members.remove(&target);
            }
            drop(chans);
            publish_roster(state, h);
        }
        GROUP_MEMBERS_KIND => {
            // Rosters are relay-minted — a client-authored roster is refuse;
            // a rogue member must not be able to grant itself exec rights.
            return Err((
                StatusCode::FORBIDDEN,
                Json(json!({ "error": "restricted: roster is relay-signed" })),
            ));
        }
        _ => {}
    }
    Ok(())
}

async fn query(
    State(state): State<SharedRelay>,
    headers: HeaderMap,
    uri: axum::extract::OriginalUri,
    body: Bytes,
) -> impl IntoResponse {
    // POST /query with a NIP-01 filter body — the real bridge shape.
    let caller = match verify_from_headers(
        &state,
        &headers,
        uri.path_and_query().map(|p| p.as_str()).unwrap_or("/query"),
    ) {
        Ok(pk) => pk,
        Err(e) => return (StatusCode::UNAUTHORIZED, Json(json!({ "error": e }))),
    };
    state.authed_callers.lock().push(caller.clone());
    let body_val: Value = serde_json::from_slice(&body).unwrap_or(Value::Null);
    let filter: &Value = body_val
        .as_array()
        .and_then(|a| a.first())
        .unwrap_or(&Value::Null);
    let kinds: Vec<u32> = filter["kinds"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|k| k.as_u64())
                .map(|k| k as u32)
                .collect()
        })
        .unwrap_or_default();
    let d_tags: Vec<String> = filter["#d"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|v| v.as_str().map(String::from))
                .collect()
        })
        .unwrap_or_default();
    let p_tags: Vec<String> = filter["#p"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|v| v.as_str().map(String::from))
                .collect()
        })
        .unwrap_or_default();
    let h_tags: Vec<String> = filter["#h"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|v| v.as_str().map(String::from))
                .collect()
        })
        .unwrap_or_default();
    let filter_tag = |e: &Value, letter: &str, want: &[String]| -> bool {
        want.is_empty() || tag_vals(e, letter).iter().any(|v| want.contains(v))
    };
    // Member gate: roster reads for a channel require the caller to be a
    // member of it (the LIVE enforcement: non-members get 403). Applies
    // whenever the filter targets kind 39002 via #h or #d (the live shape
    // uses #d — the roster's tag; #h is kept for the historical shape).
    let roster_ids: Vec<String> = if !d_tags.is_empty() {
        d_tags.clone()
    } else {
        h_tags.clone()
    };
    let roster_gate = kinds.contains(&GROUP_MEMBERS_KIND) && !roster_ids.is_empty();
    if roster_gate {
        let chans = state.channels.lock();
        let ok = roster_ids.iter().all(|id| {
            chans
                .get(id)
                .is_some_and(|c| c.members.contains(&caller) || c.owner == caller)
        });
        if !ok {
            return (
                StatusCode::FORBIDDEN,
                Json(json!({
                    "error": "relay_membership_required",
                    "message": "You must be a relay member to access this relay"
                })),
            );
        }
    }
    let events: Value = Value::Array(
        state
            .events
            .lock()
            .iter()
            .filter(|e| {
                (kinds.is_empty()
                    || e["kind"]
                        .as_u64()
                        .is_some_and(|k| kinds.contains(&(k as u32))))
                    && filter_tag(e, "d", &d_tags)
                    && filter_tag(e, "p", &p_tags)
                    && filter_tag(e, "h", &h_tags)
            })
            .cloned()
            .collect::<Vec<_>>(),
    );
    (StatusCode::OK, Json(events))
}

async fn publish(
    State(state): State<SharedRelay>,
    headers: HeaderMap,
    uri: axum::extract::OriginalUri,
    body: Bytes,
) -> impl IntoResponse {
    let caller = match verify_from_headers(
        &state,
        &headers,
        uri.path_and_query()
            .map(|p| p.as_str())
            .unwrap_or("/events"),
    ) {
        Ok(pk) => pk,
        Err(e) => return (StatusCode::UNAUTHORIZED, Json(json!({ "error": e }))),
    };
    state.authed_callers.lock().push(caller.clone());
    if *state.block_events.lock() {
        return (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(json!({ "error": "events blocked" })),
        );
    }
    let ev: Value = match serde_json::from_slice(&body) {
        Ok(v) => v,
        Err(e) => {
            return (
                StatusCode::BAD_REQUEST,
                Json(json!({ "error": e.to_string() })),
            );
        }
    };
    if let Err((status, body)) = apply_channel_semantics(&state, &caller, &ev) {
        return (status, body);
    }
    state.events.lock().push(ev);
    (StatusCode::OK, Json(json!({ "ok": true })))
}

/// Bind 127.0.0.1:0 and serve the fake relay; returns (url, state, server task).
pub async fn spawn() -> (String, SharedRelay, tokio::task::JoinHandle<()>) {
    let state = shared();
    let app = Router::new()
        .route("/query", post(query))
        .route("/events", post(publish))
        .with_state(state.clone());
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    *state.canonical_url.lock() = format!("http://{addr}");
    let server = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (format!("http://{addr}"), state, server)
}
