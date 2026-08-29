//! Orchestrator (Phase E) integration tests: the full engine-room loop
//! in-process — provision -> grant -> serve -> signed status -> signed exec —
//! plus the unauthorized-agent denial and the scripted demo steps.

use std::path::{Path, PathBuf};

use freehold_core::identity::Identity;
use freehold_orchestrator_lib::client::McpClient;
use freehold_orchestrator_lib::flows;
use freehold_runner::mcp::{self, RunnerContext};
use freehold_runner::secrets::SecretPackage;
use serde_json::Value;

fn agent_dir(base: &Path) -> PathBuf {
    let dir = base.join("agent");
    let id = Identity::generate();
    id.write_to_dir(&dir).unwrap();
    dir
}

fn cp_dir(base: &Path) -> PathBuf {
    base.join("cp")
}

async fn onboard_runner(
    base: &Path,
    agent: &Path,
    kind: &str,
    address: &str,
) -> flows::OnboardReport {
    flows::onboard(
        "demo",
        kind,
        address,
        b"demo-secret-value-123456",
        agent,
        &cp_dir(base),
        &base.join("runner"),
    )
    .await
    .unwrap()
}

/// Serve a shipped runner dir in-process (what `runner serve` does) and
/// return the client.
async fn serve_client(
    runner_dir: &Path,
    agent_dir: &Path,
    runner_pubkey: &str,
) -> (McpClient, tokio::task::JoinHandle<()>) {
    let runner_id = Identity::load(runner_dir).unwrap();
    let pkg = SecretPackage::load(runner_dir).unwrap();
    let ctx = RunnerContext {
        relay_url: None,
        relay_pubkey: None,
        identity: runner_id,
        package: pkg,
        state_dir: runner_dir.to_path_buf(),
    };
    let (addr, server) = mcp::serve("127.0.0.1:0", ctx).await.unwrap();
    tokio::time::sleep(std::time::Duration::from_millis(60)).await;
    let client = McpClient::new(
        format!("http://{addr}/mcp"),
        flows::agent_auth(agent_dir).unwrap(),
        runner_pubkey.to_string(),
    )
    .unwrap();
    (client, server)
}

#[tokio::test(flavor = "multi_thread")]
async fn onboard_then_signed_exec_roundtrip() {
    let base = tempfile::tempdir().unwrap();
    let adir = agent_dir(base.path());

    // E1: onboard an "external" service. It has no live endpoint in tests
    // (dead address) — what must be provable is: the runner is provisioned,
    // the runner's OWN local self-check is green, and the report carries the
    // readiness table.
    let report = onboard_runner(base.path(), &adir, "vultr", "http://127.0.0.1:1").await;
    assert_eq!(
        report.agent_pubkey,
        flows::agent_auth(&adir).unwrap().pubkey
    );
    assert_eq!(
        report.readiness.get("local").and_then(Value::as_str),
        Some("green"),
        "engine room local self-check must be green: {:?}",
        report.readiness
    );
    // The shipped package grants exactly the agent.
    let pkg = SecretPackage::load(&base.path().join("runner")).unwrap();
    assert!(pkg.grants.contains(&report.agent_pubkey));

    // Now the live runner (serve the shipped dir) + a signed exec on local.
    let (client, server) =
        serve_client(&base.path().join("runner"), &adir, &report.runner_pubkey).await;
    let out = client.exec("local", "echo onboard-ok", &[], 60).unwrap();
    assert_eq!(out.stdout.trim(), "onboard-ok");
    assert_eq!(out.exit_code, Some(0));

    // Audit carried the caller (the agent pubkey).
    let audit = std::fs::read_to_string(base.path().join("runner/audit.log")).unwrap();
    assert!(
        audit.contains(&report.agent_pubkey),
        "audit must record the agent: {audit}"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn ungranted_agent_is_denied() {
    let base = tempfile::tempdir().unwrap();
    let adir = agent_dir(base.path());
    let report = onboard_runner(base.path(), &adir, "vultr", "http://127.0.0.1:1").await;
    let (server_client, server) =
        serve_client(&base.path().join("runner"), &adir, &report.runner_pubkey).await;
    let _ = server_client;

    // A DIFFERENT agent (not granted) must be denied with the protocol error.
    let stranger = base.path().join("stranger");
    Identity::generate().write_to_dir(&stranger).unwrap();
    let stranger_client = McpClient::new(
        server_client_url(&server_client),
        flows::agent_auth(&stranger).unwrap(),
        report.runner_pubkey.clone(),
    )
    .unwrap();
    let err = stranger_client
        .exec("local", "echo nope", &[], 60)
        .unwrap_err();
    let msg = format!("{err:?}");
    assert!(
        msg.contains("-32001") && msg.contains("unauthorized"),
        "denial must carry the code AND the reason, got: {msg}"
    );
    server.abort();
}

fn server_client_url(c: &McpClient) -> String {
    c.url.clone()
}

#[tokio::test(flavor = "multi_thread")]
async fn demo_steps_run_and_report_failures() {
    let base = tempfile::tempdir().unwrap();
    let adir = agent_dir(base.path());
    let report = onboard_runner(base.path(), &adir, "vultr", "http://127.0.0.1:1").await;
    let (client, server) =
        serve_client(&base.path().join("runner"), &adir, &report.runner_pubkey).await;

    // CI shows a genuine first-call transient (the runner warming up under
    // parallel load) — retry the FIRST step briefly; a real regression still
    // fails loudly WITH the actual error instead of a bare assert.
    let retry_steps = vec![flows::DemoStep {
        target: "local".into(),
        cmd: "echo step-one".into(),
        secrets: vec![],
        timeout_s: 60,
    }];
    let mut ready = false;
    for attempt in 0..12 {
        let probe = flows::run_demo(&client, &retry_steps).unwrap();
        if probe[0].ok {
            ready = true;
            break;
        }
        if attempt == 11 {
            panic!(
                "first demo step never succeeded after warm-up: {:?}",
                probe[0]
            );
        }
        tokio::time::sleep(std::time::Duration::from_millis(150)).await;
    }
    assert!(ready, "runner did not come ready");

    let steps = vec![
        flows::DemoStep {
            target: "local".into(),
            cmd: "echo step-one".into(),
            secrets: vec![],
            timeout_s: 60,
        },
        flows::DemoStep {
            target: "local".into(),
            cmd: "printf step-two".into(),
            secrets: vec![],
            timeout_s: 60,
        },
        flows::DemoStep {
            target: "local".into(),
            cmd: "exit 3".into(),
            secrets: vec![],
            timeout_s: 60,
        },
        flows::DemoStep {
            target: "local".into(),
            cmd: "sleep 5".into(),
            secrets: vec![],
            timeout_s: 1,
        },
        flows::DemoStep {
            target: "missing-target".into(),
            cmd: "echo nope".into(),
            secrets: vec![],
            timeout_s: 60,
        },
    ];
    let results = flows::run_demo(&client, &steps).unwrap();
    assert!(
        results[0].ok && results[0].stdout_head.contains("step-one"),
        "step one failed: {:?}",
        results[0]
    );
    assert!(
        results[1].ok && results[1].stdout_head.contains("step-two"),
        "step two failed: {:?}",
        results[1]
    );
    assert!(
        !results[2].ok && results[2].exit_code == Some(3),
        "a non-zero exit must be reported as a FAILED step: {:?}",
        results[2]
    );
    assert!(
        results[3].timed_out && !results[3].ok,
        "a step overrunning timeout_s must report TIMEOUT, not a client-side error: {:?}",
        results[3]
    );
    assert!(
        !results[4].ok,
        "unknown target must fail the step: {:?}",
        results[4]
    );
    assert!(results[4].error.is_some());

    server.abort();
}
