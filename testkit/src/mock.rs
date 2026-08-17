//! Mock HTTP API servers for the API-target connectors (Vultr, B2) — moved
//! verbatim from the C2/C3 connector tests so the ACCEPTANCE script and the
//! connector tests share one hermetic fixture. No real network.

use std::net::SocketAddr;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use axum::extract::{Path as AxPath, State};
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
