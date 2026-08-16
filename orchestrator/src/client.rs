//! A minimal MCP-over-HTTP CLIENT that signs every tools/call with the agent
//! identity (grants enforce it runner-side). Reuses `freehold_core::auth` — the
//! exact verifier the runner runs — so the orchestrator provably speaks the
//! same wire contract.

use freehold_core::auth;
use serde::Deserialize;
use serde_json::{Value, json};
use thiserror::Error;

#[derive(Debug, Error)]
pub enum ClientError {
    #[error("http error: {0}")]
    Http(String),
    #[error("json-rpc error: {0}")]
    Rpc(String),
    #[error("tool returned isError: {0}")]
    ToolError(String),
    #[error("runner pubkey must be 64 hex chars for the signature audience")]
    BadAudience,
}

/// Agent identity material: the secret key (signing) and the pubkey (granted
/// to runners).
#[derive(Clone)]
pub struct AgentAuth {
    pub secret: [u8; 32],
    pub pubkey: String,
}

impl Drop for AgentAuth {
    fn drop(&mut self) {
        use zeroize::Zeroize;
        self.secret.zeroize();
    }
}

impl AgentAuth {
    pub fn from_identity(id: &freehold_core::identity::Identity) -> Result<Self, ClientError> {
        let bytes = hex::decode(id.nostr_secret_hex())
            .map_err(|e| ClientError::Http(format!("bad nostr secret: {e}")))?;
        let mut secret = [0u8; 32];
        secret.copy_from_slice(&bytes);
        Ok(Self {
            secret,
            pubkey: id.nostr_pubkey_hex(),
        })
    }
}

#[derive(Clone)]
pub struct McpClient {
    pub url: String,
    pub auth: AgentAuth,
    pub runner_pubkey: String,
    agent: ureq::Agent,
}

impl McpClient {
    /// Global timeout for non-exec calls (status probes): a wedged runner
    /// must not hang the orchestrator forever.
    const BASE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(30);

    pub fn new(url: String, auth: AgentAuth, runner_pubkey: String) -> Result<Self, ClientError> {
        if runner_pubkey.len() != 64 || !runner_pubkey.chars().all(|c| c.is_ascii_hexdigit()) {
            return Err(ClientError::BadAudience);
        }
        Ok(Self {
            url,
            auth,
            runner_pubkey,
            agent: ureq::Agent::new_with_config(
                ureq::config::Config::builder()
                    .http_status_as_error(false)
                    .timeout_global(Some(Self::BASE_TIMEOUT))
                    .build(),
            ),
        })
    }

    /// A request agent whose HTTP deadline is `runner_timeout + margin`.
    /// The RUNNER's `timeout_s` watchdog is the honest deadline — the client
    /// must outlive it so a slow command surfaces as `timed_out: true`, not
    /// as a client-side Http error while the command keeps running.
    fn exec_agent(runner_timeout_s: u64) -> ureq::Agent {
        ureq::Agent::new_with_config(
            ureq::config::Config::builder()
                .http_status_as_error(false)
                .timeout_global(Some(std::time::Duration::from_secs(runner_timeout_s + 30)))
                .build(),
        )
    }

    fn raw(&self, body: Value) -> Result<Value, ClientError> {
        self.raw_with(&self.agent, body)
    }

    /// Sign and send, using `agent` (caller picks the deadline).
    fn raw_with(&self, agent: &ureq::Agent, body: Value) -> Result<Value, ClientError> {
        let raw = body.to_string();
        // Sign a single timestamp — the header must carry the SAME ts the
        // signature covers.
        let ts = auth::now_secs();
        let ev = auth::sign_body(&self.auth.secret, &self.runner_pubkey, ts, &raw);
        let (pubkey, sig) = (self.auth.pubkey.clone(), ev.sig);
        let resp = agent
            .post(&self.url)
            .header("Content-Type", "application/json")
            .header(auth::PUBKEY_HEADER, pubkey)
            .header(auth::SIG_HEADER, sig)
            .header(auth::TS_HEADER, ts)
            .send(raw.as_str())
            .map_err(|e| ClientError::Http(e.to_string()))?;
        resp.into_body()
            .read_json::<Value>()
            .map_err(|e| ClientError::Http(e.to_string()))
    }

    /// tools/call with the request signed by the agent.
    pub fn call(&self, name: &str, arguments: Value) -> Result<Value, ClientError> {
        let body = json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "tools/call",
            "params": { "name": name, "arguments": arguments }
        });
        let resp = self.raw(body)?;
        if let Some(err) = resp.get("error") {
            let code = err.get("code").and_then(Value::as_i64).unwrap_or(-1);
            let msg = err
                .get("message")
                .and_then(Value::as_str)
                .unwrap_or("(no message)");
            return Err(ClientError::Rpc(format!("{code}: {msg}")));
        }
        let result = &resp["result"];
        if result.get("isError").and_then(Value::as_bool) == Some(true) {
            let text = result["content"][0]["text"].as_str().unwrap_or("(no text)");
            return Err(ClientError::ToolError(text.to_string()));
        }
        Ok(resp)
    }

    /// Parse the tool's text payload.
    pub fn call_text(&self, name: &str, arguments: Value) -> Result<Value, ClientError> {
        let resp = self.call(name, arguments)?;
        let text = resp["result"]["content"][0]["text"]
            .as_str()
            .ok_or_else(|| ClientError::ToolError("missing text content".into()))?;
        serde_json::from_str(text)
            .map_err(|e| ClientError::ToolError(format!("bad text json: {e}")))
    }

    /// targets -> readiness string, from the aggregated status map.
    pub fn readiness(&self) -> Result<serde_json::Map<String, Value>, ClientError> {
        let status = self.call_text("status", json!({}))?;
        serde_json::from_value(status).map_err(|e| ClientError::ToolError(format!("status: {e}")))
    }

    /// Run exec with a runner-side watchdog of `timeout_s`. The client HTTP
    /// deadline is set above it (exec_agent), so a slow command is reported
    /// by the RUNNER as `timed_out: true` — never as a client-side error with
    /// the command still running.
    pub fn exec(
        &self,
        target: &str,
        cmd: &str,
        secrets: &[&str],
        timeout_s: u64,
    ) -> Result<ExecOutcome, ClientError> {
        let arguments = json!({
            "cmd": cmd,
            "target": target,
            "secrets": secrets,
            "timeout_s": timeout_s,
        });
        let agent = Self::exec_agent(timeout_s);
        let resp = self.raw_with(
            &agent,
            json!({
                "jsonrpc": "2.0",
                "id": 1,
                "method": "tools/call",
                "params": { "name": "exec", "arguments": arguments }
            }),
        )?;
        if let Some(err) = resp.get("error") {
            let code = err.get("code").and_then(Value::as_i64).unwrap_or(-1);
            let msg = err
                .get("message")
                .and_then(Value::as_str)
                .unwrap_or("(no message)");
            return Err(ClientError::Rpc(format!("{code}: {msg}")));
        }
        let result = &resp["result"];
        if result.get("isError").and_then(Value::as_bool) == Some(true) {
            let text = result["content"][0]["text"].as_str().unwrap_or("(no text)");
            return Err(ClientError::ToolError(text.to_string()));
        }
        let text = result["content"][0]["text"]
            .as_str()
            .ok_or_else(|| ClientError::ToolError("missing exec text".into()))?;
        let out: ExecOutcome = serde_json::from_str(text)
            .map_err(|e| ClientError::ToolError(format!("exec json: {e}")))?;
        Ok(out)
    }
}

#[derive(Debug, Clone, Deserialize)]
pub struct ExecOutcome {
    pub stdout: String,
    pub stderr: String,
    pub exit_code: Option<i32>,
    pub timed_out: bool,
}
