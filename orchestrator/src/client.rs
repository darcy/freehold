//! A minimal MCP-over-HTTP CLIENT that signs every tools/call with the agent
//! identity (grants enforce it runner-side). Reuses `runner::auth` — the
//! exact verifier the runner runs — so the orchestrator provably speaks the
//! same wire contract.

use freehold_runner::auth;
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
                    .timeout_global(Some(std::time::Duration::from_secs(30)))
                    .build(),
            ),
        })
    }

    fn raw(&self, body: Value) -> Result<Value, ClientError> {
        let raw = body.to_string();
        // Sign a single timestamp — the header must carry the SAME ts the
        // signature covers.
        let ts = auth::now_secs();
        let ev = auth::sign_body(&self.auth.secret, &self.runner_pubkey, ts, &raw);
        let (pubkey, sig) = (self.auth.pubkey.clone(), ev.sig);
        let resp = self
            .agent
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

    /// Run exec; returns stdout when successful.
    pub fn exec(
        &self,
        target: &str,
        cmd: &str,
        secrets: &[&str],
    ) -> Result<ExecOutcome, ClientError> {
        let arguments = json!({
            "cmd": cmd,
            "target": target,
            "secrets": secrets,
        });
        let resp = self.call("exec", arguments)?;
        let text = resp["result"]["content"][0]["text"]
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
