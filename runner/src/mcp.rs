//! Runner MCP-over-HTTP tool server.
//!
//! Locked decision: agent ↔ runner speaks MCP over HTTP even while co-located,
//! proving the real shape. JSON-RPC 2.0 framing on a single POST endpoint,
//! protocol subset: `initialize`, `notifications/initialized`, `ping`,
//! `tools/list`, `tools/call`.
//!
//! Tools (the runner contract): `list`, `exec`, `config`, `status`, `snapshot`.
//! `exec` is the generic primitive (Phase A4): the agent writes the command,
//! the runner executes it verbatim on the owned connection, resolves secrets
//! BY NAME from ciphertext, redacts their values from every response, and
//! signs the run into the local audit log (A6). `status` is the runner's OWN
//! readiness self-check (A5). `snapshot` lands with the Phase C connectors.
//!
//! Security:
//! - Binds loopback only; non-loopback `Origin` gets 403 (MCP DNS-rebinding rule).
//! - A session id is issued on `initialize` but NOT yet enforced. Real
//!   authentication (runner membership, grants) lands with Phase D — the
//!   endpoint is safe to run only on loopback until then.

use std::path::PathBuf;
use std::sync::Arc;

use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::post;
use axum::{Json, Router};
use freehold_core::{audit::Auditor, identity::Identity, secrets::SecretPackage};
use rand::RngCore;
use serde::Deserialize;
use serde_json::{Value, json};

use crate::exec::{self, ExecManager};
use crate::registry;
use crate::ssh::{SshPool, SshTarget};

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

/// Everything the runner server needs: identity (decrypt + audit signing),
/// the ciphertext secret package, and the state dir for the audit log.
#[derive(Clone)]
pub struct RunnerContext {
    pub identity: Identity,
    pub package: SecretPackage,
    pub state_dir: PathBuf,
}

#[derive(Clone)]
pub struct RunnerState {
    pub ctx: Arc<RunnerContext>,
    pub exec: Arc<ExecManager>,
    pub ssh: Arc<SshPool>,
}

pub fn router(ctx: RunnerContext) -> Router {
    let auditor = Arc::new(Auditor::new(
        hex_to_arr32(&ctx.identity.nostr_secret_hex())
            .expect("identity secrets are validated at load"),
    ));
    let state_dir = ctx.state_dir.clone();
    let state = RunnerState {
        ctx: Arc::new(ctx),
        exec: Arc::new(ExecManager::new(
            Some(auditor),
            Some(Arc::from(state_dir.as_path())),
        )),
        ssh: Arc::new(SshPool::new(&state_dir)),
    };
    Router::new()
        .route("/mcp", post(mcp_endpoint))
        .with_state(Arc::new(state))
}

/// Bind the MCP server to `addr` (loopback only). Returns the bound address and
/// a task handle; the task serves until aborted or the process exits.
pub async fn serve(
    addr: &str,
    ctx: RunnerContext,
) -> anyhow::Result<(std::net::SocketAddr, tokio::task::JoinHandle<()>)> {
    let listener = tokio::net::TcpListener::bind(addr).await?;
    let bound = listener.local_addr()?;
    if !bound.ip().is_loopback() {
        return Err(anyhow::anyhow!(
            "refusing non-loopback bind {bound}: the runner is unauthenticated \
             until Phase D — bind 127.0.0.1"
        ));
    }
    let app = router(ctx);
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

    // JSON-RPC batches were removed in protocol 2025-06-18; reject loudly
    // instead of classifying the array as a notification and silently 204-ing
    // (a client would hang to its own timeout with no log line).
    if body.is_array() {
        tracing::warn!("rejected JSON-RPC batch request (not supported in {PROTOCOL_VERSION})");
        return rpc_error(None, -32600, "batch requests are not supported".into());
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
        "tools/list" => rpc_result(id, json!({ "tools": tools() })),
        "tools/call" => {
            let params = body.get("params").cloned().unwrap_or(Value::Null);
            let name = params.get("name").and_then(Value::as_str).map(String::from);
            let arguments = params.get("arguments").cloned().unwrap_or(Value::Null);
            let (is_error, text) = match name.as_deref() {
                Some("list") => match serde_json::to_string_pretty(&registry::registered(
                    &state.ctx.package.targets,
                )) {
                    Ok(s) => (false, s),
                    Err(e) => (true, format!("list failed: {e}")),
                },
                Some("config") => match serde_json::to_string_pretty(&json!({
                    "name": SERVER_NAME,
                    "version": env!("CARGO_PKG_VERSION"),
                    "transport": "mcp-over-http",
                    "targets": registry::registered(&state.ctx.package.targets),
                })) {
                    Ok(s) => (false, s),
                    Err(e) => (true, format!("config failed: {e}")),
                },
                Some("exec") => match handle_exec(&state, &arguments).await {
                    Ok(s) => (false, s),
                    Err(e) => (true, e.to_string()),
                },
                Some("status") => match handle_status(&state, &arguments).await {
                    Ok(s) => (false, s),
                    Err(e) => (true, e.to_string()),
                },
                Some("snapshot") => (
                    true,
                    "snapshot not implemented until Phase C (connector state capture)".into(),
                ),
                Some(other) => (true, format!("unknown tool: {other}")),
                None => {
                    return rpc_error(
                        id,
                        -32602,
                        "invalid params: tools/call requires name".into(),
                    );
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

#[derive(Deserialize, Default)]
struct ExecArgs {
    cmd: Option<String>,
    target: Option<String>,
    #[serde(default)]
    secrets: Vec<String>,
    #[serde(default)]
    stream: bool,
    #[serde(default)]
    session_id: Option<String>,
    #[serde(default)]
    timeout_s: Option<u64>,
}

async fn handle_exec(state: &RunnerState, arguments: &Value) -> Result<String, exec::ExecError> {
    let args: ExecArgs = serde_json::from_value(arguments.clone())?;

    // Poll an existing streaming session (no cmd required).
    if let Some(sid) = &args.session_id {
        let snap = state.exec.poll(sid)?;
        return Ok(serde_json::to_string_pretty(&snap)?);
    }

    let cmd = args
        .cmd
        .clone()
        .ok_or(exec::ExecError::MissingField("cmd"))?;
    let target = args
        .target
        .clone()
        .ok_or(exec::ExecError::MissingField("target"))?;

    // LOCAL first — a shipped target may not shadow it (ordering must match
    // `status`, where local is checked before the shipped targets).
    if target == "local" {
        return handle_local_exec(state, &args, cmd).await;
    }

    // SSH target (shipped package metadata): verbatim command over the pooled
    // connection; credential = the target's private key PEM.
    if let Some(meta) = state.ctx.package.targets.get(&target) {
        if meta.kind != "ssh" {
            return Err(exec::ExecError::UnknownTarget(target));
        }
        if args.stream {
            return Err(exec::ExecError::Ssh(
                "streaming over ssh is not implemented yet (C1 ships non-streaming exec)".into(),
            ));
        }
        // The target's credential MUST be the ONLY requested secret: nothing
        // injects extras over the channel, so anything else would silently
        // run unset and surface as a confusing service failure.
        if !args.secrets.contains(&meta.secret) {
            return Err(exec::ExecError::UnknownSecret(format!(
                "target {target} requires secret {} in `secrets`",
                meta.secret
            )));
        }
        if let Some(extra) = args.secrets.iter().find(|n| *n != &meta.secret) {
            return Err(exec::ExecError::UnknownSecret(format!(
                "target {target} only accepts its own credential {extra:?} —                  ssh does not inject env vars"
            )));
        }
        let value =
            exec::resolve_secret_value(&state.ctx.identity, &state.ctx.package, &meta.secret)?;
        let redaction = [(exec::env_name(&meta.secret), value.clone())];
        let endpoint = SshTarget::parse(&target, &meta.address)
            .map_err(|e| exec::ExecError::Ssh(e.to_string()))?;
        let started = exec::now_secs();
        let mut result = state
            .ssh
            .exec(&endpoint, value.as_str(), &cmd, args.timeout_s)
            .await
            .map_err(|e| exec::ExecError::Ssh(e.to_string()))?;
        // Same rule as local: secret values never reach the agent.
        exec::redact(&mut result.stdout, &redaction);
        exec::redact(&mut result.stderr, &redaction);
        // The locked model signs EVERY executed command — ssh included.
        state.exec.audit_cmd(&cmd, &target, &result, started);
        return Ok(serde_json::to_string_pretty(&result)?);
    }

    Err(exec::ExecError::UnknownTarget(target))
}

async fn handle_local_exec(
    state: &RunnerState,
    args: &ExecArgs,
    cmd: String,
) -> Result<String, exec::ExecError> {
    let target = "local".to_string();

    let secrets = exec::resolve_secrets(&state.ctx.identity, &state.ctx.package, &args.secrets)?;

    if args.stream {
        let sid = state
            .exec
            .start_streaming(&cmd, &target, secrets.clone(), args.timeout_s);
        let snap = state.exec.poll(&sid)?;
        return Ok(serde_json::to_string_pretty(&snap)?);
    }

    let mut result = state
        .exec
        .run(&cmd, &target, secrets.clone(), args.timeout_s)
        .await?;
    // Secret values must never reach the agent — redact before returning.
    exec::redact(&mut result.stdout, &secrets);
    exec::redact(&mut result.stderr, &secrets);
    Ok(serde_json::to_string_pretty(&result)?)
}

async fn handle_status(state: &RunnerState, arguments: &Value) -> Result<String, exec::ExecError> {
    let target = arguments.get("target").and_then(Value::as_str);
    let requested: Vec<String> = match target {
        Some(t) => vec![t.to_string()],
        None => {
            let mut names: Vec<String> = state.ctx.package.targets.keys().cloned().collect();
            names.push("local".into());
            names
        }
    };

    let mut report: serde_json::Map<String, Value> = serde_json::Map::new();
    for entry in requested {
        let state_str = if entry == "local" {
            local_status(state).await?
        } else if let Some(meta) = state.ctx.package.targets.get(&entry) {
            if meta.kind != "ssh" {
                "red(unsupported kind)".to_string()
            } else {
                ssh_status(state, &entry, meta).await?
            }
        } else {
            return Err(exec::ExecError::UnknownTarget(entry));
        };
        report.insert(entry, Value::String(state_str));
    }
    Ok(serde_json::to_string_pretty(&report)?)
}

/// Runner's OWN self-check against the local host.
async fn local_status(state: &RunnerState) -> Result<String, exec::ExecError> {
    match state.exec.self_check("uname -s").await {
        Ok(r) if r.exit_code == Some(0) => Ok("green".into()),
        Ok(r) => Ok(format!("yellow(exit {})", r.exit_code.unwrap_or(-1))),
        Err(e) => Ok(format!("red({e})")),
    }
}

/// Runner's OWN self-check against an ssh target.
async fn ssh_status(
    state: &RunnerState,
    name: &str,
    meta: &freehold_core::secrets::TargetMeta,
) -> Result<String, exec::ExecError> {
    let value =
        match exec::resolve_secret_value(&state.ctx.identity, &state.ctx.package, &meta.secret) {
            Ok(v) => v,
            Err(e) => return Ok(format!("red({e})")),
        };
    let endpoint = match SshTarget::parse(name, &meta.address) {
        Ok(e) => e,
        Err(e) => return Ok(format!("red({e})")),
    };
    match state.ssh.self_check(&endpoint, value.as_str()).await {
        Ok(true) => Ok("green".into()),
        Ok(false) => Ok("yellow(self-check failed)".into()),
        Err(e) => Ok(format!("red({e})")),
    }
}

fn new_session_id() -> String {
    // TODO(Phase D): real session registry + enforcement. Issued now so clients
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

fn hex_to_arr32(s: &str) -> Result<[u8; 32], hex::FromHexError> {
    let bytes = hex::decode(s)?;
    let mut arr = [0u8; 32];
    arr.copy_from_slice(&bytes);
    Ok(arr)
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
                            Pass secret NAMES (not values) in `secrets`; the runner resolves \
                            them from ciphertext and injects them as env vars named by \
                            uppercase(secret) (`vultr-api-key` -> `VULTR_API_KEY`). Secret \
                            values never appear in output or the agent context. `stream: true` \
                            returns a session id; call exec again with that `session_id` to \
                            poll accumulated output until `done`. `timeout_s` kills a command \
                            that overruns.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "cmd": { "type": "string" },
                    "target": { "type": "string" },
                    "secrets": { "type": "array", "items": { "type": "string" } },
                    "stream": { "type": "boolean" },
                    "session_id": { "type": "string" },
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
                tool["inputSchema"]["type"], "object",
                "tool {} schema must declare type object, got {}",
                tool["name"], tool["inputSchema"]["type"]
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
