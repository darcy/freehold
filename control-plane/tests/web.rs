//! Phase F (local web UI) integration tests.
//!
//! The console is exercised over real loopback HTTP: provision through the
//! UI (console auto-granted), glance at the overview (no plaintext, grants
//! read from the SHIPPED package), rotate / grant / revoke-grant / revoke,
//! live readiness against an in-process runner, and the DNS-rebinding guard.

use std::net::SocketAddr;
use std::sync::Arc;

use freehold_control_plane::console::Console;
use freehold_control_plane::state::StateStore;
use freehold_control_plane::web;
use freehold_core::identity::Identity;
use freehold_runner::mcp::{self, RunnerContext};
use freehold_runner::secrets::SecretPackage;
use serde_json::{Value, json};

const AGENT_B: &str = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

/// Boot the console server on an ephemeral loopback port. Returns the base
/// URL. Cleanup of the cwd-relative runner packages happens in each test.
async fn boot_web(base: &std::path::Path) -> (String, tokio::task::JoinHandle<()>) {
    let cp_dir = base.join("cp");
    let store = StateStore::open(&cp_dir).unwrap();
    let console = Console::load_or_create(&cp_dir).unwrap();
    let app = web::router(Arc::new(store), console);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr: SocketAddr = listener.local_addr().unwrap();
    let handle = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (format!("http://{addr}"), handle)
}

fn post(base: &str, path: &str, body: Value, origin: Option<&str>) -> (u16, Value) {
    let agent = ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .build(),
    );
    let mut req = agent
        .post(&format!("{base}{path}"))
        .header("Content-Type", "application/json");
    if let Some(o) = origin {
        req = req.header("Origin", o);
    }
    let resp = req.send(body.to_string()).unwrap();
    let status: u16 = resp.status().as_u16();
    let json = resp.into_body().read_json::<Value>().unwrap_or(json!({}));
    (status, json)
}

fn get_json(base: &str, path: &str, origin: Option<&str>) -> (u16, Value) {
    let agent = ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .build(),
    );
    let mut req = agent.get(&format!("{base}{path}"));
    if let Some(o) = origin {
        req = req.header("Origin", o);
    }
    let resp = req.call().unwrap();
    let status: u16 = resp.status().as_u16();
    let json = resp.into_body().read_json::<Value>().unwrap_or(Value::Null);
    (status, json)
}

/// Every overview response must never contain a credential value.
fn assert_no_plaintext(v: &Value, secrets: &[&str]) {
    let dump = v.to_string();
    for s in secrets {
        assert!(
            !dump.contains(s),
            "plaintext credential leaked into the API: {s}"
        );
    }
}

fn console_pubkey(base: &str) -> String {
    get_json(base, "/api/overview", None)
        .1
        .get("console_pubkey")
        .unwrap()
        .as_str()
        .unwrap()
        .to_string()
}

/// Provision helper: package lands under the TEST tempdir (never the crate
/// CWD), so a failing assert can't poison the next run.
fn provision(
    base: &tempfile::TempDir,
    url: &str,
    name: &str,
    kind: &str,
    address: &str,
    secret: &str,
) {
    let runner_dir = base.path().join("runner").join(name);
    let (status, res) = post(
        url,
        "/api/provision",
        json!({
            "name": name,
            "kind": kind,
            "address": address,
            "secret": secret,
            "runner_dir": runner_dir.to_string_lossy(),
        }),
        None,
    );
    assert_eq!(status, 200, "{res}");
}

#[tokio::test(flavor = "multi_thread")]
async fn provision_rotate_grant_revoke_lifecycle() {
    let base = tempfile::tempdir().unwrap();
    let (url, server) = boot_web(base.path()).await;
    let console_pk = console_pubkey(&url);

    // ---- provision via the UI: console is auto-granted --------------------
    provision(&base, &url, "pg", "ssh", "10.0.0.5", "sekrit-pg-99");
    let pkg_dir = base.path().join("runner").join("pg");

    let (_, ov) = get_json(&url, "/api/overview", None);
    assert_no_plaintext(&ov, &["sekrit-pg-99"]);
    let pg = ov["runners"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["name"] == "pg")
        .unwrap();
    assert_eq!(pg["status"], "active");
    assert_eq!(pg["secret"]["kind"], "ssh");
    assert_eq!(pg["secret"]["address"], "10.0.0.5");
    assert_eq!(pg["grants"], json!([console_pk]));
    // The overview's grant list comes from the SHIPPED package: the runner
    // would authorize the console right now.
    let shipped = SecretPackage::load(&pkg_dir).unwrap();
    assert_eq!(shipped.grants, vec![console_pk.clone()]);

    // ---- rotate: new value, still never echoed ----------------------------
    let (status, _) = post(
        &url,
        "/api/rotate",
        json!({"name": "pg", "secret": "sekrit-pg-100"}),
        None,
    );
    assert_eq!(status, 200);
    let (_, ov2) = get_json(&url, "/api/overview", None);
    assert_no_plaintext(&ov2, &["sekrit-pg-99", "sekrit-pg-100"]);
    let pg2 = ov2["runners"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["name"] == "pg")
        .unwrap();
    assert!(pg2["secret"]["rotated_at"].is_number());

    // ---- grant another agent, then revoke it ------------------------------
    let (status, res) = post(
        &url,
        "/api/grant",
        json!({"name": "pg", "pubkey": AGENT_B}),
        None,
    );
    assert_eq!(status, 200, "{res}");
    let (_, ov3) = get_json(&url, "/api/overview", None);
    let pg3 = ov3["runners"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["name"] == "pg")
        .unwrap();
    assert!(pg3["grants"].as_array().unwrap().contains(&json!(AGENT_B)));
    assert_eq!(
        SecretPackage::load(&pkg_dir).unwrap().grants,
        vec![console_pk, AGENT_B.to_string()]
    );

    let (status, _) = post(
        &url,
        "/api/revoke-grant",
        json!({"name": "pg", "pubkey": AGENT_B}),
        None,
    );
    assert_eq!(status, 200);
    let (_, ov4) = get_json(&url, "/api/overview", None);
    let pg4 = ov4["runners"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["name"] == "pg")
        .unwrap();
    assert!(!pg4["grants"].as_array().unwrap().contains(&json!(AGENT_B)));

    // ---- revoke: status flips, shipped secrets.json is removed ------------
    let (status, _) = post(&url, "/api/revoke", json!({"name": "pg"}), None);
    assert_eq!(status, 200);
    let (_, ov5) = get_json(&url, "/api/overview", None);
    let pg5 = ov5["runners"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["name"] == "pg")
        .unwrap();
    assert_eq!(pg5["status"], "revoked");
    // revoke deletes the shipped secrets.json on PURPOSE — grants must read
    // as null, not as an empty fail-closed list.
    assert!(
        pg5["grants"].is_null(),
        "revoked runner must report grants == null: {pg5}"
    );

    // Idempotency / guards: revoked runner refuses rotate.
    let (status, _) = post(
        &url,
        "/api/rotate",
        json!({"name": "pg", "secret": "nope"}),
        None,
    );
    assert_eq!(status, 409);

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn live_readiness_probe_via_console() {
    let base = tempfile::tempdir().unwrap();
    let (url, server) = boot_web(base.path()).await;

    provision(
        &base,
        &url,
        "pv",
        "vultr",
        "http://127.0.0.1:1",
        "vultr-key-7",
    );
    let pkg_dir = base.path().join("runner").join("pv");

    // Serve the shipped package in-process (the real runner code path), then
    // register its MCP addr in the UI.
    let runner_id = Identity::load(&pkg_dir).unwrap();
    let pkg = SecretPackage::load(&pkg_dir).unwrap();
    let ctx = RunnerContext {
        relay_url: None,
        grant_author: None,
        identity: runner_id,
        package: pkg,
        state_dir: pkg_dir.clone(),
    };
    let (raddr, rserver) = mcp::serve("127.0.0.1:0", ctx).await.unwrap();
    let (status, _) = post(
        &url,
        "/api/runner-addr",
        json!({"name": "pv", "addr": raddr.to_string()}),
        None,
    );
    assert_eq!(status, 200);

    // The console (auto-granted) polls the runner: local self-check green,
    // the dead-probe service target red/yellow — and the probe went through
    // the signed MCP channel, not a side door.
    let (_, ov) = get_json(&url, "/api/overview", None);
    let pv = ov["runners"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["name"] == "pv")
        .unwrap();
    let readiness = &pv["readiness"];
    assert_eq!(readiness["local"], "green", "console probe: {readiness}");
    let svc = readiness["pv"].as_str().unwrap_or("");
    assert!(
        svc.starts_with("red") || svc.starts_with("yellow"),
        "service target should read degraded against a dead probe, got: {readiness}"
    );

    rserver.abort();
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn ungranted_console_cannot_read_runner() {
    let base = tempfile::tempdir().unwrap();
    let (url, server) = boot_web(base.path()).await;

    // Provision via CLI semantics-equivalent: an agent granted to the runner
    // WITHOUT the console (simulate a pre-UI runner by stripping the grant).
    provision(
        &base,
        &url,
        "locked",
        "b2",
        "http://127.0.0.1:2",
        "b2-key-42",
    );
    let pkg_dir = base.path().join("runner").join("locked");
    let mut pkg = SecretPackage::load(&pkg_dir).unwrap();
    pkg.grants.clear();
    pkg.write_to_dir(&pkg_dir).unwrap();

    let runner_id = Identity::load(&pkg_dir).unwrap();
    let ctx = RunnerContext {
        relay_url: None,
        grant_author: None,
        identity: runner_id,
        package: pkg,
        state_dir: pkg_dir.clone(),
    };
    let (raddr, rserver) = mcp::serve("127.0.0.1:0", ctx).await.unwrap();
    let (status, _) = post(
        &url,
        "/api/runner-addr",
        json!({"name": "locked", "addr": raddr.to_string()}),
        None,
    );
    assert_eq!(status, 200);

    let (_, ov) = get_json(&url, "/api/overview", None);
    let locked = ov["runners"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["name"] == "locked")
        .unwrap();
    assert!(
        locked["readiness"]["note"]
            .as_str()
            .unwrap_or("")
            .contains("not granted"),
        "console must NOT read a runner it was never granted: {locked}"
    );

    rserver.abort();
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn non_loopback_origin_refused() {
    let base = tempfile::tempdir().unwrap();
    let (url, server) = boot_web(base.path()).await;

    provision(&base, &url, "x", "ssh", "1.2.3.4", "x");

    // A page served anywhere else must not be able to drive (or even READ)
    // the console: DNS-rebinding guard on reads and writes.
    let (status, _) = get_json(&url, "/api/overview", Some("http://evil.example"));
    assert_eq!(status, 403);
    let (status, _) = post(
        &url,
        "/api/revoke",
        json!({"name": "x"}),
        Some("http://evil.example"),
    );
    assert_eq!(status, 403);
    // Loopback origin (any port) is fine — INCLUDING a port-less one (a
    // browser sending `Origin: http://localhost` when the console serves :80
    // must not 403).
    let (status, _) = get_json(&url, "/api/overview", Some("http://127.0.0.1:9999"));
    assert_eq!(status, 200);
    let (status, _) = get_json(&url, "/api/overview", Some("http://localhost"));
    assert_eq!(status, 200);
    // Bracketed IPv6 loopback with port (serve --addr '[::1]:8080').
    let (status, _) = get_json(&url, "/api/overview", Some("http://[::1]:8080"));
    assert_eq!(status, 200);

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn admin_page_escapes_remote_readiness_text() {
    // The BLOCKING-fix scaffold: the page must escape every interpolated
    // value before innerHTML — readiness strings are remote-derived via
    // connector errors and are the XSS reach.
    let base = tempfile::tempdir().unwrap();
    let (url, server) = boot_web(base.path()).await;
    let agent = ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .build(),
    );
    let mut html = String::new();
    use std::io::Read;
    agent
        .get(&format!("{url}/"))
        .call()
        .unwrap()
        .into_body()
        .as_reader()
        .read_to_string(&mut html)
        .unwrap();
    assert!(
        html.contains("function esc("),
        "page must ship the escape helper"
    );
    assert!(
        html.contains("esc(t)") && html.contains("chip(s)"),
        "readiness keys/values render through esc()"
    );
    assert!(
        html.contains("esc(r.name)") && html.contains("esc(r.mcp_addr)"),
        "row interpolations escape before innerHTML"
    );
    // A revoked runner (secrets.json deleted on purpose) must NOT render the
    // 'package unreadable' chip — that signal is for ACTIVE runners only.
    assert!(
        html.contains("revoked — secrets.json removed (B3)"),
        "revoked rows render their own note, not the unreadable chip"
    );
    server.abort();
}
