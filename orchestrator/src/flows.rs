//! The scripted flows (E1/E2): onboarding, readiness, exec, demo.

use std::path::{Path, PathBuf};

use freehold_control_plane::provisioner::{self, ProvisionRequest};
use freehold_control_plane::state::StateStore;
use freehold_core::identity::Identity;
use freehold_core::secrets::SecretPackage;
use freehold_runner::mcp::{self, RunnerContext};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

use crate::client::{AgentAuth, ExecOutcome, McpClient};

#[derive(Debug, Error)]
pub enum FlowError {
    #[error("identity error: {0}")]
    Identity(#[from] freehold_core::identity::IdentityError),
    #[error("state error: {0}")]
    State(#[from] freehold_control_plane::state::StateError),
    #[error("provisioner error: {0}")]
    Provisioner(#[from] provisioner::ProvisionError),
    #[error("client error: {0}")]
    Client(#[from] crate::client::ClientError),
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
    #[error("bad agent identity dir {0:?}: {1}")]
    AgentDir(PathBuf, String),
}

/// Load the agent identity (identity.json in `dir`) and its signing auth.
pub fn agent_auth(dir: &Path) -> Result<AgentAuth, FlowError> {
    let id =
        Identity::load(dir).map_err(|e| FlowError::AgentDir(dir.to_path_buf(), e.to_string()))?;
    AgentAuth::from_identity(&id).map_err(Into::into)
}

/// Connect to a RUNNING runner at `addr`, authenticating as the agent.
pub fn connect(addr: &str, agent_dir: &Path, runner_pubkey: &str) -> Result<McpClient, FlowError> {
    let auth = agent_auth(agent_dir)?;
    let url = if addr.contains("://") {
        addr.to_string()
    } else {
        format!("http://{addr}/mcp")
    };
    Ok(McpClient::new(url, auth, runner_pubkey.to_string())?)
}

#[derive(Debug, Serialize)]
pub struct OnboardReport {
    pub name: String,
    pub nostr_pubkey: String,
    pub enc_pubkey: String,
    pub runner_pubkey: String,
    pub agent_pubkey: String,
    pub readiness: serde_json::Map<String, Value>,
    pub package_dir: PathBuf,
}

/// E1: existing service + credential → runner → provision → self-check →
/// report readiness. The runner is served IN-PROCESS for validation, then
/// this process exits (the operator starts `runner serve` on `package_dir`
/// for the live runner).
pub async fn onboard(
    name: &str,
    kind: &str,
    address: &str,
    secret: &[u8],
    agent_dir: &Path,
    cp_state_dir: &Path,
    runner_dir: &Path,
) -> Result<OnboardReport, FlowError> {
    let agent = agent_auth(agent_dir)?;

    let store = StateStore::open(cp_state_dir)?;
    let provisioned = provisioner::provision_runner(
        &store,
        &ProvisionRequest {
            name,
            kind,
            address,
            secret,
            runner_dir,
            grants: std::slice::from_ref(&agent.pubkey),
        },
    )?;

    // Serve the runner in-process (same code path as `runner serve`) and
    // verify readiness through a REAL signed client.
    let runner_id = Identity::load(runner_dir)?;
    let pkg = SecretPackage::load(runner_dir)?;
    let ctx = RunnerContext {
        identity: runner_id.clone(),
        package: pkg,
        state_dir: runner_dir.to_path_buf(),
    };
    let (addr, server) = mcp::serve("127.0.0.1:0", ctx)
        .await
        .map_err(|e| FlowError::Io(std::io::Error::other(format!("runner serve: {e}"))))?;

    let client = McpClient::new(
        format!("http://{addr}/mcp"),
        agent.clone(),
        runner_id.nostr_pubkey_hex(),
    )?;
    let readiness = client.readiness()?;
    // Hard failure: onboarding without the local self-check green means the
    // engine room isn't alive. The TARGET's readiness is REPORTED, not
    // asserted — a red target onboards but the report says so.
    if readiness.get("local").and_then(Value::as_str) != Some("green") {
        return Err(FlowError::Client(crate::client::ClientError::ToolError(
            format!("local self-check not green after onboarding: {readiness:?}"),
        )));
    }
    server.abort();

    Ok(OnboardReport {
        name: name.to_string(),
        nostr_pubkey: provisioned.nostr_pubkey.clone(),
        enc_pubkey: provisioned.enc_pubkey.clone(),
        runner_pubkey: runner_id.nostr_pubkey_hex(),
        agent_pubkey: agent.pubkey.clone(),
        readiness,
        package_dir: provisioned.package_dir,
    })
}

/// E2: run scripted exec steps against the runner, reporting each.
#[derive(Debug, Clone, Deserialize)]
pub struct DemoStep {
    pub target: String,
    pub cmd: String,
    #[serde(default)]
    pub secrets: Vec<String>,
}

#[derive(Debug, Serialize)]
pub struct StepResult {
    pub index: usize,
    pub target: String,
    pub ok: bool,
    pub exit_code: Option<i32>,
    pub stdout_head: String,
    pub stderr_head: String,
    pub error: Option<String>,
}

pub fn run_demo(client: &McpClient, steps: &[DemoStep]) -> Result<Vec<StepResult>, FlowError> {
    let mut results = Vec::with_capacity(steps.len());
    for (idx, step) in steps.iter().enumerate() {
        let secret_refs: Vec<&str> = step.secrets.iter().map(String::as_str).collect();
        let outcome: Result<ExecOutcome, crate::client::ClientError> =
            client.exec(&step.target, &step.cmd, &secret_refs);
        match outcome {
            Ok(out) => results.push(StepResult {
                index: idx,
                target: step.target.clone(),
                ok: !out.timed_out && out.exit_code == Some(0),
                exit_code: out.exit_code,
                stdout_head: out.stdout.chars().take(160).collect(),
                stderr_head: out.stderr.chars().take(80).collect(),
                error: None,
            }),
            Err(e) => results.push(StepResult {
                index: idx,
                target: step.target.clone(),
                ok: false,
                exit_code: None,
                stdout_head: String::new(),
                stderr_head: String::new(),
                error: Some(e.to_string()),
            }),
        }
    }
    Ok(results)
}
