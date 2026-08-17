//! Hermetic fake for the buzz HTTP bridge (Phase D — grants on the relay).
//!
//! Implements the two surfaces the grant port uses, with REAL NIP-98 auth
//! verification (freehold-core's schnorr signer/verifier roundtrip) so the
//! tests exercise the actual wire contract — not a hand-wave:
//! - `GET /query?kinds=<n>&limit=<m>` → the stored events of that kind (as
//!   the buzz bridge returns an event array), only for a valid NIP-98 caller.
//! - `POST /events` → stores a NIP-98-authenticated event.
//!
//! Author/URL binding matches the real bridge: the `u`-tag must equal the
//! request URL (scheme://host:port/path?query).

use std::sync::Arc;

use axum::body::Bytes;
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode, header::HOST};
use axum::response::IntoResponse;
use axum::routing::post;
use axum::{Json, Router};
use parking_lot::Mutex;
use serde_json::{Value, json};
use tokio::net::TcpListener;

/// The fake's event store + a sample of recent callers (for assertions).
#[derive(Default)]
pub struct RelayState {
    pub events: Mutex<Vec<Value>>,
    pub authed_callers: Mutex<Vec<String>>,
    /// When true, POST /events returns 500 — exercises the publish-failure
    /// degradation (audit spool-only) without disturbing grants reads.
    pub block_events: Mutex<bool>,
}

pub type SharedRelay = Arc<RelayState>;

pub fn shared() -> SharedRelay {
    Arc::new(RelayState::default())
}

/// Sign + store a kind-30180 grant-list event for a runner (the fake acts as
/// the console/owner when tests drive the WRITE side).
pub fn publish_grants(
    state: &SharedRelay,
    console_secret: &[u8; 32],
    runner_pk: &str,
    grants: &[String],
) {
    let ts = freehold_core::auth::now_secs();
    let content = json!({ "grants": grants, "schema": 1 }).to_string();
    let (pubkey, id, sig) = freehold_core::nip98::sign_event(
        console_secret,
        30180,
        ts,
        vec![vec!["d".into(), runner_pk.into()]],
        &content,
    )
    .expect("sign grants event");
    state.events.lock().push(json!({
        "id": id,
        "pubkey": pubkey,
        "created_at": ts,
        "kind": 30180,
        "tags": [["d", runner_pk]],
        "content": content,
        "sig": sig,
    }));
}

fn verify_from_headers(headers: &HeaderMap, uri: &str) -> Result<String, String> {
    let auth = headers
        .get("authorization")
        .and_then(|v| v.to_str().ok())
        .ok_or_else(|| "no authorization header".to_string())?;
    let host = headers
        .get(HOST)
        .and_then(|v| v.to_str().ok())
        .ok_or_else(|| "no host header".to_string())?;
    let url = format!("http://{host}{uri}");
    // Both bridge surfaces are POST on the real relay (GET /query is 405).
    freehold_core::nip98::verify_nip98(auth, "POST", &url)
}

async fn query(
    State(state): State<SharedRelay>,
    headers: HeaderMap,
    uri: axum::extract::OriginalUri,
    body: Bytes,
) -> impl IntoResponse {
    // POST /query with a NIP-01 filter body — the real bridge shape. Mirror
    // the filter semantics for the cases we use: `kinds` + `#d`.
    let caller = match verify_from_headers(
        &headers,
        uri.path_and_query().map(|p| p.as_str()).unwrap_or("/query"),
    ) {
        Ok(pk) => pk,
        Err(e) => return (StatusCode::UNAUTHORIZED, Json(json!({ "error": e }))),
    };
    state.authed_callers.lock().push(caller);
    // The real bridge takes an ARRAY of filters (mirrored).
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
    // Generic single-letter-tag filter (#d/#p/#h fully mirrored; the real
    // bridge supports any single-letter index).
    let tag_vals = |e: &Value, letter: &str| -> Vec<String> {
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
    };
    let filter_tag = |e: &Value, letter: &str, want: &[String]| -> bool {
        want.is_empty() || tag_vals(e, letter).iter().any(|v| want.contains(v))
    };
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
        &headers,
        uri.path_and_query()
            .map(|p| p.as_str())
            .unwrap_or("/events"),
    ) {
        Ok(pk) => pk,
        Err(e) => return (StatusCode::UNAUTHORIZED, Json(json!({ "error": e }))),
    };
    state.authed_callers.lock().push(caller);
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
    let server = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (format!("http://{addr}"), state, server)
}
