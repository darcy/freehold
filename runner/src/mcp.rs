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
use freehold_core::auth;

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
/// the ciphertext secret package, the state dir for the audit log, and the
/// relay this runner belongs to (Chunk 2.6.1: the whitelist is read LIVE
/// from the runner's OWN NIP-29 channel roster once configured; None = the
/// shipped-package grants are the source, so loopback-only/local runners
/// keep working before any relay exists).
#[derive(Clone)]
pub struct RunnerContext {
    pub identity: Identity,
    pub package: SecretPackage,
    pub state_dir: PathBuf,
    /// Relay whitelist source (Chunk 2.6.1): when set, grants are the
    /// RELAY-SIGNED roster of the runner's own channel (kind 39002,
    /// `h` = sha256 of the runner pubkey) instead of the shipped package.
    pub relay_url: Option<String>,
    /// The RELAY's nostr pubkey (64-hex) — the roster's trust anchor —
    /// REQUIRED when relay_url is set (fail-fast at serve; a roster
    /// accepted from any other author would be a self-admission hole).
    pub relay_pubkey: Option<String>,
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
    let relay_url = ctx.relay_url.clone();
    let state = RunnerState {
        ctx: Arc::new(ctx),
        exec: Arc::new(ExecManager::new(
            Some(auditor),
            Some(Arc::from(state_dir.as_path())),
            relay_url,
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
    if ctx.relay_url.is_some() && ctx.relay_pubkey.is_none() {
        return Err(anyhow::anyhow!(
            "--relay-pubkey is REQUIRED when --relay-url is set (the roster's \
             trust anchor; accepting a whitelist signed by any other author \
             is a self-admission hole)"
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

#[axum::debug_handler]
async fn mcp_endpoint(
    State(state): State<Arc<RunnerState>>,
    headers: HeaderMap,
    body: String,
) -> Response {
    let raw_body = body.clone();
    let body: Value = match serde_json::from_str(&body) {
        Ok(v) => v,
        Err(_) => {
            return rpc_error(None, -32700, "parse error".into());
        }
    };
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
            // Phase D grants: every privileged call must be signed by a
            // GRANTED agent pubkey. Grants are re-read from the shipped
            // package so a `control-plane grant` lands without a restart.
            let params = body.get("params").cloned().unwrap_or(Value::Null);
            // Phase D grants may hit the relay over blocking HTTP — run on the
            // blocking pool so a slow/unreachable relay can never starve the
            // async workers (or deadlock against a co-located relay server).
            let grant_ctx = state.ctx.clone();
            let grants = tokio::task::spawn_blocking(move || {
                current_grants(
                    &grant_ctx.state_dir,
                    grant_ctx.relay_url.as_deref(),
                    grant_ctx.relay_pubkey.as_deref(),
                    &grant_ctx.identity,
                )
            })
            .await
            .expect("grant check task panicked");
            let runner_pubkey = state.ctx.identity.nostr_pubkey_hex();
            let caller = match auth::verify_body(
                &grants,
                &runner_pubkey,
                headers
                    .get(auth::PUBKEY_HEADER)
                    .and_then(|v| v.to_str().ok()),
                headers.get(auth::SIG_HEADER).and_then(|v| v.to_str().ok()),
                headers.get(auth::TS_HEADER).and_then(|v| v.to_str().ok()),
                &raw_body,
            ) {
                Ok(pubkey) => pubkey,
                Err(e) => {
                    tracing::warn!(error = %e, "denied tools/call without a valid grant");
                    return rpc_error(id, -32001, format!("unauthorized: {e}"));
                }
            };
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
                Some("exec") => match handle_exec(&state, &arguments, &caller).await {
                    Ok(s) => (false, s),
                    Err(e) => (true, e.to_string()),
                },
                Some("upload") => match handle_upload(&state, &arguments, &caller).await {
                    Ok(s) => (false, s),
                    Err(e) => (true, e.to_string()),
                },
                Some("status") => match handle_status(&state, &arguments, &caller).await {
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

async fn handle_exec(
    state: &RunnerState,
    arguments: &Value,
    caller: &str,
) -> Result<String, exec::ExecError> {
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
        return handle_local_exec(state, &args, cmd, caller).await;
    }

    // Shipped target (package metadata): dispatch by connector kind.
    if let Some(meta) = state.ctx.package.targets.get(&target) {
        require_target_credential(state, &args.secrets, &target, meta)?;

        // SSH: verbatim command over the pooled connection; credential = the
        // target's private key PEM.
        if meta.kind == "ssh" {
            if args.stream {
                return Err(exec::ExecError::Ssh(
                    "streaming over ssh is not implemented yet (C1 ships non-streaming exec)"
                        .into(),
                ));
            }
            let value =
                exec::resolve_secret_value(&state.ctx.identity, &state.ctx.package, &meta.secret)?;
            // Every requested secret becomes an env var ON THE REMOTE BODY (like
            // the API connector): the target's own credential for auth, plus any
            // extras the agent asked for by name. All are redacted from output.
            let secrets = exec::resolve_secrets(&state.ctx.identity, &state.ctx.package, &args.secrets)?;
            let endpoint = SshTarget::parse(&target, &meta.address)
                .map_err(|e| exec::ExecError::Ssh(e.to_string()))?;
            let started = exec::now_secs();
            let mut result = state
                .ssh
                .exec(&endpoint, value.as_str(), &cmd, &secrets, args.timeout_s)
                .await
                .map_err(|e| exec::ExecError::Ssh(e.to_string()))?;
            // Same rule as local: secret values never reach the agent.
            exec::redact(&mut result.stdout, &secrets);
            exec::redact(&mut result.stderr, &secrets);
            // The locked model signs EVERY executed command — ssh included.
            state
                .exec
                .audit_cmd(&cmd, &target, &result, started, Some(caller));
            return Ok(serde_json::to_string_pretty(&result)?);
        }

        // API connector (vultr, b2, ...): LOCAL-run process with the target's
        // credential + base URL injected as env vars — the agent writes curl
        // commands; the runner injects creds, redacts values, signs the audit.
        return handle_api_exec(state, &args, cmd, &target, meta, caller).await;
    }

    /// The target's own credential must be requested, and EVERY requested name
    /// must exist in the package — a runner may carry extra named secrets
    /// (e.g. litellm's provider key and the postgres password alongside the
    /// target credential); each is injected + redacted by handle_api_exec.
    /// Unknown names fail closed: no silently-unset env vars.
    fn require_target_credential(
        state: &RunnerState,
        requested: &[String],
        target: &str,
        meta: &freehold_core::secrets::TargetMeta,
    ) -> Result<(), exec::ExecError> {
        if !requested.contains(&meta.secret) {
            return Err(exec::ExecError::UnknownSecret(format!(
                "target {target} requires secret {} in `secrets`",
                meta.secret
            )));
        }
        for name in requested {
            if !state.ctx.package.secrets.contains_key(name) {
                return Err(exec::ExecError::UnknownSecret(format!(
                    "requested secret {name:?} is not in this runner's package — \
                     {target} knows {}",
                    meta.secret
                )));
            }
        }
        Ok(())
    }

    Err(exec::ExecError::UnknownTarget(target))
}

/// Run a command on the runner host with the target credential and base URL
/// in env: `<SECRET>_URL` carries `meta.address`; the target's credential is
/// injected under its env name, and EVERY extra requested secret the package
/// carries (e.g. litellm's provider key, the postgres password) is injected
/// under its own env name and redacted from output. `run` (and streaming)
/// sign the audit.
async fn handle_api_exec(
    state: &RunnerState,
    args: &ExecArgs,
    cmd: String,
    target: &str,
    meta: &freehold_core::secrets::TargetMeta,
    caller: &str,
) -> Result<String, exec::ExecError> {
    // Resolve EVERY requested name (the target credential + any extras the
    // agent asked for by name) — each becomes an env var; all get redacted.
    let secrets = exec::resolve_secrets(&state.ctx.identity, &state.ctx.package, &args.secrets)?;
    let url_env = format!("{}_URL", exec::env_name(&meta.secret));
    let mut envs: Vec<(String, zeroize::Zeroizing<String>)> = secrets.clone();
    envs.push((url_env, zeroize::Zeroizing::new(meta.address.clone())));

    if args.stream {
        let sid =
            state
                .exec
                .start_streaming(&cmd, target, envs.clone(), args.timeout_s, Some(caller));
        let snap = state.exec.poll(&sid)?;
        return Ok(serde_json::to_string_pretty(&snap)?);
    }

    // run() signs the audit; EVERY injected value is redacted from output.
    let mut result = state
        .exec
        .run(&cmd, target, envs, args.timeout_s, Some(caller))
        .await?;
    exec::redact(&mut result.stdout, &secrets);
    exec::redact(&mut result.stderr, &secrets);
    Ok(serde_json::to_string_pretty(&result)?)
}

/// Runner's OWN self-check against an API connector: a curl probe with the
/// credential in env; HTTP 200 = green. Never reports red for the whole
/// report — every failure folds into the string.
async fn api_status(
    state: &RunnerState,
    meta: &freehold_core::secrets::TargetMeta,
) -> Result<String, exec::ExecError> {
    let value =
        match exec::resolve_secret_value(&state.ctx.identity, &state.ctx.package, &meta.secret) {
            Ok(v) => v,
            Err(e) => return Ok(format!("red({e})")),
        };
    let cred = exec::env_name(&meta.secret);
    let url_env = format!("{cred}_URL");
    let envs = vec![
        (cred.clone(), value.clone()),
        (
            url_env.clone(),
            zeroize::Zeroizing::new(meta.address.clone()),
        ),
    ];
    let cmd = match meta.kind.as_str() {
        "vultr" => format!(
            "curl -sS -o /dev/null -w '%{{http_code}}' \"${{{url_env}}}/v2/account\" -H \
             \"Authorization: Bearer ${{{cred}}}\""
        ),
        "b2" => format!(
            "curl -sS -o /dev/null -w '%{{http_code}}' -u \"${{{cred}}}\" \
             \"${{{url_env}}}/b2api/v3/b2_authorize_account\""
        ),
        "hetzner" => format!(
            "curl -sS -o /dev/null -w '%{{http_code}}' \"${{{url_env}}}/v1/servers\" -H \
             \"Authorization: Bearer ${{{cred}}}\""
        ),
        "github" => format!(
            "curl -sS -o /dev/null -w '%{{http_code}}' \"${{{url_env}}}/user\" -H \
             \"Authorization: token ${{{cred}}}\""
        ),
        "websearch" => format!(
            "curl -sS -o /dev/null -w '%{{http_code}}' \
             \"${{{url_env}}}/search?q=freehold&format=json\""
        ),
        "litellm" => format!(
            "curl -sS -o /dev/null -w '%{{http_code}}' \
             \"${{{url_env}}}/health/liveliness\" -H \
             \"Authorization: Bearer ${{{cred}}}\""
        ),
        k => return Ok(format!("red(unsupported api kind {k})")),
    };
    match state.exec.probe_env(&cmd, &envs).await {
        Ok(r) if r.stdout.trim() == "200" => Ok("green".into()),
        Ok(r) => Ok(format!("yellow(probe {})", r.stdout.trim())),
        Err(e) => Ok(format!("red({e})")),
    }
}

async fn handle_upload(
    state: &RunnerState,
    arguments: &Value,
    caller: &str,
) -> Result<String, exec::ExecError> {
    let target = arguments
        .get("target")
        .and_then(Value::as_str)
        .ok_or(exec::ExecError::MissingField("target"))?
        .to_string();
    let local_path = arguments
        .get("local_path")
        .and_then(Value::as_str)
        .ok_or(exec::ExecError::MissingField("local_path"))?;
    let remote_path = arguments
        .get("remote_path")
        .and_then(Value::as_str)
        .ok_or(exec::ExecError::MissingField("remote_path"))?;
    if !std::path::Path::new(local_path).exists() {
        return Err(exec::ExecError::MissingField("local_path"));
    }
    if target == "local" {
        // loopback: a direct local file copy (no ssh lane) — STILL audited
        // (the ssh branch's write happens under `ssh.upload`; here the copy
        // IS the op, so the 48001/spool row must be signed like any exec).
        let started = exec::now_secs();
        let n = std::fs::copy(local_path, remote_path)?;
        let result = exec::ExecResult {
            stdout: format!("uploaded {n} bytes to {remote_path}"),
            stderr: String::new(),
            exit_code: Some(0),
            timed_out: false,
        };
        let cmd = format!("upload {remote_path} (from {local_path})");
        state
            .exec
            .audit_cmd(&cmd, &target, &result, started, Some(caller));
        return Ok(format!("{{\"uploaded\": {n}}}"));
    }
    let Some(meta) = state.ctx.package.targets.get(&target) else {
        return Err(exec::ExecError::UnknownTarget(target));
    };
    if meta.kind != "ssh" {
        return Err(exec::ExecError::Ssh(format!(
            "upload only supported on ssh targets ({} is {})",
            target, meta.kind
        )));
    }
    let value = exec::resolve_secret_value(&state.ctx.identity, &state.ctx.package, &meta.secret)?;
    let endpoint = SshTarget::parse(&target, &meta.address)
        .map_err(|e| exec::ExecError::Ssh(e.to_string()))?;
    let started = exec::now_secs();
    let n = state
        .ssh
        .upload(
            &endpoint,
            value.as_str(),
            std::path::Path::new(local_path),
            remote_path,
        )
        .await
        .map_err(|e| exec::ExecError::Ssh(e.to_string()))?;
    let result = exec::ExecResult {
        stdout: format!("uploaded {n} bytes to {remote_path}"),
        stderr: String::new(),
        exit_code: Some(0),
        timed_out: false,
    };
    let cmd = format!("upload {remote_path} (from {local_path})");
    state
        .exec
        .audit_cmd(&cmd, &target, &result, started, Some(caller));
    Ok(format!("{{\"uploaded\": {n}}}"))
}

async fn handle_local_exec(
    state: &RunnerState,
    args: &ExecArgs,
    cmd: String,
    caller: &str,
) -> Result<String, exec::ExecError> {
    let target = "local".to_string();

    let secrets = exec::resolve_secrets(&state.ctx.identity, &state.ctx.package, &args.secrets)?;

    if args.stream {
        let sid = state.exec.start_streaming(
            &cmd,
            &target,
            secrets.clone(),
            args.timeout_s,
            Some(caller),
        );
        let snap = state.exec.poll(&sid)?;
        return Ok(serde_json::to_string_pretty(&snap)?);
    }

    let mut result = state
        .exec
        .run(&cmd, &target, secrets.clone(), args.timeout_s, Some(caller))
        .await?;
    // Secret values must never reach the agent — redact before returning.
    exec::redact(&mut result.stdout, &secrets);
    exec::redact(&mut result.stderr, &secrets);
    Ok(serde_json::to_string_pretty(&result)?)
}

async fn handle_status(
    state: &RunnerState,
    arguments: &Value,
    _caller: &str,
) -> Result<String, exec::ExecError> {
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
    // the connection LANE's health — lets the CP/TUI ALERT when the lane
    // keeps dropping or can't reconnect (core liveness feature).
    let h = state.ssh.health();
    report.insert(
        "runner_lane".into(),
        json!({
            "drops": h.drops,
            "reconnects": h.reconnects,
            "last_drop": h.last_drop,
            "last_error": h.last_error,
        }),
    );
    for entry in requested {
        let state_str = if entry == "local" {
            local_status(state).await?
        } else if let Some(meta) = state.ctx.package.targets.get(&entry) {
            match meta.kind.as_str() {
                "ssh" => ssh_status(state, &entry, meta).await?,
                "vultr" | "b2" | "hetzner" | "github" | "websearch" | "litellm" => {
                    api_status(state, meta).await?
                }
                k => format!("red(unsupported kind {k})"),
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

/// Grants shipped with the package, re-read fresh so `control-plane grant`
/// takes effect without a runner restart. Unreadable package -> fail closed.
fn current_grants(
    state_dir: &std::path::Path,
    relay_url: Option<&str>,
    relay_pubkey: Option<&str>,
    identity: &Identity,
) -> Vec<String> {
    // Chunk 2.6.1: with a relay configured, the whitelist is the runner's
    // own channel ROSTER — read fresh per call (a revoke lands without a
    // restart; same per-call freshness the package path gave). The roster
    // is relay-signed; any query/verification error FAILS CLOSED (no
    // grants), like an unreadable package.
    if let Some(url) = relay_url {
        let secret = identity.secret_seed();
        // serve() fail-fasts, but current_grants stays defensive: no anchor
        // -> no whitelist -> deny (never accept an unanchored roster).
        let Some(relay_key) = relay_pubkey else {
            tracing::warn!("relay whitelist requested without --relay-pubkey — failing closed");
            return Vec::new();
        };
        return match freehold_core::relay_http::query_channel_roster(
            url,
            relay_key,
            &identity.nostr_pubkey_hex(),
            &secret,
        ) {
            Ok(members) => members,
            Err(e) => {
                tracing::warn!(error = %e, relay = %url,
                    "grant check: relay unreachable/failed — failing closed (empty grants)");
                Vec::new()
            }
        };
    }

    // No relay: the shipped package grants are the source of record.
    // The runner can boot WHILE `control-plane provision` is still writing
    // the package (temp + atomic rename). Retrying a few times means a
    // just-shipped package is not denied with a misleading "not granted";
    // still fail closed (empty grants) if it genuinely never appears.
    for attempt in 0..5 {
        match freehold_core::secrets::SecretPackage::load(state_dir) {
            Ok(p) => return p.grants,
            Err(e) if attempt < 4 => {
                tracing::warn!(error = %e, dir = %state_dir.display(), attempt,
                    "grant check: package not visible yet — retrying");
                std::thread::sleep(std::time::Duration::from_millis(100));
            }
            Err(e) => {
                tracing::warn!(error = %e, dir = %state_dir.display(),
                    "grant check: could not load the shipped package — failing closed");
                return Vec::new();
            }
        }
    }
    Vec::new()
}

/// Testkit helper: provision a runner's channel on the fake relay with the
/// console key (create the channel + member the runner + the granted
/// agents) — the Chunk 2.6.1 fixture replacing the old publish_grants.
#[cfg(test)]
fn seed_runner_channel(
    relay_url: &str,
    console_secret: &[u8; 32],
    runner_pk: &str,
    members: &[&str],
) {
    freehold_core::relay_http::create_runner_channel(
        relay_url,
        console_secret,
        runner_pk,
        "runner",
    )
    .expect("create channel");
    freehold_core::relay_http::put_user(relay_url, console_secret, runner_pk, runner_pk)
        .expect("member runner");
    for m in members {
        freehold_core::relay_http::put_user(relay_url, console_secret, runner_pk, m)
            .expect("member grant");
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
            "name": "upload",
            "description": "Upload a LOCAL file (on the runner's machine) to `remote_path`                             on the target via sftp over the pooled connection — raw binary                             streaming. Returns the uploaded byte count.",
            "inputSchema": { "type": "object", "properties": {
                "target": { "type": "string" },
                "local_path": { "type": "string" },
                "remote_path": { "type": "string" }
            }, "required": ["target", "local_path", "remote_path"] }
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
    use std::collections::BTreeMap;

    #[test]
    fn contract_tools_present() {
        let expected = ["list", "exec", "upload", "config", "status", "snapshot"];
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

    /// Sign + POST an MCP tools/call (`list`) as `agent` against the runner.
    fn mcp_list(url: &str, agent: &Identity, runner_pk: &str) -> Result<(), String> {
        let body = json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "tools/call",
            "params": { "name": "list", "arguments": {} }
        });
        let raw = body.to_string();
        let ts = freehold_core::auth::now_secs();
        let ev = freehold_core::auth::sign_body(&agent.secret_seed(), runner_pk, ts, &raw);
        let mut resp = ureq::post(url)
            .header("Content-Type", "application/json")
            .header(auth::PUBKEY_HEADER, agent.nostr_pubkey_hex())
            .header(auth::SIG_HEADER, ev.sig)
            .header(auth::TS_HEADER, ts)
            .send(raw.clone())
            .map_err(|e| e.to_string())?;
        let v: Value = resp
            .body_mut()
            .read_json()
            .map_err(|e| format!("read: {e}"))?;
        if let Some(err) = v.get("error") {
            return Err(err["message"]
                .as_str()
                .unwrap_or("(no message)")
                .to_string());
        }
        Ok(())
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn grants_are_read_live_from_relay_and_revoke_lands_without_restart() {
        use freehold_testkit::relay as fake_relay;
        let (relay_url, _state, relay_task) = fake_relay::spawn().await;

        let rid = Identity::generate();
        let runner_pk = rid.nostr_pubkey_hex();
        let console_secret = [9u8; 32];
        let a = Identity::generate();
        let b = Identity::generate();
        let c = Identity::generate();
        let (ap, bp, _cp) = (
            a.nostr_pubkey_hex(),
            b.nostr_pubkey_hex(),
            c.nostr_pubkey_hex(),
        );
        // Chunk 2.6.1: grants ARE channel membership. Seed the runner's
        // channel: the console creates it, members the runner + A + B.
        seed_runner_channel(&relay_url, &console_secret, &runner_pk, &[&ap, &bp]);

        // A ROGUE agent cannot grant THEMSELVES: membership commands are
        // owner-gated at the relay (and rosters are relay-signed — the
        // runner's local verification ignores anything else). The fake
        // enforces the write gate; the runner enforces the read gate.
        let rogue = Identity::generate();
        let rogue_pk = rogue.nostr_pubkey_hex();
        assert!(
            freehold_core::relay_http::put_user(
                &relay_url,
                &rogue.secret_seed(),
                &runner_pk,
                &rogue_pk,
            )
            .is_err(),
            "relay must refuse a non-owner put-user"
        );

        let dir = tempfile::tempdir().unwrap();
        let ctx = RunnerContext {
            identity: rid.clone(),
            package: SecretPackage::default(),
            state_dir: dir.path().to_path_buf(),
            relay_url: Some(relay_url.clone()),
            relay_pubkey: Some(fake_relay::relay_pubkey()),
        };
        let (addr, server) = serve("127.0.0.1:0", ctx).await.unwrap();
        let url = format!("http://{addr}/mcp");

        mcp_list(&url, &a, &runner_pk).expect("A is granted");
        mcp_list(&url, &b, &runner_pk).expect("B is granted");
        // never-granted agent denied (and the rogue's newer-but-unanchored
        // event must NOT have granted it)
        let err = mcp_list(&url, &c, &runner_pk).unwrap_err();
        assert!(err.contains("unauthorized"), "{err}");
        let err = mcp_list(&url, &rogue, &runner_pk).unwrap_err();
        assert!(
            err.contains("unauthorized"),
            "rogue-author grant list must be ignored: {err}"
        );

        // REVOKE B: remove B from the channel — the runner re-reads its
        // roster per call, so B is denied WITHOUT any restart.
        freehold_core::relay_http::remove_user(&relay_url, &console_secret, &runner_pk, &bp)
            .expect("revoke B");
        let err = mcp_list(&url, &b, &runner_pk).unwrap_err();
        assert!(err.contains("unauthorized"), "B revoked live: {err}");
        // A still granted after the replacement
        mcp_list(&url, &a, &runner_pk).expect("A still granted");

        server.abort();
        relay_task.abort();
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn relay_unreachable_fails_closed_to_no_grants() {
        let rid = Identity::generate();
        let runner_pk = rid.nostr_pubkey_hex();
        let a = Identity::generate();
        let dir = tempfile::tempdir().unwrap();
        let ctx = RunnerContext {
            identity: rid,
            package: SecretPackage::default(),
            state_dir: dir.path().to_path_buf(),
            // nothing listens on :1 — the grant query must fail closed
            relay_url: Some("http://127.0.0.1:1".into()),
            relay_pubkey: Some("a".repeat(64)),
        };
        let (addr, server) = serve("127.0.0.1:0", ctx).await.unwrap();
        let url = format!("http://{addr}/mcp");
        let err = mcp_list(&url, &a, &runner_pk).unwrap_err();
        assert!(
            err.contains("unauthorized"),
            "relay outage must deny (fail closed): {err}"
        );
        server.abort();
    }

    // C0: a litellm runner carries THREE named secrets (target credential =
    // the master key, plus provider-key and postgres-pw). An exec may
    // request them by name; require_target_credential must accept the
    // extras (they EXIST in the package) and handle_api_exec must inject
    // each under its env name and redact every value from output.
    #[tokio::test(flavor = "multi_thread")]
    async fn exec_injects_and_redacts_extra_named_secrets() {
        let rid = Identity::generate();
        let runner_pk = rid.nostr_pubkey_hex();
        let a = Identity::generate();
        let grant_pk = a.nostr_pubkey_hex();
        let dir = tempfile::tempdir().unwrap();

        // The fake API target: echoes env values so the test can assert
        // exactly what was injected. It prints the PROXY value so redaction
        // is observable in the runner's response.
        // Pick a free port: bind, note the address, then serve on it.
        let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = l.local_addr().unwrap();
        let api_url = format!("http://{addr}");
        let echo = tokio::task::spawn(async move {
            let (mut sock, _) = l.accept().await.unwrap();
            use tokio::io::{AsyncReadExt, AsyncWriteExt};
            let mut buf = vec![0u8; 4096];
            let n = sock.read(&mut buf).await.unwrap_or(0);
            let req = String::from_utf8_lossy(&buf[..n]).to_string();
            // Respond with the env values that were visible to the child.
            let body = format!(
                "seen master={} provider={} pg={}",
                std::env::var("LITELLM").unwrap_or_default(),
                std::env::var("PROVIDER_KEY").unwrap_or_default(),
                std::env::var("POSTGRES_PW").unwrap_or_default(),
            );
            let resp = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: {}\r\n\r\n{}",
                body.len(),
                body
            );
            let _ = sock.write_all(resp.as_bytes()).await;
            let _ = req;
        });

        // Build the package: the target credential + two extras, sealed to
        // the RUNNER's own identity (the one serve() loads).
        let enc_pub = hex::decode(rid.enc_pubkey_hex()).unwrap();
        let mut enc_arr = [0u8; 32];
        enc_arr.copy_from_slice(&enc_pub);
        let seal_hex = |name: &str, val: &[u8]| -> String {
            hex::encode(freehold_core::crypto::seal(&enc_arr, name.as_bytes(), val).unwrap())
        };
        let pkg = SecretPackage {
            secrets: BTreeMap::from([
                ("litellm".to_string(), seal_hex("litellm", b"MASTER-SECRET")),
                (
                    "provider-key".to_string(),
                    seal_hex("provider-key", b"fw_PROVIDER"),
                ),
                ("postgres-pw".to_string(), seal_hex("postgres-pw", b"PG-PW")),
            ]),
            targets: BTreeMap::from([(
                "litellm".to_string(),
                freehold_core::secrets::TargetMeta {
                    kind: "litellm".into(),
                    address: api_url.clone(),
                    secret: "litellm".into(),
                },
            )]),
            grants: vec![grant_pk.clone()],
        };
        pkg.write_to_dir(dir.path()).unwrap();

        let ctx = RunnerContext {
            identity: rid,
            package: pkg,
            state_dir: dir.path().to_path_buf(),
            relay_url: None,
            relay_pubkey: None,
        };
        let (addr, server) = serve("127.0.0.1:0", ctx).await.unwrap();
        let url = format!("http://{addr}/mcp");

        // Sign + exec: request ALL THREE names. litellm_flavor uses curl, so
        // the command itself prints the env values the runner injected.
        let body = json!({
            "jsonrpc": "2.0", "id": 1, "method": "tools/call",
            "params": { "name": "exec", "arguments": {
                "cmd": "echo MASTER=$LITELLM PROVIDER=$PROVIDER_KEY PG=$POSTGRES_PW",
                "target": "litellm",
                "secrets": ["litellm", "provider-key", "postgres-pw"]
            } }
        });
        let raw = body.to_string();
        let ts = freehold_core::auth::now_secs();
        let ev = freehold_core::auth::sign_body(&a.secret_seed(), &runner_pk, ts, &raw);
        let mut resp = ureq::post(&url)
            .header("Content-Type", "application/json")
            .header(auth::PUBKEY_HEADER, &grant_pk)
            .header(auth::SIG_HEADER, ev.sig)
            .header(auth::TS_HEADER, ts)
            .send(raw.clone())
            .unwrap();
        let v: Value = resp.body_mut().read_json().unwrap();
        if let Some(err) = v.get("error") {
            panic!("exec failed: {}", err["message"]);
        }
        let text = v["result"]["content"][0]["text"].as_str().unwrap_or("");
        assert!(
            !text.contains("MASTER-SECRET")
                && !text.contains("fw_PROVIDER")
                && !text.contains("PG-PW"),
            "injected values must be redacted from output: {text}"
        );
        // The env reached the child: every requested secret is present under
        // its env name, and every value is masked by redaction.
        assert!(
            text.contains("MASTER=***") && text.contains("PROVIDER=***") && text.contains("PG=***"),
            "each requested secret must be injected by name + redacted: {text}"
        );
        drop(echo);
        server.abort();
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

#[cfg(test)]
mod d5_tests {
    use super::*;

    /// A tracing writer capturing everything into a Vec, plus a handle to
    /// read it after the subscriber guard drops and the flush settles.
    struct SharedBuf(std::sync::Arc<std::sync::Mutex<Vec<u8>>>);

    impl std::io::Write for SharedBuf {
        fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(buf);
            Ok(buf.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    #[derive(Clone)]
    struct CapWriter(std::sync::Arc<std::sync::Mutex<Vec<u8>>>);

    impl<'a> tracing_subscriber::fmt::MakeWriter<'a> for CapWriter {
        type Writer = SharedBuf;
        fn make_writer(&'a self) -> Self::Writer {
            SharedBuf(self.0.clone())
        }
    }

    fn tracing_capture() -> (
        std::sync::Arc<std::sync::Mutex<Vec<u8>>>,
        impl FnOnce() -> Vec<u8>,
    ) {
        let buf = std::sync::Arc::new(std::sync::Mutex::new(Vec::new()));
        let w = buf.clone();
        (buf.clone(), move || std::mem::take(&mut *w.lock().unwrap()))
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn audit_events_publish_to_the_relay_and_stay_spooled() {
        use freehold_testkit::relay as fake_relay;
        let (relay_url, state, relay_task) = fake_relay::spawn().await;

        let rid = Identity::generate();
        let runner_pk = rid.nostr_pubkey_hex();
        let console_secret = [42u8; 32];
        let a = Identity::generate();
        let ap = a.nostr_pubkey_hex();
        seed_runner_channel(&relay_url, &console_secret, &runner_pk, &[&ap]);

        let dir = tempfile::tempdir().unwrap();
        let ctx = RunnerContext {
            identity: rid,
            package: SecretPackage::default(),
            state_dir: dir.path().to_path_buf(),
            relay_url: Some(relay_url.clone()),
            relay_pubkey: Some(fake_relay::relay_pubkey()),
        };
        let (addr, server) = serve("127.0.0.1:0", ctx).await.unwrap();
        let url = format!("http://{addr}/mcp");

        // Granted agent runs a LOCAL exec (no ssh fixture needed).
        let body = json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "tools/call",
            "params": { "name": "exec", "arguments": { "target": "local", "cmd": "echo d5-audit" } }
        });
        let raw = body.to_string();
        let ts = freehold_core::auth::now_secs();
        let ev = freehold_core::auth::sign_body(&a.secret_seed(), &runner_pk, ts, &raw);
        let resp = ureq::post(&url)
            .header("Content-Type", "application/json")
            .header(auth::PUBKEY_HEADER, ap.clone())
            .header(auth::SIG_HEADER, ev.sig)
            .header(auth::TS_HEADER, ts)
            .send(raw);
        assert!(resp.is_ok(), "exec must succeed: {resp:?}");
        // Let the DETACHED publish land.
        tokio::time::sleep(std::time::Duration::from_millis(600)).await;

        // The relay holds a kind-48001 event by the RUNNER with a valid
        // id + BIP-340 signature and the audited command in content.
        let events = state.events.lock().clone();
        let audit = events
            .iter()
            .find(|e| e["kind"].as_u64() == Some(48001))
            .expect("relay received the audit event");
        assert_eq!(
            audit["pubkey"].as_str().unwrap(),
            runner_pk,
            "runner-signed"
        );
        let audit_tags: Vec<Vec<String>> = audit["tags"]
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
        freehold_core::nip98::verify_event(
            audit["pubkey"].as_str().unwrap(),
            audit["created_at"].as_i64().unwrap(),
            48001,
            &audit_tags,
            audit["content"].as_str().unwrap(),
            audit["sig"].as_str().unwrap(),
        )
        .expect("relay copy verifies");
        assert!(
            audit["content"].as_str().unwrap().contains("d5-audit"),
            "content: {}",
            audit["content"]
        );
        assert_eq!(audit["tags"][0][0], "p");
        assert_eq!(audit["tags"][0][1], ap, "caller indexed as p-tag");

        // The LOCAL spool holds the SAME event.
        let spooled = std::fs::read_to_string(dir.path().join("audit.log")).unwrap();
        assert!(spooled.contains("d5-audit"), "spool: {spooled}");
        assert!(
            spooled.contains(audit["id"].as_str().unwrap()),
            "same event id spooled + published: {spooled}"
        );

        server.abort();
        relay_task.abort();
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn audit_degrades_to_spool_only_when_relay_events_fail() {
        use freehold_testkit::relay as fake_relay;
        // Relay UP (so the whitelist resolves — the roster is the grants
        // source), but POST /events is blocked: the audit publish degrades
        // to spool-only. The channel seed (writes) happens BEFORE the block.
        let (relay_url, state, relay_task) = fake_relay::spawn().await;

        let rid = Identity::generate();
        let runner_pk = rid.nostr_pubkey_hex();
        let console_secret = [43u8; 32];
        let a = Identity::generate();
        let ap = a.nostr_pubkey_hex();
        seed_runner_channel(&relay_url, &console_secret, &runner_pk, &[&ap]);
        *state.block_events.lock() = true;

        let dir = tempfile::tempdir().unwrap();
        let ctx = RunnerContext {
            identity: rid.clone(),
            package: SecretPackage::default(),
            state_dir: dir.path().to_path_buf(),
            relay_url: Some(relay_url.clone()),
            relay_pubkey: Some(fake_relay::relay_pubkey()),
        };
        let (addr, server) = serve("127.0.0.1:0", ctx).await.unwrap();
        let url = format!("http://{addr}/mcp");

        let body = json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "tools/call",
            "params": { "name": "exec", "arguments": { "target": "local", "cmd": "echo deg-spool" } }
        });
        let raw = body.to_string();
        let ts = freehold_core::auth::now_secs();
        let ev = freehold_core::auth::sign_body(&a.secret_seed(), &runner_pk, ts, &raw);
        let resp = ureq::post(&url)
            .header("Content-Type", "application/json")
            .header(auth::PUBKEY_HEADER, ap)
            .header(auth::SIG_HEADER, ev.sig)
            .header(auth::TS_HEADER, ts)
            .send(raw);
        assert!(resp.is_ok(), "exec succeeds with /events blocked: {resp:?}");
        tokio::time::sleep(std::time::Duration::from_millis(700)).await;
        // The EXEC's own DETACHED publish must already have hit the bridge:
        // at this point authed_callers holds the grants query + that publish
        // (recorded before the block check) — before we manufacture anything.
        let pre_block = state.authed_callers.lock().len();
        assert!(
            pre_block >= 2,
            "the exec's detached publish was attempted (grants query + events attempt): {:?}",
            state.authed_callers.lock().clone()
        );
        let captured = {
            // The surfaced rule, unit-level: report_audit_publish is sync and
            // emits its warn on THIS thread — captured directly.
            let (writer, handle) = tracing_capture();
            let guard = tracing::subscriber::set_default(
                tracing_subscriber::fmt()
                    .with_max_level(tracing::Level::WARN)
                    .with_writer(CapWriter(writer))
                    .finish(),
            );
            let runner_secret = rid.clone().secret_seed();
            crate::exec::report_audit_publish(
                &relay_url,
                &runner_secret,
                &serde_json::json!({ "kind": 48001 }).to_string(),
            );
            drop(guard);
            handle()
        };

        // The manufactured direct call is the ONLY addition beyond the
        // exec's own authenticated attempt.
        assert_eq!(
            state.authed_callers.lock().len(),
            pre_block + 1,
            "only the direct report_audit_publish added a caller: {:?}",
            state.authed_callers.lock().clone()
        );
        // ...and the failure was SURFACED, never silently swallowed.
        assert!(
            String::from_utf8_lossy(&captured).contains("audit relay publish failed"),
            "surfaced warn expected; captured: {}",
            String::from_utf8_lossy(&captured)
        );
        // The local spool is still authoritative — never silently dropped.
        let spooled = std::fs::read_to_string(dir.path().join("audit.log")).unwrap();
        assert!(spooled.contains("deg-spool"), "spool: {spooled}");
        // And NOTHING reached the relay's store (the blocked publish failed).
        let kinds: Vec<u64> = state
            .events
            .lock()
            .iter()
            .map(|e| e["kind"].as_u64().unwrap_or(0))
            .collect();
        assert!(
            !kinds.contains(&48001),
            "no audit event may reach a failing bridge (store has {kinds:?})"
        );

        server.abort();
        relay_task.abort();
    }
}
