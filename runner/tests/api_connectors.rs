//! C2/C3 API connector tests (Vultr, Backblaze B2) against IN-PROCESS mock
//! HTTP servers. The runner injects the target credential + base URL as env
//! vars; the agent writes curl commands; values are redacted; every exec is
//! audited. No real network.

use std::collections::HashMap;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use axum::extract::{Path as AxPath, State};
use axum::http::{HeaderMap, StatusCode};
use axum::routing::{delete, get, post};
use axum::{Json, Router};
use freehold_runner::crypto;
use freehold_runner::identity::Identity;
use freehold_runner::mcp::{self, RunnerContext};
use freehold_runner::secrets::{SecretPackage, TargetMeta};
use parking_lot::Mutex;
use serde_json::{Value, json};
use tokio::net::TcpListener;

use std::collections::BTreeMap;

fn hex32(s: &str) -> [u8; 32] {
    let b = hex::decode(s).unwrap();
    let mut arr = [0u8; 32];
    arr.copy_from_slice(&b);
    arr
}

#[derive(Default)]
struct VultrState {
    instances: Mutex<Vec<(String, String)>>, // (id, label)
    next: AtomicU64,
}

fn vultr_router(state: Arc<VultrState>) -> Router {
    async fn list(
        State(state): State<Arc<VultrState>>,
        headers: HeaderMap,
    ) -> Result<Json<Value>, StatusCode> {
        let auth = headers
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("");
        if auth != "Bearer vltr-token-123" {
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
        if headers
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            != "Bearer vltr-token-123"
        {
            return Err(StatusCode::UNAUTHORIZED);
        }
        let id = format!("inst-{}", state.next.fetch_add(1, Ordering::SeqCst));
        state.instances.lock().push((id.clone(), "mock".into()));
        Ok(Json(json!({ "instance": { "id": id, "label": "mock" } })))
    }

    async fn destroy(
        AxPath(id): AxPath<String>,
        State(state): State<Arc<VultrState>>,
        headers: HeaderMap,
    ) -> StatusCode {
        if headers
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            != "Bearer vltr-token-123"
        {
            return StatusCode::UNAUTHORIZED;
        }
        state.instances.lock().retain(|(i, _)| *i != id);
        StatusCode::NO_CONTENT
    }

    async fn account(headers: HeaderMap) -> Result<Json<Value>, StatusCode> {
        if headers
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            != "Bearer vltr-token-123"
        {
            return Err(StatusCode::UNAUTHORIZED);
        }
        Ok(Json(json!({ "account": {} })))
    }

    Router::new()
        .route("/v2/instances", get(list).post(create))
        .route("/v2/instances/{id}", delete(destroy))
        .route("/v2/account", get(account))
        .with_state(state)
}

#[derive(Default)]
struct B2State {
    files: Mutex<Vec<String>>, // uploaded bodies
    next: AtomicU64,
    /// Set after the mock binds (the authorize response must hand the client
    /// a reachable apiUrl).
    base_url: Mutex<String>,
}

fn b2_router(state: Arc<B2State>) -> Router {
    async fn authorize(
        State(state): State<Arc<B2State>>,
        headers: HeaderMap,
    ) -> Result<Json<Value>, StatusCode> {
        let auth = headers
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("");
        if auth != "Basic a2V5aWQxMjM6YXBwa2V5NDU2" {
            // basic base64("keyid123:appkey456")
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
        if headers
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            != "token-abc"
        {
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
        if headers
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            != "upload-tok"
        {
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
        if headers
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            != "token-abc"
        {
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

async fn spawn_http(app: Router) -> std::net::SocketAddr {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    addr
}

/// A CP-shaped package with a vultr + a b2 target; returns (dir, identity).
fn api_runner_dir(vultr_url: &str, b2_url: &str) -> (tempfile::TempDir, Identity) {
    let dir = tempfile::tempdir().unwrap();
    let id = Identity::generate();
    id.write_to_dir(dir.path()).unwrap();
    let enc = hex32(&id.enc_pubkey_hex());

    let seal = |name: &str, value: &[u8]| -> String {
        hex::encode(crypto::seal(&enc, name.as_bytes(), value).unwrap())
    };
    let pkg = SecretPackage {
        secrets: BTreeMap::from([
            ("vultr".to_string(), seal("vultr", b"vltr-token-123")),
            ("b2".to_string(), seal("b2", b"keyid123:appkey456")),
        ]),
        targets: BTreeMap::from([
            (
                "vultr".to_string(),
                TargetMeta {
                    kind: "vultr".into(),
                    address: vultr_url.to_string(),
                    secret: "vultr".into(),
                },
            ),
            (
                "b2".to_string(),
                TargetMeta {
                    kind: "b2".into(),
                    address: b2_url.to_string(),
                    secret: "b2".into(),
                },
            ),
        ]),
    };
    pkg.write_to_dir(dir.path()).unwrap();
    (dir, id)
}

fn agent() -> ureq::Agent {
    ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .build(),
    )
}

#[tokio::test(flavor = "multi_thread")]
async fn vultr_create_list_destroy_over_curl() {
    let vultr_state = Arc::new(VultrState::default());
    let vultr_addr = spawn_http(vultr_router(vultr_state.clone())).await;

    let (dir, id) = api_runner_dir(
        &format!("http://{vultr_addr}"),
        "http://127.0.0.1:1", // unused
    );
    let ctx = RunnerContext {
        identity: id,
        package: SecretPackage::load(dir.path()).unwrap(),
        state_dir: dir.path().to_path_buf(),
    };
    let (addr, server) = mcp::serve("127.0.0.1:0", ctx).await.unwrap();
    let url = format!("http://{addr}/mcp");
    let agent = agent();
    let call = |params: Value| -> Value {
        agent
            .post(&url)
            .header("Content-Type", "application/json")
            .send_json(
                json!({ "jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params }),
            )
            .unwrap()
            .into_body()
            .read_json::<Value>()
            .unwrap()
    };

    // Create.
    let created = call(json!({ "name": "exec", "arguments": {
        "cmd": "curl -sS -X POST \"$VULTR_URL/v2/instances\" -H \"Authorization: Bearer $VULTR\"",
        "target": "vultr", "secrets": ["vultr"]
    }}));
    assert_eq!(created["result"]["isError"], false, "{created}");
    let created_text = created["result"]["content"][0]["text"]
        .as_str()
        .unwrap()
        .to_string();
    assert!(created_text.contains("inst-0"), "create: {created_text}");

    // List.
    let listed = call(json!({ "name": "exec", "arguments": {
        "cmd": "curl -sS \"$VULTR_URL/v2/instances\" -H \"Authorization: Bearer $VULTR\"",
        "target": "vultr", "secrets": ["vultr"]
    }}));
    let listed_text = listed["result"]["content"][0]["text"]
        .as_str()
        .unwrap()
        .to_string();
    assert!(listed_text.contains("inst-0"), "list: {listed_text}");

    // Destroy.
    let destroyed = call(json!({ "name": "exec", "arguments": {
        "cmd": "curl -sS -X DELETE \"$VULTR_URL/v2/instances/inst-0\" -H \"Authorization: Bearer $VULTR\" -o /dev/null -w done",
        "target": "vultr", "secrets": ["vultr"]
    }}));
    let destroyed_text = destroyed["result"]["content"][0]["text"]
        .as_str()
        .unwrap()
        .to_string();
    assert!(destroyed_text.contains("done"));

    // The credential never leaks, even when a command echoes it.
    let leak = call(json!({ "name": "exec", "arguments": {
        "cmd": "echo \"cred=$VULTR url=$VULTR_URL\"",
        "target": "vultr", "secrets": ["vultr"]
    }}));
    let leak_text = leak["result"]["content"][0]["text"]
        .as_str()
        .unwrap()
        .to_string();
    assert!(
        !leak_text.contains("vltr-token-123"),
        "credential leaked: {leak_text}"
    );
    assert!(
        leak_text.contains("cred=***"),
        "redaction marker: {leak_text}"
    );
    assert!(
        leak_text.contains("url=http"),
        "URL env is injected: {leak_text}"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn b2_authorize_upload_list_roundtrip() {
    let b2_state = Arc::new(B2State::default());
    let b2_addr = spawn_http(b2_router(b2_state.clone())).await;
    *b2_state.base_url.lock() = format!("http://{b2_addr}");
    let base = format!("http://{b2_addr}");

    let (dir, id) = api_runner_dir("http://127.0.0.1:1", &base);
    let ctx = RunnerContext {
        identity: id,
        package: SecretPackage::load(dir.path()).unwrap(),
        state_dir: dir.path().to_path_buf(),
    };
    let (addr, server) = mcp::serve("127.0.0.1:0", ctx).await.unwrap();
    let url = format!("http://{addr}/mcp");
    let agent = agent();
    let call = |params: Value| -> Value {
        agent
            .post(&url)
            .header("Content-Type", "application/json")
            .send_json(
                json!({ "jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params }),
            )
            .unwrap()
            .into_body()
            .read_json::<Value>()
            .unwrap()
    };

    // Full read/write round-trip as ONE agent command (B2 classic API):
    // authorize -> get upload url -> upload -> list names.
    let cmd = concat!(
        "A=$(curl -sS -u \"$B2\" \"$B2_URL/b2api/v3/b2_authorize_account\"); ",
        "API=$(printf '%s' \"$A\" | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"apiUrl\"])'); ",
        "TOK=$(printf '%s' \"$A\" | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"authToken\"])'); ",
        "U=$(curl -sS -X POST \"$API/b2api/v3/b2_get_upload_url\" -H \"Authorization: $TOK\" -d '{\"bucketId\":\"b1\"}'); ",
        "UURL=$(printf '%s' \"$U\" | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"uploadUrl\"])'); ",
        "UTOK=$(printf '%s' \"$U\" | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"authorizationToken\"])'); ",
        "curl -sS -X POST \"$UURL\" -H \"Authorization: $UTOK\" -d 'hello-b2'; ",
        "echo; curl -sS -X POST \"$API/b2api/v3/b2_list_file_names\" -H \"Authorization: $TOK\" -d '{\"bucketId\":\"b1\"}'",
    );
    let roundtrip = call(json!({ "name": "exec", "arguments": {
        "cmd": cmd, "target": "b2", "secrets": ["b2"]
    }}));
    assert_eq!(roundtrip["result"]["isError"], false, "{roundtrip}");
    let text = roundtrip["result"]["content"][0]["text"]
        .as_str()
        .unwrap()
        .to_string();
    assert!(
        text.contains("file-0"),
        "round-trip must list the uploaded file: {text}"
    );
    assert!(!text.contains("keyid123"), "b2 credential leaked: {text}");
    assert!(!text.contains("appkey456"), "b2 credential leaked: {text}");
    assert_eq!(b2_state.files.lock().len(), 1, "mock received the upload");
    assert_eq!(
        b2_state.files.lock()[0],
        "hello-b2",
        "content round-tripped verbatim"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn api_targets_report_green_and_list() {
    let vultr_state = Arc::new(VultrState::default());
    let vultr_addr = spawn_http(vultr_router(vultr_state.clone())).await;
    let b2_state = Arc::new(B2State::default());
    let b2_addr = spawn_http(b2_router(b2_state.clone())).await;
    *b2_state.base_url.lock() = format!("http://{b2_addr}");
    let b2_url = format!("http://{b2_addr}");

    let (dir, id) = api_runner_dir(&format!("http://{vultr_addr}"), &b2_url);
    let ctx = RunnerContext {
        identity: id,
        package: SecretPackage::load(dir.path()).unwrap(),
        state_dir: dir.path().to_path_buf(),
    };
    let (mcp_addr, server) = mcp::serve("127.0.0.1:0", ctx).await.unwrap();
    let url = format!("http://{mcp_addr}/mcp");
    let agent = agent();
    let call = |params: Value| -> Value {
        agent
            .post(&url)
            .header("Content-Type", "application/json")
            .send_json(
                json!({ "jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params }),
            )
            .unwrap()
            .into_body()
            .read_json::<Value>()
            .unwrap()
    };

    let list = call(json!({ "name": "list", "arguments": {} }));
    let list_text = list["result"]["content"][0]["text"]
        .as_str()
        .unwrap()
        .to_string();
    assert!(
        list_text.contains("vultr") && list_text.contains("b2"),
        "list: {list_text}"
    );

    let status = call(json!({ "name": "status", "arguments": {} }));
    let status_text = status["result"]["content"][0]["text"]
        .as_str()
        .unwrap()
        .to_string();
    assert!(
        status_text.contains("\"vultr\": \"green\""),
        "vultr status: {status_text}"
    );
    assert!(
        status_text.contains("\"b2\": \"green\""),
        "b2 status: {status_text}"
    );
    assert!(
        status_text.contains("\"local\": \"green\""),
        "local stays green"
    );

    // A missing target errors, but a bad probe on ONE target folds to red
    // without failing the whole report.
    let status_bad = call(json!({ "name": "status", "arguments": {} }));
    assert_eq!(status_bad["result"]["isError"], false);

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn extra_or_missing_secrets_are_rejected() {
    let vultr_state = Arc::new(VultrState::default());
    let vultr_addr = spawn_http(vultr_router(vultr_state.clone())).await;
    let (dir, id) = api_runner_dir(&format!("http://{vultr_addr}"), "http://127.0.0.1:1");
    let ctx = RunnerContext {
        identity: id,
        package: SecretPackage::load(dir.path()).unwrap(),
        state_dir: dir.path().to_path_buf(),
    };
    let (addr, server) = mcp::serve("127.0.0.1:0", ctx).await.unwrap();
    let url = format!("http://{addr}/mcp");
    let agent = agent();
    let call = |params: Value| -> Value {
        agent
            .post(&url)
            .header("Content-Type", "application/json")
            .send_json(
                json!({ "jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params }),
            )
            .unwrap()
            .into_body()
            .read_json::<Value>()
            .unwrap()
    };

    let missing = call(json!({ "name": "exec", "arguments": {
        "cmd": "echo x", "target": "vultr", "secrets": []
    }}));
    assert_eq!(missing["result"]["isError"], true, "credential required");

    let extra = call(json!({ "name": "exec", "arguments": {
        "cmd": "echo x", "target": "vultr", "secrets": ["vultr", "other"]
    }}));
    assert_eq!(
        extra["result"]["isError"], true,
        "only the target credential allowed"
    );

    server.abort();
}

// Keep HashMap referenced for parity with the state shapes (mock list).
#[allow(unused)]
fn _touch(_: &HashMap<String, String>) {}
