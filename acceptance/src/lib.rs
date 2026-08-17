//! Chunk 1 acceptance script (Phase G).
//!
//! Reproduces the POC acceptance criteria end-to-end with the REAL crates:
//! the control-plane provisioner, a live in-process runner (the same code
//! `runner serve` runs), the web console, and the three connectors against
//! hermetic loopback fixtures (mock Vultr/B2 APIs + an in-process sshd).
//! No real network, no external dependency — deterministic in CI.
//!
//! G1 — the algolia-style happy path: existing service -> runner -> self
//!      check green -> grant -> the console's green view.
//! G2 — SSH exec, Vultr create/destroy, B2 read/write — ALL via the runner,
//!      with secrets resolved by the runner from CP-provisioned ciphertext.
//! G3 — secrets never in agent context; ciphertext-only on disk + the
//!      runner's own injected key; NO master key anywhere; revoking
//!      membership cuts the runner off; rotation re-encrypts.

use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use freehold_control_plane::console::Console;
use freehold_control_plane::provisioner::{self, ProvisionRequest};
use freehold_control_plane::state::StateStore;
use freehold_control_plane::web;
use freehold_core::audit::SignedEvent;
use freehold_core::auth;
use freehold_core::identity::Identity;
use freehold_core::secrets::SecretPackage;
use freehold_runner::mcp::{self, RunnerContext};
use freehold_testkit::mock::{self, B2_CRED, B2State, VULTR_TOKEN, VultrState};
use freehold_testkit::sshd;
use serde_json::{Value, json};
use zeroize::Zeroizing;

pub struct Check {
    pub id: &'static str,
    pub description: &'static str,
    pub ok: bool,
    pub detail: Option<String>,
}

impl Check {
    fn pass(id: &'static str, description: &'static str, detail: impl Into<String>) -> Self {
        Self {
            id,
            description,
            ok: true,
            detail: Some(detail.into()),
        }
    }
    fn fail(id: &'static str, description: &'static str, detail: impl Into<String>) -> Self {
        Self {
            id,
            description,
            ok: false,
            detail: Some(detail.into()),
        }
    }
}

type R<T> = Result<T, String>;

// ---------------------------------------------------------------------------
// Agent + signed MCP client (the orchestrator's wire contract: core::auth
// signer + ureq).
// ---------------------------------------------------------------------------

struct Agent {
    secret: Zeroizing<[u8; 32]>,
    pubkey: String,
}

impl Agent {
    fn generate() -> Self {
        let id = Identity::generate();
        let hexsec = Zeroizing::new(id.nostr_secret_hex());
        let decoded = Zeroizing::new(hex::decode(&*hexsec).expect("valid hex"));
        let mut secret = Zeroizing::new([0u8; 32]);
        secret.copy_from_slice(&decoded);
        Self {
            secret,
            pubkey: id.nostr_pubkey_hex(),
        }
    }
}

/// Sign + send a tools/call; returns the full JSON-RPC response.
fn call(agent: &Agent, runner_pubkey: &str, url: &str, name: &str, arguments: Value) -> R<Value> {
    let body = json!({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": { "name": name, "arguments": arguments }
    });
    let raw = body.to_string();
    let ts = auth::now_secs();
    let event: SignedEvent = auth::sign_body(&agent.secret, runner_pubkey, ts, &raw);
    let agent_http = ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .timeout_global(Some(Duration::from_secs(20)))
            .build(),
    );
    let resp = agent_http
        .post(url)
        .header("Content-Type", "application/json")
        .header(auth::PUBKEY_HEADER, event.pubkey)
        .header(auth::SIG_HEADER, event.sig)
        .header(auth::TS_HEADER, ts.to_string())
        .send(raw.as_str())
        .map_err(|e| format!("http: {e}"))?;
    resp.into_body()
        .read_json::<Value>()
        .map_err(|e| format!("bad response: {e}"))
}

/// Run exec; returns the text payload on success, Err on rpc/tool failure.
fn exec(
    agent: &Agent,
    runner_pubkey: &str,
    url: &str,
    target: &str,
    cmd: &str,
    secrets: &[&str],
) -> R<String> {
    let resp = call(
        agent,
        runner_pubkey,
        url,
        "exec",
        json!({ "cmd": cmd, "target": target, "secrets": secrets, "timeout_s": 20 }),
    )?;
    if let Some(e) = resp.get("error") {
        return Err(format!("rpc error: {e}"));
    }
    if resp["result"].get("isError").and_then(Value::as_bool) == Some(true) {
        let text = resp["result"]["content"][0]["text"].as_str().unwrap_or("");
        return Err(format!("tool error: {text}"));
    }
    resp["result"]["content"][0]["text"]
        .as_str()
        .map(str::to_string)
        .ok_or_else(|| "missing text content".into())
}

/// The runner's OWN self-check per target.
fn readiness(agent: &Agent, runner_pubkey: &str, url: &str) -> R<Value> {
    let resp = call(agent, runner_pubkey, url, "status", json!({}))?;
    if let Some(e) = resp.get("error") {
        return Err(format!("rpc error: {e}"));
    }
    let text = resp["result"]["content"][0]["text"]
        .as_str()
        .ok_or("missing status text")?;
    serde_json::from_str(text).map_err(|e| format!("status parse: {e}"))
}

/// Boot a runner serving a provisioned package; returns (mcp url, task).
async fn serve_runner(pkg_dir: &Path) -> R<(String, tokio::task::JoinHandle<()>)> {
    let id = Identity::load(pkg_dir).map_err(|e| e.to_string())?;
    let pkg = SecretPackage::load(pkg_dir).map_err(|e| e.to_string())?;
    let ctx = RunnerContext {
        relay_url: None,
        identity: id,
        package: pkg,
        state_dir: pkg_dir.to_path_buf(),
    };
    let (addr, server) = mcp::serve("127.0.0.1:0", ctx)
        .await
        .map_err(|e| e.to_string())?;
    Ok((format!("http://{addr}/mcp"), server))
}

async fn boot_console(
    store: Arc<StateStore>,
    console: Console,
) -> (String, tokio::task::JoinHandle<()>) {
    let app = web::router(store, console);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr: SocketAddr = listener.local_addr().unwrap();
    let handle = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (format!("http://{addr}"), handle)
}

fn http_post_json(base: &str, path: &str, body: &Value) -> R<Value> {
    let agent = ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .timeout_global(Some(Duration::from_secs(20)))
            .build(),
    );
    let resp = agent
        .post(&format!("{base}{path}"))
        .header("Content-Type", "application/json")
        .send(body.to_string())
        .map_err(|e| format!("http: {e}"))?;
    let status = resp.status();
    let value = resp.into_body().read_json::<Value>().unwrap_or(Value::Null);
    if status != 200 {
        return Err(format!("{path} -> HTTP {status}: {value}"));
    }
    Ok(value)
}

fn http_get(base: &str, path: &str) -> R<Value> {
    let agent = ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .timeout_global(Some(Duration::from_secs(20)))
            .build(),
    );
    let resp = agent
        .get(&format!("{base}{path}"))
        .call()
        .map_err(|e| format!("http: {e}"))?;
    let status = resp.status();
    let value = resp.into_body().read_json::<Value>().unwrap_or(Value::Null);
    if status != 200 {
        return Err(format!("{path} -> HTTP {status}: {value}"));
    }
    Ok(value)
}

fn provision(
    store: &StateStore,
    name: &str,
    kind: &str,
    address: &str,
    secret: &[u8],
    grants: &[String],
    runner_dir: &Path,
) -> R<provisioner::ProvisionResult> {
    provisioner::provision_runner(
        store,
        &ProvisionRequest {
            name,
            kind,
            address,
            secret,
            runner_dir,
            grants,
        },
    )
    .map_err(|e| e.to_string())
}

/// One agent command doing the FULL B2 read/write round-trip (classic API):
/// authorize -> get upload url -> upload -> list names. `$B2` is the raw
/// credential env the runner injects; `curl -u` builds the Basic header.
const B2_UPLOAD_CMD: &str = concat!(
    "A=$(curl -sS -u \"$B2\" \"$B2_URL/b2api/v3/b2_authorize_account\"); ",
    "API=$(printf '%s' \"$A\" | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"apiUrl\"])'); ",
    "TOK=$(printf '%s' \"$A\" | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"authToken\"])'); ",
    "U=$(curl -sS -X POST \"$API/b2api/v3/b2_get_upload_url\" -H \"Authorization: $TOK\" -d '{\"bucketId\":\"b1\"}'); ",
    "UURL=$(printf '%s' \"$U\" | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"uploadUrl\"])'); ",
    "UTOK=$(printf '%s' \"$U\" | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"authorizationToken\"])'); ",
    "curl -sS -X POST \"$UURL\" -H \"Authorization: $UTOK\" -d 'hello-freehold-b2'; ",
    "echo; curl -sS -X POST \"$API/b2api/v3/b2_list_file_names\" -H \"Authorization: $TOK\" -d '{\"bucketId\":\"b1\"}'",
);

fn mcp_hostport(url: &str) -> String {
    url.trim_start_matches("http://")
        .trim_end_matches("/mcp")
        .to_string()
}

/// Run every acceptance check; failures are collected, never fatal.
pub async fn run_checks() -> Vec<Check> {
    let base = tempfile::tempdir().expect("tempdir");
    let mut checks = Vec::new();
    let agent = Agent::generate();
    let agent_pubkey = agent.pubkey.clone();

    // ONE shared StateStore — the console and the provisioner see the same
    // mutations (the Phase F shape).
    let cp_dir = base.path().join("cp");
    let store = Arc::new(StateStore::open(&cp_dir).expect("cp state"));
    let console = Console::load_or_create(&cp_dir).expect("console identity");
    let console_pk = console.pubkey();
    let (web_url, web_server) = boot_console(store.clone(), console).await;

    // G1 -- the algolia-style happy path, provisioned THROUGH the console.
    let svc_dir = base.path().join("runners/svc");
    let (sshd_addr, _sshd_state) = sshd::spawn_server(false).await;
    let (_key, pem) = sshd::client_key_pem();

    match g1_happy_path(&web_url, &svc_dir, &agent, &console_pk, &sshd_addr, &pem).await {
        Ok(detail) => checks.push(Check::pass(
            "G1.1",
            "algolia-style happy path: provision -> runner -> green -> grant -> green view",
            detail,
        )),
        Err(e) => checks.push(Check::fail(
            "G1.1",
            "algolia-style happy path: provision -> runner -> green -> grant -> green view",
            e,
        )),
    }

    // G2 -- the three connectors ALL via the runner. Secrets: the SSH PEM,
    // the Vultr token, the B2 key — resolved by the runner from ciphertext
    // the CP provisioned.
    let ssh_dir = base.path().join("runners/ssh");
    let ssh_addr_str = format!("testuser@127.0.0.1:{}", sshd_addr.port());
    let (ssh_url, ssh_server, ssh_job) = match provision(
        &store,
        "ssh",
        "ssh",
        &ssh_addr_str,
        pem.as_bytes(),
        std::slice::from_ref(&agent_pubkey),
        &ssh_dir,
    ) {
        Ok(_) => match serve_runner(&ssh_dir).await {
            Ok((url, task)) => {
                let nostr = Identity::load(&ssh_dir)
                    .map(|i| i.nostr_pubkey_hex())
                    .unwrap_or_default();
                (url, task, nostr)
            }
            Err(e) => {
                checks.push(Check::fail(
                    "G2.1",
                    "SSH exec via the runner",
                    format!("serve: {e}"),
                ));
                (
                    "http://127.0.0.1:1/mcp".into(),
                    tokio::task::spawn(async {}),
                    String::new(),
                )
            }
        },
        Err(e) => {
            checks.push(Check::fail(
                "G2.1",
                "SSH exec via the runner",
                format!("provision/serve: {e}"),
            ));
            (
                "http://127.0.0.1:1/mcp".into(),
                tokio::task::spawn(async {}),
                String::new(),
            )
        }
    };

    let vultr_state = Arc::new(VultrState::default());
    let vultr_addr = mock::spawn_http(mock::vultr_router(vultr_state.clone())).await;
    let vultr_url = format!("http://{vultr_addr}");
    let vultr_dir = base.path().join("runners/vultr");
    let (vultr_mcp, vultr_server, vultr_job) = match provision(
        &store,
        "vultr",
        "vultr",
        &vultr_url,
        VULTR_TOKEN.as_bytes(),
        std::slice::from_ref(&agent_pubkey),
        &vultr_dir,
    ) {
        Ok(_) => match serve_runner(&vultr_dir).await {
            Ok((url, task)) => {
                let nostr = Identity::load(&vultr_dir)
                    .map(|i| i.nostr_pubkey_hex())
                    .unwrap_or_default();
                (url, task, nostr)
            }
            Err(e) => {
                checks.push(Check::fail(
                    "G2.2",
                    "Vultr create/destroy via the runner",
                    format!("serve: {e}"),
                ));
                (
                    "http://127.0.0.1:1/mcp".into(),
                    tokio::task::spawn(async {}),
                    String::new(),
                )
            }
        },
        Err(e) => {
            checks.push(Check::fail(
                "G2.2",
                "Vultr create/destroy via the runner",
                format!("provision/serve: {e}"),
            ));
            (
                "http://127.0.0.1:1/mcp".into(),
                tokio::task::spawn(async {}),
                String::new(),
            )
        }
    };

    let b2_state = Arc::new(B2State::default());
    let b2_addr = mock::spawn_http(mock::b2_router(b2_state.clone())).await;
    let b2_url = format!("http://{b2_addr}");
    *b2_state.base_url.lock() = b2_url.clone();
    let b2_dir = base.path().join("runners/b2");
    let (b2_mcp, b2_server, b2_job) = match provision(
        &store,
        "b2",
        "b2",
        &b2_url,
        B2_CRED.as_bytes(),
        std::slice::from_ref(&agent_pubkey),
        &b2_dir,
    ) {
        Ok(_) => match serve_runner(&b2_dir).await {
            Ok((url, task)) => {
                let nostr = Identity::load(&b2_dir)
                    .map(|i| i.nostr_pubkey_hex())
                    .unwrap_or_default();
                (url, task, nostr)
            }
            Err(e) => {
                checks.push(Check::fail(
                    "G2.3",
                    "B2 read/write round-trip via the runner",
                    format!("serve: {e}"),
                ));
                (
                    "http://127.0.0.1:1/mcp".into(),
                    tokio::task::spawn(async {}),
                    String::new(),
                )
            }
        },
        Err(e) => {
            checks.push(Check::fail(
                "G2.3",
                "B2 read/write round-trip via the runner",
                format!("provision/serve: {e}"),
            ));
            (
                "http://127.0.0.1:1/mcp".into(),
                tokio::task::spawn(async {}),
                String::new(),
            )
        }
    };

    // G2.1
    if !ssh_job.is_empty() {
        match exec(
            &agent,
            &ssh_job,
            &ssh_url,
            "ssh",
            "echo ssh-accepted",
            &["ssh"],
        ) {
            Ok(out) if out.contains("ssh-accepted") => checks.push(Check::pass(
                "G2.1",
                "SSH exec via the runner",
                format!("stdout: {out:?}"),
            )),
            Ok(out) => checks.push(Check::fail(
                "G2.1",
                "SSH exec via the runner",
                format!("unexpected stdout: {out:?}"),
            )),
            Err(e) => checks.push(Check::fail("G2.1", "SSH exec via the runner", e)),
        }
    }

    // G2.2
    if !vultr_job.is_empty() {
        match vultr_create_destroy(&agent, &vultr_job, &vultr_mcp, &vultr_state).await {
            Ok(detail) => checks.push(Check::pass(
                "G2.2",
                "Vultr create/destroy via the runner",
                detail,
            )),
            Err(e) => checks.push(Check::fail(
                "G2.2",
                "Vultr create/destroy via the runner",
                e,
            )),
        }
    }

    // G2.3
    if !b2_job.is_empty() {
        match b2_roundtrip(&agent, &b2_job, &b2_mcp).await {
            Ok(detail) => checks.push(Check::pass(
                "G2.3",
                "B2 read/write round-trip via the runner",
                detail,
            )),
            Err(e) => checks.push(Check::fail(
                "G2.3",
                "B2 read/write round-trip via the runner",
                e,
            )),
        }
    }

    // G3 -- the security invariants. Every dimension must PROVE itself: a
    // silent empty scan (missing audit, failed status) fails the check.
    let mut status_evidence: Vec<String> = Vec::new();
    let mut status_problems: Vec<String> = Vec::new();
    for (label, nostr, url) in [
        ("ssh", &ssh_job, &ssh_url),
        ("vultr", &vultr_job, &vultr_mcp),
        ("b2", &b2_job, &b2_mcp),
    ] {
        match readiness(&agent, nostr, url) {
            Ok(map)
                if map
                    .get("local")
                    .and_then(Value::as_str)
                    .map(|s| s.starts_with("green"))
                    .unwrap_or(false) =>
            {
                status_evidence.push(format!("{label}:{map}"));
            }
            Ok(map) => status_problems.push(format!("{label}:UNEXPECTED({map})")),
            Err(e) => status_problems.push(format!("{label}:ERROR({e})")),
        }
    }
    let status_ok = status_problems.is_empty();

    // The audit log must EXIST and record our runs — a swallowed read or a
    // redirected audit path fails instead of passing silently.
    let mut audit_blob = String::new();
    let mut audit_missing: Vec<String> = Vec::new();
    for d in [&svc_dir, &ssh_dir, &vultr_dir, &b2_dir] {
        match std::fs::read_to_string(d.join("audit.log")) {
            Ok(content) => audit_blob.push_str(&content),
            Err(_) => audit_missing.push(d.display().to_string()),
        }
    }
    let audit_ok = audit_missing.is_empty() && !audit_blob.is_empty();

    let redaction_out = exec(
        &agent,
        &vultr_job,
        &vultr_mcp,
        "vultr",
        "echo tok=$VULTR",
        &["vultr"],
    )
    .unwrap_or_default();

    // G3.1 secrets never in agent context (exec output, status views, audit).
    let evidence = format!(
        "{}\n{}\n{}",
        status_evidence.join("\n"),
        audit_blob,
        redaction_out
    );
    let mut leaked = Vec::new();
    for secret in [VULTR_TOKEN, B2_CRED] {
        if evidence.contains(secret) {
            leaked.push(secret.to_string());
        }
    }
    if evidence.contains("OPENSSH PRIVATE KEY") {
        leaked.push("ssh private key pem".into());
    }
    let redaction_ok = redaction_out.contains("tok=***");
    if leaked.is_empty() && redaction_ok && status_ok && audit_ok {
        checks.push(Check::pass(
            "G3.1",
            "secrets never in agent context (exec, status, audit; redaction proven)",
            format!(
                "status={status_evidence:?}; audit={} byte(s); echo $VULTR -> ***",
                audit_blob.len()
            ),
        ));
    } else {
        checks.push(Check::fail(
            "G3.1",
            "secrets never in agent context (exec, status, audit; redaction proven)",
            format!(
                "leaked={leaked:?} redaction_ok={redaction_ok} status_ok={status_ok} ({status_problems:?}) audit_missing={audit_missing:?} audit_empty={}",
                audit_blob.is_empty()
            ),
        ));
    }

    // G3.2 runner holds only ciphertext + its own injected key.
    let mut problems: Vec<String> = Vec::new();
    for (dir, secret) in [
        (&vultr_dir, VULTR_TOKEN.as_bytes()),
        (&b2_dir, B2_CRED.as_bytes()),
    ] {
        let raw = std::fs::read_to_string(dir.join(freehold_core::secrets::SECRETS_FILE))
            .unwrap_or_default();
        if raw.as_bytes().windows(secret.len()).any(|w| w == secret) {
            problems.push(format!("{}: package contains plaintext", dir.display()));
        }
    }
    for dir in [&ssh_dir, &vultr_dir, &b2_dir] {
        if !dir.join("identity.json").exists() {
            problems.push(format!("{}: missing injected identity", dir.display()));
        }
    }
    // State matches the shipped package (ciphertext-only, consistent).
    let shipped_all = ["vultr", "b2"].iter().all(|name| {
        store
            .get_secret(name)
            .map(|rec| {
                SecretPackage::load(&dir_of(name, &base))
                    .map(|p| p.secrets.get(*name) == Some(&rec.ciphertext_hex))
                    .unwrap_or(false)
            })
            .unwrap_or(false)
    });
    if problems.is_empty() && shipped_all {
        checks.push(Check::pass("G3.2", "runner holds only ciphertext + injected key", "secrets.json = sealed hex only, matches CP state; injected identity in the package dir"));
    } else {
        checks.push(Check::fail(
            "G3.2",
            "runner holds only ciphertext + injected key",
            format!("problems={problems:?} state_matches_package={shipped_all}"),
        ));
    }

    // G3.3 no master key anywhere.
    match no_master_key(&cp_dir).await {
        Ok(detail) => checks.push(Check::pass("G3.3", "no master key anywhere", detail)),
        Err(e) => checks.push(Check::fail("G3.3", "no master key anywhere", e)),
    }

    // G3.4 revoking membership cuts off. Evidence gates: the runner must be
    // ALIVE before revoke (its own status probe), and the post-revoke denial
    // must be the fail-closed grant denial — not a timeout, not a connection
    // refused on a dead endpoint.
    let runner_alive = matches!(
        readiness(&agent, &vultr_job, &vultr_mcp),
        Ok(m) if m.get("local").and_then(Value::as_str) == Some("green")
    );
    if !runner_alive {
        checks.push(Check::fail(
            "G3.4",
            "revoking membership cuts off the runner",
            "runner never came up — cannot prove cutoff",
        ));
    } else {
        match provisioner::revoke_runner(&store, "vultr") {
            Ok(_) => {
                let cut = exec(
                    &agent,
                    &vultr_job,
                    &vultr_mcp,
                    "vultr",
                    "curl -sS \"$VULTR_URL/v2/instances\" -H \"Authorization: Bearer $VULTR\"",
                    &["vultr"],
                );
                let rotate_blocked =
                    provisioner::rotate_secret(&store, "vultr", VULTR_TOKEN.as_bytes()).is_err();
                match (cut, rotate_blocked) {
                    (Err(e), true) if e.contains("not granted") => {
                        checks.push(Check::pass(
                            "G3.4",
                            "revoking membership cuts off the runner",
                            format!("post-revoke exec denied with the fail-closed grant denial ({e}); rotate blocked"),
                        ))
                    }
                    (Err(_), true) => checks.push(Check::fail(
                        "G3.4",
                        "revoking membership cuts off the runner",
                        "denied, but NOT the fail-closed grant reason",
                    )),
                    (cut_res, rot) => checks.push(Check::fail(
                        "G3.4",
                        "revoking membership cuts off the runner",
                        format!("exec after revoke: {cut_res:?}; rotate blocked: {rot}"),
                    )),
                }
            }
            Err(e) => checks.push(Check::fail(
                "G3.4",
                "revoking membership cuts off the runner",
                format!("revoke failed: {e}"),
            )),
        }
    }

    // G3.5 rotation re-encrypts.
    match rotation_check(&store, &agent, &b2_job, &b2_mcp, &b2_dir, &b2_state).await {
        Ok(detail) => checks.push(Check::pass("G3.5", "rotation re-encrypts", detail)),
        Err(e) => checks.push(Check::fail("G3.5", "rotation re-encrypts", e)),
    }

    web_server.abort();
    ssh_server.abort();
    vultr_server.abort();
    b2_server.abort();
    checks
}

fn dir_of(name: &str, base: &tempfile::TempDir) -> PathBuf {
    match name {
        "vultr" => base.path().join("runners/vultr"),
        "b2" => base.path().join("runners/b2"),
        _ => base.path().join("runners/svc"),
    }
}

async fn g1_happy_path(
    web_url: &str,
    svc_dir: &Path,
    agent: &Agent,
    console_pk: &str,
    sshd_addr: &SocketAddr,
    pem: &str,
) -> R<String> {
    // The operator flow: provision through the console (console auto-granted).
    http_post_json(
        web_url,
        "/api/provision",
        &json!({
            "name": "svc",
            "kind": "ssh",
            "address": format!("testuser@127.0.0.1:{}", sshd_addr.port()),
            "secret": pem,
            "runner_dir": svc_dir.to_string_lossy(),
        }),
    )?;
    // Serve the shipped package; register its MCP addr with the console.
    let (svc_mcp, svc_server) = serve_runner(svc_dir).await?;
    http_post_json(
        web_url,
        "/api/runner-addr",
        &json!({ "name": "svc", "addr": mcp_hostport(&svc_mcp) }),
    )?;
    // Green view: the console (a granted peer) reads the runner's own check.
    let ov = http_get(web_url, "/api/overview")?;
    let svc = ov["runners"]
        .as_array()
        .and_then(|rs| rs.iter().find(|r| r["name"] == "svc"))
        .ok_or_else(|| format!("svc missing from overview: {ov}"))?;
    let readiness = svc["readiness"].clone();
    let green = readiness.get("local").and_then(Value::as_str) == Some("green")
        && readiness
            .get("svc")
            .and_then(Value::as_str)
            .map(|s| s.starts_with("green"))
            .unwrap_or(false);
    // Grant OUR agent through the console (the console itself was granted at
    // provision — the grant table shows both).
    http_post_json(
        web_url,
        "/api/grant",
        &json!({ "name": "svc", "pubkey": agent.pubkey }),
    )?;
    let ov2 = http_get(web_url, "/api/overview")?;
    let svc2 = ov2["runners"]
        .as_array()
        .and_then(|rs| rs.iter().find(|r| r["name"] == "svc"))
        .ok_or("svc missing after grant")?;
    let grants = svc2["grants"].as_array().cloned().unwrap_or_default();
    if !grants
        .iter()
        .any(|g| g.as_str() == Some(agent.pubkey.as_str()))
    {
        return Err(format!("agent not granted: {grants:?}"));
    }
    if !grants.iter().any(|g| g == console_pk) {
        return Err(format!("console missing from grants: {grants:?}"));
    }
    // The whole chain, end-to-end: the agent, a granted peer, execs on the
    // runner with the runner's own ssh credential.
    let nostr = Identity::load(svc_dir)
        .map(|i| i.nostr_pubkey_hex())
        .map_err(|e| e.to_string())?;
    let out = exec(agent, &nostr, &svc_mcp, "svc", "echo g1-accepted", &["svc"])?;
    if !out.contains("g1-accepted") {
        return Err(format!("signed exec failed: {out:?}"));
    }
    if !green {
        return Err(format!("green view missing: readiness={readiness}"));
    }
    svc_server.abort();
    Ok(format!(
        "readiness {readiness}; grants=[{grants:?}]; signed exec ok"
    ))
}

async fn vultr_create_destroy(
    agent: &Agent,
    runner_pubkey: &str,
    mcp_url: &str,
    state: &VultrState,
) -> R<String> {
    let created = exec(
        agent,
        runner_pubkey,
        mcp_url,
        "vultr",
        "curl -sS -X POST \"$VULTR_URL/v2/instances\" -H \"Authorization: Bearer $VULTR\"",
        &["vultr"],
    )?;
    if !created.contains("inst-0") {
        return Err(format!("create output: {created}"));
    }
    let destroy = exec(
        agent,
        runner_pubkey,
        mcp_url,
        "vultr",
        "curl -sS -X DELETE \"$VULTR_URL/v2/instances/inst-0\" -H \"Authorization: Bearer $VULTR\" -o /dev/null -w done",
        &["vultr"],
    )?;
    if !destroy.contains("done") {
        return Err(format!("destroy output: {destroy}"));
    }
    if !state.instances.lock().is_empty() {
        return Err(format!(
            "mock still holds instances: {:?}",
            state.instances.lock()
        ));
    }
    Ok("create inst-0, destroy -> 204, mock empty".into())
}

async fn b2_roundtrip(agent: &Agent, runner_pubkey: &str, mcp_url: &str) -> R<String> {
    let out = exec(agent, runner_pubkey, mcp_url, "b2", B2_UPLOAD_CMD, &["b2"])?;
    // Uploads (file-0) AND lists it back in ONE command — the whole
    // round-trip via the runner.
    if !out.contains("file-0") {
        return Err(format!("round-trip output: {out}"));
    }
    if out.contains("keyid123") || out.contains("appkey456") {
        return Err("b2 credential leaked through the round-trip".into());
    }
    Ok("authorize -> upload -> list names, all via the runner; credential clean".into())
}

async fn no_master_key(cp_dir: &Path) -> R<String> {
    let state_raw =
        std::fs::read_to_string(cp_dir.join("state.json")).map_err(|e| e.to_string())?;
    if state_raw.contains(VULTR_TOKEN) || state_raw.contains(B2_CRED) {
        return Err("plaintext found in cp state.json".into());
    }
    // The CP dir's private-key inventory: EXACTLY the console agent key.
    let mut identities_under_cp = Vec::new();
    let mut walk = vec![cp_dir.to_path_buf()];
    while let Some(d) = walk.pop() {
        for entry in std::fs::read_dir(&d).map_err(|e| e.to_string())? {
            let entry = entry.map_err(|e| e.to_string())?;
            if entry.path().is_dir() {
                walk.push(entry.path());
            } else if entry.file_name() == "identity.json" {
                identities_under_cp.push(entry.path());
            }
        }
    }
    let console_only = identities_under_cp.len() == 1
        && identities_under_cp[0]
            .parent()
            .and_then(|p| p.file_name())
            .map(|f| f == "console")
            .unwrap_or(false);
    if !console_only {
        return Err(format!(
            "cp private-key inventory wrong: {identities_under_cp:?} (expected only console/identity.json)"
        ));
    }
    // And that console key cannot decrypt anything the CP ships: it is not
    // the encryption recipient of any runner.
    let console_id = Identity::load(&cp_dir.join("console")).map_err(|e| e.to_string())?;
    for (label, dir) in [("vultr", "runners/vultr"), ("b2", "runners/b2")] {
        let runner_id =
            Identity::load(&cp_dir.parent().unwrap().join(dir)).map_err(|e| e.to_string())?;
        if runner_id.enc_pubkey_hex() == console_id.enc_pubkey_hex() {
            return Err(format!(
                "console enc key == {label} runner enc key (would be a master key)"
            ));
        }
    }
    Ok("state.json = pubkeys + ciphertext only; the ONLY key under the CP dir is the console AGENT key, which is not the encryption recipient of any runner".into())
}

async fn rotation_check(
    store: &StateStore,
    agent: &Agent,
    runner_pubkey: &str,
    _mcp_url: &str,
    pkg_dir: &Path,
    b2_state: &B2State,
) -> R<String> {
    let before = store
        .get_secret("b2")
        .ok_or("secret record missing")?
        .ciphertext_hex;
    // Rotate to the SAME value: re-encryption is observable as fresh
    // ciphertext (new nonce) even with an unchanged credential.
    provisioner::rotate_secret(store, "b2", B2_CRED.as_bytes()).map_err(|e| e.to_string())?;
    let after = store.get_secret("b2").ok_or("secret record missing")?;
    if after.ciphertext_hex == before {
        return Err("rotation did not re-encrypt (ciphertext unchanged)".into());
    }
    if after.rotated_at.is_none() {
        return Err("rotated_at not stamped".into());
    }
    let shipped = SecretPackage::load(pkg_dir).map_err(|e| e.to_string())?;
    if shipped.secrets.get("b2") != Some(&after.ciphertext_hex) {
        return Err("shipped package ciphertext != state ciphertext after rotate".into());
    }
    // The RE-SHIPPED ciphertext must decrypt with the runner's injected key.
    // A runner booted BEFORE the rotate holds the pre-rotation package in
    // memory (only grants are re-read from disk), so restart it: the new
    // process decrypts the rotated blob with its unchanged injected key.
    let (new_mcp, new_server) = serve_runner(pkg_dir).await?;
    b2_state.files.lock().clear();
    let uploaded = exec(agent, runner_pubkey, &new_mcp, "b2", B2_UPLOAD_CMD, &["b2"])?;
    new_server.abort();
    if !uploaded.contains("file-0") {
        return Err(format!("post-rotate round-trip failed: {uploaded}"));
    }
    Ok("fresh ciphertext on rotate (new nonce); rotated_at stamped; package re-shipped; a restarted runner decrypts the NEW blob with its injected key".into())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test(flavor = "multi_thread")]
    async fn every_acceptance_check_passes() {
        let checks = run_checks().await;
        for c in &checks {
            eprintln!(
                "[{}] {} — {}",
                if c.ok { "PASS" } else { "FAIL" },
                c.id,
                c.description
            );
            if let Some(d) = &c.detail {
                eprintln!("      {d}");
            }
        }
        let failed: Vec<&Check> = checks.iter().filter(|c| !c.ok).collect();
        assert!(
            failed.is_empty(),
            "acceptance failures:\n{}",
            failed
                .iter()
                .map(|c| format!("{}: {} — {:?}", c.id, c.description, c.detail))
                .collect::<Vec<_>>()
                .join("\n")
        );
        assert_eq!(checks.len(), 9, "nine checks: G1.1, G2.1-3, G3.1-5");
    }
}
