//! Runner MCP-over-HTTP tool server (Phase A3 — skeleton).
//!
//! Locked decision: agent ↔ runner speaks MCP over HTTP even while co-located,
//! proving the real shape. JSON-RPC 2.0 framing on a single POST endpoint,
//! protocol subset: `initialize`, `notifications/initialized`, `ping`,
//! `tools/list`, `tools/call`. Tools mirror the runner contract:
//! `list`, `exec`, `config`, `status`, `snapshot`.
//!
//! Phase A3 delivers transport + registry + framing. `exec` semantics land in
//! A4, readiness self-check in A5, audit in A6 — until then those tools answer
//! with a typed `isError` result explaining the pending phase.
//!
//! Security (pre-A4):
//! - Binds loopback only: a non-loopback `--addr` is rejected outright.
//! - `Origin` is validated per the MCP spec's DNS-rebinding rule for local
//!   servers: non-loopback origins get 403.
//! - A session id is issued on `initialize` but NOT yet enforced. Real
//!   enforcement + authentication (runner membership, grants) land with A4/D —
//!   the endpoint is safe to leave open only because exec does not exist yet.
//!
//! Minimal streamable-HTTP: JSON responses only (no SSE). If a third-party MCP
//! client needs to drive the runner directly, swap in a full SDK (e.g. rmcp) —
//! the tool contract is unchanged either way.

use std::sync::Arc;

use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::post;
use axum::{Json, Router};
use rand::RngCore;
use serde_json::{json, Value};

use crate::registry;

const PROTOCOL_VERSION: &str = "2025-06-18";
const SERVER_NAME: &str = "freehold-runner";
/// Loopback origins allowed by the DNS-rebinding guard (scheme + host only;
/// any port is fine — local UI ports vary; https covers mkcert/Tailscale-cert
/// local UIs).
const LOOPBACK_ORIGINS: [&str; 6] = [
    "http://localhost",
    "http://127.0.0.1",
    "http://[::1]",
    "https://localhost",
    "https://127.0.0.1",
    "https://[::1]",
];

#[derive(Clone, Default)]
pub struct RunnerState;

pub fn router() -> Router {
    Router::new()
        .route("/mcp", post(mcp_endpoint))
        .with_state(Arc::new(RunnerState))
}

/// Bind the MCP server to `addr` (loopback only). Returns the bound address and
/// a task handle; the task serves until aborted or the process exits.
pub async fn serve(
    addr: &str,
) -> anyhow::Result<(std::net::SocketAddr, tokio::task::JoinHandle<()>)> {
    let listener = tokio::net::TcpListener::bind(addr).await?;
    let bound = listener.local_addr()?;
    if !bound.ip().is_loopback() {
        return Err(anyhow::anyhow!(
            "refusing non-loopback bind {bound}: the runner is unauthenticated \
             until A4/D — bind 127.0.0.1"
        ));
    }
    let app = router();
    let handle = tokio::spawn(async move {
        if let Err(e) = axum::serve(listener, app).await {
            tracing::error!(error = %e, "MCP server failed");
        }
    });
    Ok((bound, handle))
}

async fn mcp_endpoint(
    State(_state): State<Arc<RunnerState>>,
    headers: HeaderMap,
    Json(body): Json<Value>,
) -> Response {
    // MCP spec: local HTTP servers MUST validate Origin to prevent DNS
    // rebinding. Browsers send Origin; curl/MCP clients don't (allowed).
    if let Some(origin) = headers
        .get("origin")
        .and_then(|v| v.to_str().ok())
        .filter(|o| !origin_is_loopback(o))
    {
        tracing::warn!(origin, "rejected non-loopback Origin (DNS rebinding guard)");
        return StatusCode::FORBIDDEN.into_response();
    }

    let incoming_session = headers
        .get("mcp-session-id")
        .and_then(|v| v.to_str().ok())
        .map(ToString::to_string);

    let id = body.get("id").cloned();
    let notification = id.is_none();

    let method = match (
        body.get("jsonrpc").and_then(|v| v.as_str()),
        body.get("method").and_then(|v| v.as_str()),
    ) {
        (Some("2.0"), Some(m)) => m,
        // JSON-RPC §4.1: never reply to a notification — that means an ABSENT
        // id. An explicit `"id": null` is a request per spec and gets a
        // null-id reply (it comes through as `Some(Value::Null)`).
        _ => {
            return if notification {
                StatusCode::NO_CONTENT.into_response()
            } else {
                rpc_error(id, -32600, "invalid request".into())
            };
        }
    };

    // JSON-RPC §4.1: never reply to a notification. ANY method sent without an
    // id is one — including spec notifications we don't otherwise handle
    // (progress, roots/list_changed, …). Dropping here means no match arm can
    // accidentally answer them.
    if notification {
        return StatusCode::NO_CONTENT.into_response();
    }

    let mut resp = match method {
        "initialize" => {
            let session_id = new_session_id();
            let mut resp = Json(json!({
                "jsonrpc": "2.0", "id": id,
                "result": {
                    "protocolVersion": PROTOCOL_VERSION,
                    "capabilities": { "tools": { "listChanged": false } },
                    "serverInfo": { "name": SERVER_NAME, "version": env!("CARGO_PKG_VERSION") }
                }
            }))
            .into_response();
            resp.headers_mut()
                .insert("mcp-session-id", session_id.parse().unwrap());
            resp
        }
        "ping" => rpc_result(id, json!({})),
        "tools/list" => {
            rpc_result(id, json!({ "tools": tools() }))
        }
        "tools/call" => {
            let params = body.get("params").cloned().unwrap_or(Value::Null);
            let name = params.get("name").and_then(Value::as_str).map(String::from);
            let (is_error, text) = match name.as_deref() {
                Some("list") => match serde_json::to_string_pretty(&registry::registered()) {
                    Ok(s) => (false, s),
                    Err(e) => (true, format!("list failed: {e}")),
                },
                Some("config") => match serde_json::to_string_pretty(&json!({
                    "name": SERVER_NAME,
                    "version": env!("CARGO_PKG_VERSION"),
                    "transport": "mcp-over-http",
                    "targets": registry::registered(),
                })) {
                    Ok(s) => (false, s),
                    Err(e) => (true, format!("config failed: {e}")),
                },
                // Pending phases — typed tool result, not a protocol error.
                Some("exec") => (
                    true,
                    "exec not implemented until Phase A4 (owned connection + verbatim command)".into(),
                ),
                Some("status") => (
                    true,
                    "status not implemented until Phase A5 (runner self-check readiness)".into(),
                ),
                Some("snapshot") => (
                    true,
                    "snapshot not implemented until Phase C (connector state capture)".into(),
                ),
                Some(other) => (true, format!("unknown tool: {other}")),
                None => {
                    return rpc_error(id, -32602, "invalid params: tools/call requires name".into());
                }
            };
            Json(json!({
                "jsonrpc": "2.0", "id": id,
                "result": {
                    "content": [{ "type": "text", "text": text }],
                    "isError": is_error
                }
            }))
            .into_response()
        }
        other => rpc_error(id, -32601, format!("method not found: {other}")),
    };

    // Echo the client's session id back (except on `initialize`, where we just
    // issued a fresh one — echoing a stale header would clobber it and the
    // client would never learn its new session).
    if method != "initialize"
        && let Some(sid) = incoming_session.and_then(|s| s.parse().ok())
    {
        resp.headers_mut().insert("mcp-session-id", sid);
    }
    resp
}

fn new_session_id() -> String {
    // TODO(A4/D): real session registry + enforcement. Issued now so clients
    // can pin the header; the value is opaque and stateless until then.
    let mut b = [0u8; 16];
    rand::rng().fill_bytes(&mut b);
    hex::encode(b)
}

fn origin_is_loopback(origin: &str) -> bool {
    LOOPBACK_ORIGINS.iter().any(|prefix| {
        origin == *prefix
            || origin
                .strip_prefix(prefix)
                .is_some_and(|rest| rest.starts_with(':'))
    })
}

fn tools() -> Vec<Value> {
    vec![
        json!({
            "name": "list",
            "description": "List the targets this runner can reach.",
            "inputSchema": { "type": "object", "properties": {} }
        }),
        json!({
            "name": "exec",
            "description": "Run `cmd` verbatim on `target` via the runner-owned connection. \
                            Pass secret names (not values) in `secrets`; the runner resolves them \
                            locally from ciphertext. `stream` returns output as pull-style chunks.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "cmd": { "type": "string" },
                    "target": { "type": "string" },
                    "secrets": { "type": "array", "items": { "type": "string" } },
                    "stream": { "type": "boolean" },
                    "timeout_s": { "type": "integer" }
                },
                "required": ["cmd", "target"]
            }
        }),
        json!({
            "name": "config",
            "description": "Non-secret runner configuration.",
            "inputSchema": { "type": "object", "properties": {} }
        }),
        json!({
            "name": "status",
            "description": "Self-check readiness for `target` (or all targets): green/yellow/red \
                            reported by the runner's OWN check against its service.",
            "inputSchema": {
                "type": "object",
                "properties": { "target": { "type": "string" } }
            }
        }),
        json!({
            "name": "snapshot",
            "description": "Capture the current state of `target` (or all targets).",
            "inputSchema": {
                "type": "object",
                "properties": { "target": { "type": "string" } }
            }
        }),
    ]
}

fn rpc_result(id: Option<Value>, result: Value) -> Response {
    Json(json!({ "jsonrpc": "2.0", "id": id, "result": result })).into_response()
}

fn rpc_error(id: Option<Value>, code: i64, message: String) -> Response {
    Json(json!({ "jsonrpc": "2.0", "id": id, "error": { "code": code, "message": message } }))
        .into_response()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn contract_tools_present() {
        let expected = ["list", "exec", "config", "status", "snapshot"];
        let tools = tools();
        let names: Vec<&str> = tools.iter().map(|t| t["name"].as_str().unwrap()).collect();
        assert_eq!(names, expected);
    }

    #[test]
    fn all_schemas_declare_object_type() {
        for tool in tools() {
            assert_eq!(
                tool["inputSchema"]["type"],
                "object",
                "tool {} schema must declare type object, got {}",
                tool["name"],
                tool["inputSchema"]["type"]
            );
        }
    }

    #[test]
    fn exec_schema_requires_cmd_and_target() {
        let tools = tools();
        let exec = tools.iter().find(|t| t["name"] == "exec").unwrap();
        let required = exec["inputSchema"]["required"].as_array().unwrap();
        assert!(required.contains(&json!("cmd")));
        assert!(required.contains(&json!("target")));
    }

    #[test]
    fn origin_guard_accepts_loopback_only() {
        for ok in [
            "http://localhost",
            "http://localhost:5173",
            "http://127.0.0.1:8787",
            "http://[::1]:8080",
            "https://localhost:8443",
            "https://127.0.0.1",
        ] {
            assert!(origin_is_loopback(ok), "{ok} should be allowed");
        }
        for bad in [
            "http://evil.example",
            "https://evil.example",
            "http://10.0.0.5:8787",
            "null",
        ] {
            assert!(!origin_is_loopback(bad), "{bad} should be rejected");
        }
    }
}
