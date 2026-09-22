//! C1 SSH connector tests against an IN-PROCESS russh SSH server — a real
//! ssh daemon (kex, host keys, exec channel) without any external dependency.
//! The server fixture lives in `freehold-testkit` (shared with the Phase G
//! acceptance script).

mod common;

use std::path::Path;
use std::sync::atomic::Ordering;
use std::time::Duration;

use std::collections::HashMap;

use freehold_runner::ssh::{self, SshPool, SshTarget};
use freehold_testkit::sshd::{client_key_pem, random_ed25519, spawn_server};
use russh::keys::ssh_key::PublicKey;

fn make_pool(state_dir: &Path) -> SshPool {
    SshPool::new(state_dir)
}

fn target(name: &str, addr: &std::net::SocketAddr) -> SshTarget {
    SshTarget {
        name: name.to_string(),
        host: "127.0.0.1".to_string(),
        port: addr.port(),
        user: "testuser".to_string(),
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn exec_roundtrip_over_ssh() {
    let (addr, state) = spawn_server(false).await;
    let dir = tempfile::tempdir().unwrap();
    let (_key, pem) = client_key_pem();
    let pool = make_pool(dir.path());

    let res = pool
        .exec(
            &target("t", &addr),
            &pem,
            "echo hello-over-ssh",
            &[],
            Some(10),
        )
        .await
        .unwrap();
    assert_eq!(res.stdout.trim(), "hello-over-ssh");
    assert_eq!(res.exit_code, Some(0));
    assert!(state.connections.load(Ordering::SeqCst) >= 1);

    // Exit status propagates.
    let res = pool
        .exec(&target("t", &addr), &pem, "exit 7", &[], Some(10))
        .await
        .unwrap();
    assert_eq!(res.exit_code, Some(7));
}

#[tokio::test(flavor = "multi_thread")]
async fn connection_is_pooled_across_execs() {
    let (addr, state) = spawn_server(false).await;
    let dir = tempfile::tempdir().unwrap();
    let (_key, pem) = client_key_pem();
    let pool = make_pool(dir.path());
    let t = target("t", &addr);

    pool.exec(&t, &pem, "echo one", &[], Some(10))
        .await
        .unwrap();
    pool.exec(&t, &pem, "echo two", &[], Some(10))
        .await
        .unwrap();
    pool.exec(&t, &pem, "echo three", &[], Some(10))
        .await
        .unwrap();

    assert_eq!(
        state.connections.load(Ordering::SeqCst),
        1,
        "repeated commands must reuse ONE ssh connection (the ControlMaster property)"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn wrong_key_is_rejected() {
    let (addr, _state) = spawn_server(true).await;
    let dir = tempfile::tempdir().unwrap();
    let (_key, pem) = client_key_pem();
    let pool = make_pool(dir.path());

    let err = pool
        .exec(&target("t", &addr), &pem, "echo x", &[], Some(10))
        .await
        .expect_err("server rejects all keys");
    assert!(
        err.to_string().contains("auth"),
        "expected auth failure, got: {err}"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn timeout_bounds_ssh_exec() {
    let (addr, _state) = spawn_server(false).await;
    let dir = tempfile::tempdir().unwrap();
    let (_key, pem) = client_key_pem();
    let pool = make_pool(dir.path());

    let start = std::time::Instant::now();
    let res = pool
        .exec(&target("t", &addr), &pem, "sleep 30", &[], Some(1))
        .await
        .unwrap_or_else(|e| panic!("ssh exec must not hard-error on timeout, got {e}"));
    assert!(
        start.elapsed() < Duration::from_secs(10),
        "ssh timeout must actually bound (took {:?})",
        start.elapsed()
    );
    // Same contract as local: Ok with timed_out: true, not an Err.
    assert!(res.timed_out, "timeout must be reported on the result");
    assert_eq!(res.exit_code, None);
}

fn known_label(host: &str, port: u16) -> String {
    format!("[{host}]:{port}")
}

#[tokio::test(flavor = "multi_thread")]
async fn host_keys_are_tofu_and_tamper_is_rejected() {
    let (addr, _state) = spawn_server(false).await;
    let dir = tempfile::tempdir().unwrap();
    let (_key, pem) = client_key_pem();
    let pool = make_pool(dir.path());
    let t = target("t", &addr);

    // First connect: host key stored (TOFU add).
    pool.exec(&t, &pem, "echo first", &[], Some(10))
        .await
        .unwrap();
    let known = dir.path().join(ssh::KNOWN_HOSTS_FILE);
    assert!(known.exists(), "TOFU must persist the host key");

    // Second connect (fresh pool, same store): key matches, still works.
    let pool2 = make_pool(dir.path());
    pool2
        .exec(&t, &pem, "echo second", &[], Some(10))
        .await
        .unwrap();

    // Semantic tamper: a FOREIGN key planted under a NEW label before ever
    // connecting there. The next connect to that label sees a key that does
    // not match the server's real key -> HostKeyChanged (MITM alarm).
    let evil = spawn_server(false).await.0;
    let t_evil = SshTarget {
        name: "evil".to_string(),
        host: "127.0.0.1".to_string(),
        port: evil.port(),
        user: "testuser".to_string(),
    };
    let foreign = PublicKey::from(random_ed25519())
        .to_openssh()
        .unwrap()
        .to_string();
    let mut map: HashMap<String, String> =
        serde_json::from_str(&std::fs::read_to_string(&known).unwrap()).unwrap();
    map.insert(known_label(&t_evil.host, t_evil.port), foreign);
    std::fs::write(&known, serde_json::to_string(&map).unwrap()).unwrap();

    let fresh_pool = make_pool(dir.path());
    let err = fresh_pool
        .exec(&t_evil, &pem, "echo mitm", &[], Some(10))
        .await
        .expect_err("changed host key must reject");
    assert!(
        err.to_string().contains("changed"),
        "expected HostKeyChanged, got: {err}"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn self_check_reports_green() {
    let (addr, _state) = spawn_server(false).await;
    let dir = tempfile::tempdir().unwrap();
    let (_key, pem) = client_key_pem();
    let pool = make_pool(dir.path());

    let ok = pool.self_check(&target("t", &addr), &pem).await.unwrap();
    assert!(ok, "uname must succeed on the test server");
}

fn hex32(s: &str) -> [u8; 32] {
    let b = hex::decode(s).unwrap();
    let mut arr = [0u8; 32];
    arr.copy_from_slice(&b);
    arr
}

/// Full loop: a CP-shaped package (ciphertext + ssh target metadata) shipped
/// to a runner dir, then an AGENT-STYLE MCP exec routed to the ssh target —
/// credential resolved by NAME from ciphertext, value redacted, over a REAL
/// ssh connection.
#[tokio::test(flavor = "multi_thread")]
async fn mcp_exec_routes_to_ssh_target_over_the_wire() {
    use std::collections::BTreeMap;
    use std::time::Duration;

    use freehold_runner::identity::Identity;
    use freehold_runner::mcp::{self, RunnerContext};
    use freehold_runner::secrets::{SecretPackage, TargetMeta};

    let (addr, _) = spawn_server(false).await;
    let dir = tempfile::tempdir().unwrap();
    let id = Identity::generate();
    id.write_to_dir(dir.path()).unwrap();

    let (_key, pem) = client_key_pem();
    let enc = hex32(&id.enc_pubkey_hex());
    let blob = freehold_runner::crypto::seal(&enc, b"ssh-laptop", pem.as_bytes()).unwrap();
    let pkg = SecretPackage {
        secrets: BTreeMap::from([("ssh-laptop".to_string(), hex::encode(&blob))]),
        targets: BTreeMap::from([(
            "ssh-laptop".to_string(),
            TargetMeta {
                kind: "ssh".to_string(),
                address: format!("testuser@127.0.0.1:{}", addr.port()),
                secret: "ssh-laptop".to_string(),
            },
        )]),
        grants: vec![common::agent_pubkey()],
    };
    pkg.write_to_dir(dir.path()).unwrap();

    let runner_pubkey = id.nostr_pubkey_hex();
    let ctx = RunnerContext {
        relay_url: None,
        relay_pubkey: None,
        relay_auth_url: None,
        identity: id,
        package: pkg,
        state_dir: dir.path().to_path_buf(),
    };
    let (mcp_addr, server) = mcp::serve("127.0.0.1:0", ctx, false).await.unwrap();
    let url = format!("http://{mcp_addr}/mcp");

    let agent = ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .build(),
    );
    let body = |method: &str, params: serde_json::Value| serde_json::json!({ "jsonrpc": "2.0", "id": 1, "method": method, "params": params });
    let call = |params: serde_json::Value| -> Result<serde_json::Value, ureq::Error> {
        let body = body("tools/call", params);
        let raw = body.to_string();
        let (pubkey, sig, ts) = common::signed_headers(&raw, &runner_pubkey);
        agent
            .post(&url)
            .header("Content-Type", "application/json")
            .header("x-freehold-pubkey", pubkey)
            .header("x-freehold-sig", sig)
            .header("x-freehold-ts", ts)
            .send(raw.as_str())
            .map(|r| r.into_body().read_json::<serde_json::Value>().unwrap())
    };

    let exec = call(serde_json::json!({
        "name": "exec",
        "arguments": { "cmd": "echo over-ssh-ok", "target": "ssh-laptop", "secrets": ["ssh-laptop"] }
    }))
    .expect("exec call")
    .clone();
    let text = exec["result"]["content"][0]["text"]
        .as_str()
        .unwrap()
        .to_string();
    assert!(text.contains("over-ssh-ok"), "ssh exec output: {text}");
    assert!(
        !text.contains("BEGIN OPENSSH PRIVATE KEY"),
        "key must never leak"
    );

    // A requested EXTRA secret is rejected: ssh injects no env over the
    // channel, so it would otherwise run unset and fail confusingly.
    let extra = call(serde_json::json!({
        "name": "exec",
        "arguments": { "cmd": "echo x", "target": "ssh-laptop", "secrets": ["ssh-laptop", "vultr_api_key"] }
    }))
    .unwrap()
    .clone();
    assert_eq!(
        extra["result"]["isError"], true,
        "extra secrets must be rejected"
    );

    // Redaction is exercised for real: make the remote ECHO the key, so
    // deleting exec::redact from the ssh branch would fail this test.
    let leak_cmd = format!("printf %s '{}'", pem);
    let leak = call(serde_json::json!({
        "name": "exec",
        "arguments": { "cmd": leak_cmd, "target": "ssh-laptop", "secrets": ["ssh-laptop"] }
    }))
    .unwrap()
    .clone();
    let leak_text = leak["result"]["content"][0]["text"]
        .as_str()
        .unwrap()
        .to_string();
    assert!(
        !leak_text.contains("OPENSSH PRIVATE KEY"),
        "key must be redacted: {leak_text}"
    );
    assert!(
        leak_text.contains("***"),
        "redaction marker expected: {leak_text}"
    );

    // The pinned model signs EVERY command — ssh execs included.
    let audit = std::fs::read_to_string(dir.path().join("audit.log")).ok();
    assert!(
        audit.as_deref().is_some_and(|a| a.contains("ssh-laptop")),
        "ssh execs must land in the audit log"
    );

    // List now includes the ssh target; status reports it green.
    let list = call(serde_json::json!({ "name": "list", "arguments": {} })).unwrap();
    let list_text = list["result"]["content"][0]["text"].as_str().unwrap();
    assert!(list_text.contains("ssh-laptop"), "list: {list_text}");

    let status = call(serde_json::json!({ "name": "status", "arguments": {} })).unwrap();
    let status_text = status["result"]["content"][0]["text"].as_str().unwrap();
    assert!(
        status_text.contains("\"ssh-laptop\": \"green\""),
        "status: {status_text}"
    );

    server.abort();
    tokio::time::sleep(Duration::from_millis(50)).await;
}
