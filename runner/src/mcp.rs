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
//! Minimal streamable-HTTP: JSON responses only (no SSE), an `Mcp-Session-Id`
//! is issued on `initialize` and echoed back, but not yet enforced. If a
//! third-party MCP client ever needs to drive the runner directly, swap in a
//! full SDK (e.g. rmcp) — the tool contract is unchanged either way.

use std::collections::HashSet;
use std::sync::Arc;

use parking_lot::Mutex;

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

#[derive(Clone, Default)]
pub struct RunnerState {
    sessions: Arc<Mutex<HashSet<String>>>,
}

pub fn router() -> Router {
    Router::new()
        .route("/mcp", post(mcp_endpoint))
        .with_state(Arc::new(RunnerState::default()))
}

/// Bind the MCP server to `addr` (localhost). Returns the bound address and a
/// task handle; awaiting the handle serves until it errors or is aborted.
pub async fn serve(
    addr: &str,
) -> anyhow::Result<(std::net::SocketAddr, tokio::task::JoinHandle<()>)> {
    let listener = tokio::net::TcpListener::bind(addr).await?;
    let bound = listener.local_addr()?;
    let app = router();
    let handle = tokio::spawn(async move {
        if let Err(e) = axum::serve(listener, app).await {
            tracing::error!(error = %e, "MCP server failed");
        }
    });
    Ok((bound, handle))
}

async fn mcp_endpoint(
    State(state): State<Arc<RunnerState>>,
    headers: HeaderMap,
    Json(body): Json<Value>,
) -> Response {
    // Echo the client's session id back on every response (spec-friendly).
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
        _ => return rpc_error(id, -32600, "invalid request".into()),
    };

    let mut resp = match method {
        "initialize" => {
            let session_id = state.new_session_id();
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
        // Notifications never carry ids — acknowledge and drop.
        "notifications/initialized" | "notifications/cancelled" => {
            return StatusCode::NO_CONTENT.into_response();
        }
        "ping" => {
            if notification {
                return StatusCode::NO_CONTENT.into_response();
            }
            rpc_result(id, json!({}))
        }
        "tools/list" => {
            if notification {
                return StatusCode::NO_CONTENT.into_response();
            }
            rpc_result(id, json!({ "tools": tools() }))
        }
        "tools/call" => {
            if notification {
                return StatusCode::NO_CONTENT.into_response();
            }
            let params = body.get("params").cloned().unwrap_or(Value::Null);
            let name = params.get("name").and_then(Value::as_str).map(String::from);
            let (is_error, text) = match name.as_deref() {
                Some("list") => (false, serde_json::to_string_pretty(&registry::registered()).unwrap()),
                Some("config") => (
                    false,
                    serde_json::to_string_pretty(&json!({
                        "name": SERVER_NAME,
                        "version": env!("CARGO_PKG_VERSION"),
                        "transport": "mcp-over-http",
                        "targets": registry::registered(),
                    }))
                    .unwrap(),
                ),
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

    if let Some(sid) = incoming_session {
        resp.headers_mut()
            .insert("mcp-session-id", sid.parse().unwrap());
    }
    resp
}

impl RunnerState {
    fn new_session_id(&self) -> String {
        let mut b = [0u8; 16];
        rand::rng().fill_bytes(&mut b);
        let sid = hex::encode(b);
        self.sessions.lock().insert(sid.clone());
        sid
    }
}

fn tools() -> Vec<Value> {
    vec![
        json!({
            "name": "list",
            "description": "List the targets this runner can reach.",
            "inputSchema": {}
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
            "inputSchema": {}
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
    fn exec_schema_requires_cmd_and_target() {
        let exec = tools().into_iter().find(|t| t["name"] == "exec").unwrap();
        let required = exec["inputSchema"]["required"].as_array().unwrap();
        assert!(required.contains(&json!("cmd")));
        assert!(required.contains(&json!("target")));
    }
}
