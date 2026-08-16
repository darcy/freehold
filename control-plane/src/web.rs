//! Local admin/ops web surface (Phase F).
//!
//! F1 — services-at-a-glance: every runner with its status, secret, grants,
//! and LIVE readiness (the runner's OWN self-check) probed over the signed
//! MCP channel. F2 — manage runners / secrets / grants from the UI.
//!
//! Posture:
//! - Loopback only, and all routes check the `Origin` header (DNS-rebinding
//!   guard, same rule as the runner's MCP endpoint).
//! - The console agent signs every readiness probe — the runner still fails
//!   closed; an UNGRANTED console reads as "console not granted", never as a
//!   side door.
//! - Secrets arrive over loopback HTTP, are used immediately (sealed or
//!   re-sealed), and are zeroized — never stored, never logged, never echoed
//!   back. The overview shows secret NAMES and rotation stamps only.
//! - This is the admin/ops console, NOT chat. Buzz owns conversation.

use std::sync::Arc;
use std::time::Duration;

use axum::extract::{DefaultBodyLimit, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::{Html, IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use freehold_core::audit::SignedEvent;
use freehold_core::auth;
use freehold_core::secrets::SecretPackage;
use serde::Deserialize;
use serde_json::{Value, json};
use zeroize::Zeroizing;

use crate::console::Console;
use crate::provisioner::{self, ProvisionError, ProvisionRequest};
use crate::state::{RunnerStatus, StateStore};

/// Loopback origins allowed by the DNS-rebinding guard (scheme + host only;
/// any port is fine). The console never leaves the machine.
const LOOPBACK_ORIGINS: [&str; 3] = ["http://localhost", "http://127.0.0.1", "http://[::1]"];

/// Per-probe deadline: readiness is a glance, not a hang.
const PROBE_TIMEOUT: Duration = Duration::from_secs(4);

#[derive(Clone)]
struct WebState {
    store: Arc<StateStore>,
    console: Console,
}

pub fn router(store: StateStore, console: Console) -> Router {
    Router::new()
        .route("/", get(index))
        .route("/healthz", get(healthz))
        .route("/api/overview", get(overview))
        .route("/api/provision", post(provision))
        .route("/api/rotate", post(rotate))
        .route("/api/revoke", post(revoke))
        .route("/api/grant", post(grant))
        .route("/api/revoke-grant", post(revoke_grant))
        .route("/api/runner-addr", post(runner_addr))
        .layer(DefaultBodyLimit::max(256 * 1024))
        .with_state(WebState {
            store: Arc::new(store),
            console,
        })
}

// ---------------------------------------------------------------------------
// Guards + plumbing
// ---------------------------------------------------------------------------

/// DNS-rebinding guard: a page hosted anywhere else must not be able to drive
/// this loopback console. A missing Origin (curl, hand-rolled clients) is
/// allowed; a PRESENT non-loopback Origin is refused.
fn check_origin(headers: &HeaderMap) -> Result<(), Box<Response>> {
    let Some(origin) = headers.get("origin").and_then(|v| v.to_str().ok()) else {
        return Ok(());
    };
    // Strip the port for the comparison; the runner guard uses scheme+host.
    let without_port = origin
        .rsplit_once(':')
        .map(|(head, _port)| head.to_string())
        .unwrap_or_else(|| origin.to_string());
    if LOOPBACK_ORIGINS.iter().any(|l| *l == without_port) {
        return Ok(());
    }
    Err(Box::new(
        (
            StatusCode::FORBIDDEN,
            Json(json!({"error": "cross-origin request refused (console is loopback-only)"})),
        )
            .into_response(),
    ))
}

fn action_error(e: ProvisionError) -> (StatusCode, Json<Value>) {
    let (code, msg) = match e {
        ProvisionError::RunnerExists(_) | ProvisionError::RunnerRevoked(_) => {
            (StatusCode::CONFLICT, e.to_string())
        }
        ProvisionError::SecretNotFound(_) => (StatusCode::NOT_FOUND, e.to_string()),
        ProvisionError::InvalidName(_) | ProvisionError::InvalidGrant(_) => {
            (StatusCode::BAD_REQUEST, e.to_string())
        }
        other => (StatusCode::INTERNAL_SERVER_ERROR, other.to_string()),
    };
    (code, Json(json!({"error": msg})))
}

fn state_error(e: crate::state::StateError) -> (StatusCode, Json<Value>) {
    match e {
        crate::state::StateError::RunnerNotFound(_) => {
            (StatusCode::NOT_FOUND, Json(json!({"error": e.to_string()})))
        }
        other => (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(json!({"error": other.to_string()})),
        ),
    }
}

/// Sign a tools/call body as the console agent for `runner_pubkey`.
fn sign_call(console: &Console, runner_pubkey: &str, raw: &str) -> SignedEvent {
    // The console's signing secret: hex string -> bytes, wiped on drop.
    let secret_hex = console.identity.nostr_secret_hex();
    let mut secret = Zeroizing::new([0u8; 32]);
    let decoded = hex::decode(&secret_hex).expect("console identity secret is valid hex");
    secret.copy_from_slice(&decoded);
    auth::sign_body(&secret, runner_pubkey, auth::now_secs(), raw)
}

// ---------------------------------------------------------------------------
// Readiness probe: the console asks a RUNNING runner for its own self-check,
// through the same signed MCP channel an agent would use. Blocking HTTP lives
// here on purpose (ureq is a sync client); callers run it on a blocking task.
// ---------------------------------------------------------------------------

fn probe_targets(console: Console, runner_pubkey: &str, addr: &str) -> Value {
    let raw = r#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{}}}"#;
    let ev = sign_call(&console, runner_pubkey, raw);
    let url = if addr.contains("://") {
        addr.to_string()
    } else {
        format!("http://{addr}/mcp")
    };
    let agent = ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .timeout_global(Some(PROBE_TIMEOUT))
            .build(),
    );
    let resp = match agent
        .post(&url)
        .header(auth::PUBKEY_HEADER, ev.pubkey)
        .header(auth::SIG_HEADER, ev.sig)
        .header(auth::TS_HEADER, auth::now_secs().to_string())
        .send(raw)
    {
        Ok(r) => r,
        Err(e) => return json!({"error": format!("unreachable ({e})")}),
    };
    let body: Value = match resp.into_body().read_json::<Value>() {
        Ok(v) => v,
        Err(e) => return json!({"error": format!("bad response ({e})")}),
    };
    if let Some(err) = body.get("error") {
        let code = err.get("code").and_then(Value::as_i64).unwrap_or(-1);
        let msg = err
            .get("message")
            .and_then(Value::as_str)
            .unwrap_or("(no message)");
        if code == -32001 {
            return json!({"note": "console not granted — grant the console pubkey in the UI"});
        }
        return json!({"error": format!("{code}: {msg}")});
    }
    let text = body["result"]["content"][0]["text"].as_str().unwrap_or("");
    match serde_json::from_str::<Value>(text) {
        Ok(states) => states,
        Err(_) => json!({"error": "opaque status payload"}),
    }
}

/// Probe one runner's live readiness on a blocking task (ureq is sync).
/// Returns `(name, readiness)` so the collector can map probes back.
async fn probe_one(
    store: Arc<StateStore>,
    console: Console,
    name: String,
) -> Option<(String, Value)> {
    let rec = store.get_runner(&name)?;
    let addr = rec.mcp_addr.clone()?;
    let pubkey = rec.nostr_pubkey.clone();
    let value = tokio::task::spawn_blocking(move || probe_targets(console, &pubkey, &addr))
        .await
        .unwrap_or_else(|_| json!({"error": "probe task panicked"}));
    Some((name, value))
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

async fn index() -> Html<&'static str> {
    Html(INDEX_HTML)
}

async fn healthz() -> &'static str {
    "ok"
}

async fn overview(
    State(state): State<WebState>,
    headers: HeaderMap,
) -> Result<Json<Value>, Response> {
    check_origin(&headers).map_err(|b| *b)?;
    let snapshot = state.store.snapshot();
    let console_pk = state.console.pubkey();

    // Fire one probe per ACTIVE runner with an mcp_addr registered, then
    // collect. Probes are independent — parallel.
    let mut probes = tokio::task::JoinSet::new();
    for (name, rec) in &snapshot.runners {
        if rec.status == RunnerStatus::Active && rec.mcp_addr.is_some() {
            probes.spawn(probe_one(
                state.store.clone(),
                state.console.clone(),
                name.clone(),
            ));
        }
    }

    let mut runners = Vec::with_capacity(snapshot.runners.len());
    for (name, rec) in &snapshot.runners {
        let secret = snapshot.secrets.get(name);
        let grants = SecretPackage::load(&rec.package_dir)
            .map(|p| p.grants)
            .unwrap_or_default();
        runners.push(json!({
            "name": name,
            "status": match rec.status { RunnerStatus::Active => "active", RunnerStatus::Revoked => "revoked" },
            "nostr_pubkey": rec.nostr_pubkey,
            "enc_pubkey": rec.enc_pubkey,
            "mcp_addr": rec.mcp_addr,
            "secret": secret.map(|s| json!({
                "name": s.runner,
                "kind": s.kind,
                "address": s.address,
                "rotated_at": s.rotated_at,
                "created_at": s.created_at,
            })),
            "grants": grants,
        }));
    }
    let mut by_name: std::collections::HashMap<String, Value> = std::collections::HashMap::new();
    while let Some(res) = probes.join_next().await {
        if let Some((name, probe)) = res.ok().flatten() {
            by_name.insert(name, probe);
        }
    }
    for r in &mut runners {
        if let Some(probe) = by_name.get(r["name"].as_str().unwrap_or("")) {
            r["readiness"] = probe.clone();
        }
    }

    Ok(Json(json!({
        "console_pubkey": console_pk,
        "runners": runners,
    })))
}

#[derive(Deserialize)]
struct ProvisionReq {
    name: String,
    kind: String,
    address: String,
    secret: String,
}

async fn provision(
    State(state): State<WebState>,
    headers: HeaderMap,
    Json(req): Json<ProvisionReq>,
) -> Result<Json<Value>, Response> {
    check_origin(&headers).map_err(|b| *b)?;
    // The console grants ITSELF: the UI can only ever ask the runner what the
    // operator can also ask from the CLI. The runner still fails closed for
    // every pubkey NOT on the list.
    let console_pk = state.console.pubkey();
    let secret = Zeroizing::new(req.secret);
    let runner_dir = std::path::PathBuf::from(format!("./.freehold/runner/{}", req.name));
    let res = provisioner::provision_runner(
        &state.store,
        &ProvisionRequest {
            name: &req.name,
            kind: &req.kind,
            address: &req.address,
            secret: secret.as_bytes(),
            runner_dir: &runner_dir,
            grants: std::slice::from_ref(&console_pk),
        },
    )
    .map_err(|e| action_error(e).into_response())?;
    Ok(Json(json!({
        "ok": true,
        "name": res.name,
        "nostr_pubkey": res.nostr_pubkey,
        "enc_pubkey": res.enc_pubkey,
        "package_dir": res.package_dir,
        "granted": [console_pk],
    })))
}

#[derive(Deserialize)]
struct SecretReq {
    name: String,
    secret: String,
}

async fn rotate(
    State(state): State<WebState>,
    headers: HeaderMap,
    Json(req): Json<SecretReq>,
) -> Result<Json<Value>, Response> {
    check_origin(&headers).map_err(|b| *b)?;
    let secret = Zeroizing::new(req.secret);
    provisioner::rotate_secret(&state.store, &req.name, secret.as_bytes())
        .map_err(|e| action_error(e).into_response())?;
    Ok(Json(json!({"ok": true, "name": req.name})))
}

#[derive(Deserialize)]
struct NameReq {
    name: String,
}

async fn revoke(
    State(state): State<WebState>,
    headers: HeaderMap,
    Json(req): Json<NameReq>,
) -> Result<Json<Value>, Response> {
    check_origin(&headers).map_err(|b| *b)?;
    provisioner::revoke_runner(&state.store, &req.name)
        .map_err(|e| action_error(e).into_response())?;
    Ok(Json(json!({"ok": true, "name": req.name})))
}

#[derive(Deserialize)]
struct GrantReq {
    name: String,
    pubkey: String,
}

async fn grant(
    State(state): State<WebState>,
    headers: HeaderMap,
    Json(req): Json<GrantReq>,
) -> Result<Json<Value>, Response> {
    check_origin(&headers).map_err(|b| *b)?;
    let grants = provisioner::grant_agent(&state.store, &req.name, &req.pubkey)
        .map_err(|e| action_error(e).into_response())?;
    Ok(Json(
        json!({"ok": true, "name": req.name, "granted": grants}),
    ))
}

async fn revoke_grant(
    State(state): State<WebState>,
    headers: HeaderMap,
    Json(req): Json<GrantReq>,
) -> Result<Json<Value>, Response> {
    check_origin(&headers).map_err(|b| *b)?;
    let grants = provisioner::revoke_grant(&state.store, &req.name, &req.pubkey)
        .map_err(|e| action_error(e).into_response())?;
    Ok(Json(
        json!({"ok": true, "name": req.name, "granted": grants}),
    ))
}

#[derive(Deserialize)]
struct AddrReq {
    name: String,
    addr: String,
}

async fn runner_addr(
    State(state): State<WebState>,
    headers: HeaderMap,
    Json(req): Json<AddrReq>,
) -> Result<Json<Value>, Response> {
    check_origin(&headers).map_err(|b| *b)?;
    state
        .store
        .set_runner_mcp_addr(&req.name, Some(req.addr))
        .map_err(|e| state_error(e).into_response())?;
    state
        .store
        .save()
        .map_err(|e| state_error(e).into_response())?;
    Ok(Json(json!({"ok": true, "name": req.name})))
}

// ---------------------------------------------------------------------------
// The page: services-at-a-glance + manage. No build step, no framework — a
// single self-contained HTML file the console serves.
// ---------------------------------------------------------------------------

const INDEX_HTML: &str = r##"<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>freehold · control plane</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body { margin: 0; font-family: ui-monospace, "SFMono-Regular", Menlo, monospace;
         background: #0d1117; color: #c9d1d9; padding: 24px; }
  h1 { font-size: 18px; letter-spacing: 2px; color: #e6edf3; }
  h1 span { color: #58a6ff; }
  .muted { color: #8b949e; font-size: 12px; }
  table { border-collapse: collapse; width: 100%; margin-top: 16px; }
  th, td { text-align: left; padding: 8px 10px; border-bottom: 1px solid #21262d;
           vertical-align: top; font-size: 13px; }
  th { color: #8b949e; font-weight: 600; text-transform: uppercase; font-size: 11px; }
  input, textarea, select { background: #161b22; border: 1px solid #30363d; color: #c9d1d9;
         border-radius: 6px; padding: 6px 8px; font: inherit; width: 100%; }
  button { background: #21262d; border: 1px solid #30363d; color: #c9d1d9;
         border-radius: 6px; padding: 6px 12px; font: inherit; cursor: pointer; }
  button:hover { background: #30363d; }
  button.danger { background: #3d1d1d; border-color: #6b2a2a; color: #ffa198; }
  .chip { display: inline-block; padding: 2px 8px; border-radius: 999px; font-size: 11px;
          border: 1px solid #30363d; }
  .green  { background: #0e4429; color: #7ee787; }
  .yellow { background: #5d4b0f; color: #f2cc60; }
  .red    { background: #521c1c; color: #ff7b72; }
  .gray   { background: #30363d; color: #8b949e; }
  .card { background: #161b22; border: 1px solid #21262d; border-radius: 8px; padding: 16px;
          margin-top: 16px; }
  .form-grid { display: grid; grid-template-columns: 1fr 1fr 2fr 1fr; gap: 8px; }
  .row { display: flex; gap: 8px; align-items: center; }
  .small { font-size: 11px; color: #8b949e; }
  #err { color: #ff7b72; }
</style>
</head>
<body>
<h1>freehold <span>·</span> control plane</h1>
<div class="muted" id="console-line">console agent: loading…</div>

<div class="card">
  <form id="provision-form" class="form-grid">
    <input name="name" placeholder="runner/service name" required>
    <input name="kind" placeholder="kind (ssh|vultr|b2)" required>
    <input name="address" placeholder="address" required>
    <button type="submit">provision</button>
    <input name="secret" placeholder="credential (pasted once, sealed, never stored)" required
           style="grid-column: 1 / -1">
  </form>
</div>

<table>
<thead><tr><th>runner</th><th>status</th><th>secret</th><th>readiness</th><th>grants</th><th></th></tr></thead>
<tbody id="rows"></tbody>
</table>

<div id="err" class="muted"></div>

<script>
const $ = (s) => document.querySelector(s);

async function api(path, body) {
  const r = await fetch(path, {
    method: body ? "POST" : "GET",
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
  return j;
}

function chip(state) {
  let cls = "gray";
  if (typeof state === "string") {
    if (state.startsWith("green")) cls = "green";
    else if (state.startsWith("yellow")) cls = "yellow";
    else if (state.startsWith("red")) cls = "red";
  }
  return '<span class="chip ' + cls + '">' + state + "</span>";
}

async function refresh() {
  const o = await api("/api/overview");
  $("#console-line").textContent = "console agent: " + o.console_pubkey;
  const rows = o.runners.map((r) => {
    const readiness = r.readiness
      ? Object.entries(r.readiness).map(([t, s]) => t + " " + chip(s)).join(" ")
      : '<span class="muted">—</span>';
    const grants = r.grants.length
      ? r.grants.map((g) => '<div class="muted">' + g + "</div>").join("")
      : '<span class="muted">nobody (fail closed)</span>';
    const secret = r.secret
      ? r.secret.name + " · " + r.secret.kind + " · " + r.secret.address +
        (r.secret.rotated_at ? " · rotated" : "")
      : '<span class="muted">—</span>';
    return `<tr>
      <td><b>${r.name}</b><div class="muted">${r.nostr_pubkey.slice(0, 16)}…</div></td>
      <td>${r.status === "revoked" ? chip("red(revoked)") : chip("green(active)")}</td>
      <td>${secret}</td>
      <td>${readiness}</td>
      <td>${grants}</td>
      <td>
        <div class="row">
          <input class="mcp" placeholder="mcp addr" value="${r.mcp_addr || ""}" data-name="${r.name}">
          <button data-act="addr" data-name="${r.name}" data-nostr="${r.nostr_pubkey}">set addr</button>
          <button data-act="rotate" data-name="${r.name}">rotate</button>
          <button data-act="grant" data-name="${r.name}">grant</button>
          <button data-act="ungrant" data-name="${r.name}">ungrant</button>
          ${r.status === "revoked" ? "" : '<button class="danger" data-act="revoke" data-name="' + r.name + '">revoke</button>'}
        </div>
      </td>
    </tr>`;
  }).join("");
  $("#rows").innerHTML = rows || '<tr><td colspan="6" class="muted">no runners yet — provision one above</td></tr>';
  bindActions();
}

async function act(kind, payload) {
  const path = { rotate: "/api/rotate", grant: "/api/grant", ungrant: "/api/revoke-grant",
                 revoke: "/api/revoke", addr: "/api/runner-addr" }[kind];
  await api(path, payload);
  await refresh();
}

function bindActions() {
  $("#rows").querySelectorAll("button").forEach((b) => {
    b.onclick = async () => {
      try {
        const name = b.dataset.name;
        if (b.dataset.act === "revoke" && !confirm("revoke " + name + "? removes its credential capability")) return;
        if (b.dataset.act === "grant") {
          const pk = prompt("agent pubkey to grant (64 hex)");
          if (!pk) return;
          await act("grant", { name, pubkey: pk });
        } else if (b.dataset.act === "ungrant") {
          const pk = prompt("agent pubkey to remove from grants (64 hex)");
          if (!pk) return;
          await act("ungrant", { name, pubkey: pk });
        } else if (b.dataset.act === "rotate") {
          const secret = prompt("NEW credential for " + name);
          if (!secret) return;
          await act("rotate", { name, secret });
        } else if (b.dataset.act === "addr") {
          const inp = b.parentElement.querySelector(".mcp");
          await act("addr", { name, addr: inp.value.trim() });
        }
      } catch (e) { $("#err").textContent = "error: " + e.message; }
    };
  });
}

$("#provision-form").onsubmit = async (ev) => {
  ev.preventDefault();
  const f = new FormData(ev.target);
  try {
    await api("/api/provision", {
      name: f.get("name"), kind: f.get("kind"), address: f.get("address"), secret: f.get("secret"),
    });
    ev.target.reset();
    await refresh();
  } catch (e) { $("#err").textContent = "error: " + e.message; }
};

refresh().catch((e) => { $("#err").textContent = "error: " + e.message; });
</script>
</body>
</html>"##;
