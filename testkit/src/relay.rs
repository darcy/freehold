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

use std::collections::HashMap;

use std::sync::Arc;

use axum::body::Bytes;
use axum::extract::{Query, State};
use axum::http::{HeaderMap, StatusCode, header::HOST};
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use parking_lot::Mutex;
use serde_json::{Value, json};
use tokio::net::TcpListener;

/// The fake's event store + a sample of recent callers (for assertions).
#[derive(Default)]
pub struct RelayState {
    pub events: Mutex<Vec<Value>>,
    pub authed_callers: Mutex<Vec<String>>,
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
    let method = if uri.starts_with("/events") {
        "POST"
    } else {
        "GET"
    };
    freehold_core::nip98::verify_nip98(auth, method, &url)
}

async fn query(
    State(state): State<SharedRelay>,
    headers: HeaderMap,
    uri: axum::extract::OriginalUri,
    Query(params): Query<HashMap<String, String>>,
) -> impl IntoResponse {
    let caller = match verify_from_headers(
        &headers,
        uri.path_and_query().map(|p| p.as_str()).unwrap_or("/query"),
    ) {
        Ok(pk) => pk,
        Err(e) => return (StatusCode::UNAUTHORIZED, Json(json!({ "error": e }))),
    };
    state.authed_callers.lock().push(caller);
    let kinds: Vec<u32> = params
        .get("kinds")
        .map(|k| k.split(',').filter_map(|s| s.trim().parse().ok()).collect())
        .unwrap_or_default();
    let events: Value = Value::Array(
        state
            .events
            .lock()
            .iter()
            .filter(|e| {
                kinds.is_empty()
                    || e["kind"]
                        .as_u64()
                        .is_some_and(|k| kinds.contains(&(k as u32)))
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
        .route("/query", get(query))
        .route("/events", post(publish))
        .with_state(state.clone());
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let server = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (format!("http://{addr}"), state, server)
}
