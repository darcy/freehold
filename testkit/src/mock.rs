//! Mock HTTP API servers for the API-target connectors (Vultr, B2) — moved
//! verbatim from the C2/C3 connector tests so the ACCEPTANCE script and the
//! connector tests share one hermetic fixture. No real network.

use std::net::SocketAddr;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use axum::extract::{Path as AxPath, Query, State};
use axum::http::HeaderMap;
use axum::http::StatusCode;
use axum::routing::{get, post};
use axum::{Json, Router};
use parking_lot::Mutex;
use serde_json::{Value, json};
use tokio::net::TcpListener;

#[derive(Default)]
pub struct VultrState {
    pub instances: Mutex<Vec<(String, String)>>, // (id, label)
    pub next: AtomicU64,
}

pub const VULTR_TOKEN: &str = "vltr-token-123";

fn vultr_ok(headers: &HeaderMap) -> bool {
    headers.get("authorization").and_then(|v| v.to_str().ok())
        == Some(&format!("Bearer {VULTR_TOKEN}"))
}

pub fn vultr_router(state: Arc<VultrState>) -> Router {
    async fn list(
        State(state): State<Arc<VultrState>>,
        headers: HeaderMap,
    ) -> Result<Json<Value>, StatusCode> {
        if !vultr_ok(&headers) {
            return Err(StatusCode::UNAUTHORIZED);
        }
        let files: Vec<Value> = state
            .instances
            .lock()
            .iter()
            .map(|(id, label)| json!({ "id": id, "label": label }))
            .collect();
        Ok(Json(json!({ "instances": files })))
    }

    async fn create(
        State(state): State<Arc<VultrState>>,
        headers: HeaderMap,
    ) -> Result<Json<Value>, StatusCode> {
        if !vultr_ok(&headers) {
            return Err(StatusCode::UNAUTHORIZED);
        }
        let id = format!("inst-{}", state.next.fetch_add(1, Ordering::SeqCst));
        state.instances.lock().push((id.clone(), "mock".into()));
        Ok(Json(json!({
            "instance": { "id": id, "label": "mock", "status": "active", "main_ip": "10.0.0.1" }
        })))
    }

    async fn destroy(
        AxPath(id): AxPath<String>,
        State(state): State<Arc<VultrState>>,
        headers: HeaderMap,
    ) -> StatusCode {
        if !vultr_ok(&headers) {
            return StatusCode::UNAUTHORIZED;
        }
        state.instances.lock().retain(|(i, _)| *i != id);
        StatusCode::NO_CONTENT
    }

    async fn account(headers: HeaderMap) -> Result<Json<Value>, StatusCode> {
        if !vultr_ok(&headers) {
            return Err(StatusCode::UNAUTHORIZED);
        }
        Ok(Json(json!({ "account": {} })))
    }

    async fn instance(
        AxPath(id): AxPath<String>,
        State(state): State<Arc<VultrState>>,
        headers: HeaderMap,
    ) -> Result<Json<Value>, StatusCode> {
        if !vultr_ok(&headers) {
            return Err(StatusCode::UNAUTHORIZED);
        }
        let row = state
            .instances
            .lock()
            .iter()
            .find(|(i, _)| *i == id)
            .cloned();
        match row {
            Some((id, label)) => Ok(Json(json!({
                "instance": { "id": id, "label": label, "status": "active", "main_ip": "10.0.0.1" }
            }))),
            None => Err(StatusCode::NOT_FOUND),
        }
    }

    Router::new()
        .route("/v2/instances", get(list).post(create))
        .route("/v2/instances/{id}", get(instance).delete(destroy))
        .route("/v2/account", get(account))
        .with_state(state)
}

#[derive(Default)]
pub struct B2State {
    /// Uploaded bodies, in order (the mock's object store).
    pub files: Mutex<Vec<String>>,
    pub next: AtomicU64,
    /// Set after the mock binds (the authorize response must hand the client
    /// a reachable apiUrl).
    pub base_url: Mutex<String>,
}

pub const B2_CRED: &str = "keyid123:appkey456";
pub const B2_BASIC: &str = "Basic a2V5aWQxMjM6YXBwa2V5NDU2"; // base64("keyid123:appkey456")

pub fn b2_router(state: Arc<B2State>) -> Router {
    async fn authorize(
        State(state): State<Arc<B2State>>,
        headers: HeaderMap,
    ) -> Result<Json<Value>, StatusCode> {
        if headers.get("authorization").and_then(|v| v.to_str().ok()) != Some(B2_BASIC) {
            return Err(StatusCode::UNAUTHORIZED);
        }
        let base = state.base_url.lock();
        Ok(Json(json!({
            "apiUrl": *base,
            "authToken": "token-abc",
            "downloadUrl": format!("{base}/download"),
        })))
    }

    async fn get_upload_url(
        State(state): State<Arc<B2State>>,
        headers: HeaderMap,
    ) -> Result<Json<Value>, StatusCode> {
        if headers.get("authorization").and_then(|v| v.to_str().ok()) != Some("token-abc") {
            return Err(StatusCode::UNAUTHORIZED);
        }
        let base = state.base_url.lock();
        Ok(Json(json!({
            "uploadUrl": format!("{base}/b2api/v3/b2_upload_file"),
            "authorizationToken": "upload-tok",
        })))
    }

    async fn upload(
        State(state): State<Arc<B2State>>,
        headers: HeaderMap,
        body: String,
    ) -> Result<Json<Value>, StatusCode> {
        if headers.get("authorization").and_then(|v| v.to_str().ok()) != Some("upload-tok") {
            return Err(StatusCode::UNAUTHORIZED);
        }
        let id = format!("file-{}", state.next.fetch_add(1, Ordering::SeqCst));
        state.files.lock().push(body.clone());
        Ok(Json(
            json!({ "fileId": id, "fileName": format!("{id}.txt") }),
        ))
    }

    async fn list_names(
        State(state): State<Arc<B2State>>,
        headers: HeaderMap,
        body: String,
    ) -> Result<Json<Value>, StatusCode> {
        if headers.get("authorization").and_then(|v| v.to_str().ok()) != Some("token-abc") {
            return Err(StatusCode::UNAUTHORIZED);
        }
        let files: Vec<Value> = state
            .files
            .lock()
            .iter()
            .enumerate()
            .map(|(i, _)| json!({ "fileId": format!("file-{i}") }))
            .collect();
        Ok(Json(json!({ "files": files, "bucketId_echo": body })))
    }

    Router::new()
        .route("/b2api/v3/b2_authorize_account", get(authorize))
        .route("/b2api/v3/b2_get_upload_url", post(get_upload_url))
        .route("/b2api/v3/b2_upload_file", post(upload))
        .route("/b2api/v3/b2_list_file_names", post(list_names))
        .with_state(state)
}

/// Bind a router to an ephemeral loopback port; serve until the runtime ends.
pub async fn spawn_http(app: Router) -> SocketAddr {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    addr
}

#[derive(Default)]
pub struct HetznerState {
    /// (id, name) of live servers.
    pub servers: Mutex<Vec<(String, String)>>,
    pub next: AtomicU64,
}

pub const HETZNER_TOKEN: &str = "htz-token-123";

fn hetzner_ok(headers: &HeaderMap) -> bool {
    headers.get("authorization").and_then(|v| v.to_str().ok())
        == Some(&format!("Bearer {HETZNER_TOKEN}"))
}

/// cax11 unavailable in fsn1, cpx11 available — the driver's availability
/// fallback must pick cpx11 for a fsn1 request.
async fn hetzner_server_types(
    State(_): State<Arc<HetznerState>>,
    headers: HeaderMap,
) -> Result<Json<Value>, StatusCode> {
    if !hetzner_ok(&headers) {
        return Err(StatusCode::UNAUTHORIZED);
    }
    Ok(Json(json!({ "server_types": [
        { "name": "cax11", "deprecated": false, "locations": [
            { "name": "fsn1", "available": false }
        ]},
        { "name": "cpx11", "deprecated": false, "locations": [
            { "name": "fsn1", "available": true }
        ]}
    ] })))
}

pub fn hetzner_router(state: Arc<HetznerState>) -> Router {
    async fn create(
        State(state): State<Arc<HetznerState>>,
        headers: HeaderMap,
    ) -> Result<Json<Value>, StatusCode> {
        if !hetzner_ok(&headers) {
            return Err(StatusCode::UNAUTHORIZED);
        }
        let n = state.next.fetch_add(1, Ordering::Relaxed) + 1;
        let id = n.to_string();
        state
            .servers
            .lock()
            .push((id.clone(), format!("server-{n}")));
        Ok(Json(json!({ "server": {
            "id": n,
            "name": format!("server-{n}"),
            "status": "initializing",
            "public_net": { "ipv4": { "ip": format!("10.0.0.{n}") } }
        } })))
    }

    async fn instance(
        State(state): State<Arc<HetznerState>>,
        AxPath(id): AxPath<String>,
        headers: HeaderMap,
    ) -> Result<Json<Value>, StatusCode> {
        if !hetzner_ok(&headers) {
            return Err(StatusCode::UNAUTHORIZED);
        }
        if !state.servers.lock().iter().any(|(sid, _)| sid == &id) {
            return Err(StatusCode::NOT_FOUND);
        }
        Ok(Json(json!({
            "server": { "id": id, "status": "running",
                        "public_net": { "ipv4": { "ip": format!("10.0.0.{id}") } } }
        })))
    }

    async fn destroy(
        State(state): State<Arc<HetznerState>>,
        AxPath(id): AxPath<String>,
        headers: HeaderMap,
    ) -> StatusCode {
        if !hetzner_ok(&headers) {
            return StatusCode::UNAUTHORIZED;
        }
        let mut servers = state.servers.lock();
        let before = servers.len();
        servers.retain(|(sid, _)| sid != &id);
        if servers.len() == before {
            StatusCode::NOT_FOUND
        } else {
            StatusCode::NO_CONTENT
        }
    }

    Router::new()
        .route("/v1/servers", get(status_list).post(create))
        .route("/v1/servers/{id}", get(instance).delete(destroy))
        .route("/v1/server_types", get(hetzner_server_types))
        .with_state(state)
}

async fn status_list(
    State(_): State<Arc<HetznerState>>,
    headers: HeaderMap,
) -> Result<Json<Value>, StatusCode> {
    if !hetzner_ok(&headers) {
        return Err(StatusCode::UNAUTHORIZED);
    }
    Ok(Json(json!({ "servers": [] })))
}

pub const GITHUB_TOKEN: &str = "ghp-mock-123";

fn github_ok(headers: &HeaderMap) -> bool {
    headers.get("authorization").and_then(|v| v.to_str().ok())
        == Some(&format!("token {GITHUB_TOKEN}"))
}

pub fn github_router() -> Router {
    async fn user(headers: HeaderMap) -> Result<Json<Value>, StatusCode> {
        if !github_ok(&headers) {
            return Err(StatusCode::UNAUTHORIZED);
        }
        Ok(Json(json!({ "login": "mock-user", "id": 1 })))
    }
    async fn repo(
        AxPath(parts): AxPath<String>,
        headers: HeaderMap,
    ) -> Result<Json<Value>, StatusCode> {
        if !github_ok(&headers) {
            return Err(StatusCode::UNAUTHORIZED);
        }
        let mut p = parts.split('/');
        let owner = p.next().unwrap_or("?");
        let repo = p.next().unwrap_or("?");
        Ok(Json(
            json!({ "full_name": format!("{owner}/{repo}"), "private": false }),
        ))
    }
    Router::new()
        .route("/user", get(user))
        .route("/repos/{owner}/{repo}", get(repo))
}

pub const LITELLM_ADMIN_KEY: &str = "sk-lite-master-123";

fn litellm_admin_ok(headers: &HeaderMap) -> bool {
    headers.get("authorization").and_then(|v| v.to_str().ok())
        == Some(&format!("Bearer {LITELLM_ADMIN_KEY}"))
}

pub fn litellm_router() -> Router {
    async fn liveliness() -> Json<Value> {
        Json(json!({ "status": "ok" }))
    }
    async fn key_generate(headers: HeaderMap) -> Result<Json<Value>, StatusCode> {
        if !litellm_admin_ok(&headers) {
            return Err(StatusCode::UNAUTHORIZED);
        }
        Ok(Json(json!({
            "key": "sk-live-mock-123",
            "key_alias": "agent-x",
            "agent_id": "agent-x",
            "expires": null
        })))
    }
    Router::new()
        .route("/health/liveliness", get(liveliness))
        .route("/key/generate", post(key_generate))
}

pub const WEBSEARCH_KEY: &str = "ws-mock-123";

pub fn websearch_router() -> Router {
    async fn search(
        Query(q): Query<std::collections::HashMap<String, String>>,
    ) -> Result<Json<Value>, StatusCode> {
        if q.get("format").map(String::as_str) != Some("json") {
            return Err(StatusCode::BAD_REQUEST);
        }
        Ok(Json(json!({
            "results": [{ "title": "freehold mock hit", "url": "https://example.com/fh" }]
        })))
    }
    Router::new().route("/search", get(search))
}
