//! The freehold console API — ONE contract, two clients.
//!
//! The web page (embedded in `control-plane/src/web.rs`) and the TUI
//! (`freehold-tui`) render the same console data through this module. It is
//! the CLIENT-side authority on the wire format: any request/response shape
//! change here MUST move together with `web.rs` (the router is the server
//! half — the round-trip test lives in `control-plane/src/web.rs`).
//!
//! # Auth
//! Auth is opt-in on the server (present when the deploy seeded
//! `--operator-pubkey`): NIP-98 login — GET `/api/auth/challenge`, sign the
//! issued nonce (kind 27235, tags `u=<base>` + `method=login`), POST
//! `/api/auth/login`, receive the `fh_session` cookie (24h, sliding). The
//! signing key lives on the OPERATOR's machine and never leaves it; callers
//! resolve the key (identity dir / nsec) and hand only the 32-byte seed to
//! [`Client::login`]. Without auth configured every endpoint stays
//! loopback-open — this client works in both postures (it sends the cookie
//! when it has one).
//!
//! # Endpoints
//! - `GET  /api/overview` — every runner: status, secret stamp, grants
//!   (read from the SHIPPED package — `None` = unreadable anomaly, distinct
//!   from an honest empty list), and the LIVE readiness probe (the console
//!   asks the runner through the same signed MCP channel an agent would).
//! - `POST /api/provision` `{name, kind, address, secret, risk?}`
//! - `POST /api/rotate` `{name, secret}`
//! - `POST /api/revoke` `{name}`
//! - `POST /api/grant` `{name, pubkey}` (Chunk 2.6.1: channel membership)
//! - `POST /api/revoke-grant` `{name, pubkey}`
//! - `POST /api/runner-addr` `{name, addr}`
//! - `GET  /api/runner/{name}/channel` — the operator's relay-channel view
//!   (profile + relay-verified roster + recent messages).

use std::time::{Duration, SystemTime, UNIX_EPOCH};

use freehold_core::nip98::{self, KIND_HTTP_AUTH};
use serde::{Deserialize, Serialize};
use thiserror::Error;

/// The console's session cookie name (server: `control-plane/src/web.rs`).
pub const SESSION_COOKIE: &str = "fh_session";

#[derive(Debug, Error)]
pub enum Error {
    #[error("GET {base}/api/auth/challenge — console reachable? {source}")]
    Challenge {
        base: String,
        #[source]
        source: ureq::Error,
    },
    #[error("challenge refused (HTTP {status}): {body}")]
    ChallengeStatus { status: u16, body: String },
    #[error("challenge response missing nonce: {body}")]
    ChallengeShape { body: String },
    #[error("nip98 signing: {0}")]
    Sign(String),
    #[error("login refused (HTTP {status}): {body}")]
    Login { status: u16, body: String },
    #[error("login response carried no session cookie")]
    NoCookie,
    #[error("portal response missing token: {0}")]
    PortalShape(String),
    #[error("{method} {path} — {source}")]
    Transport {
        method: String,
        path: String,
        #[source]
        source: ureq::Error,
    },
    #[error("{method} {path} -> HTTP {status}: {message}")]
    Api {
        method: String,
        path: String,
        status: u16,
        message: String,
    },
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    #[error("json: {0}")]
    Json(#[from] serde_json::Error),
}

fn now_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Normalize a Set-Cookie header value (or a bare token) down to
/// `fh_session=<token>` — the part the client echoes back.
fn normalize_cookie(raw: &str) -> String {
    let first = raw.split(';').next().unwrap_or(raw).trim();
    if first.starts_with(SESSION_COOKIE) {
        first.to_string()
    } else {
        format!("{SESSION_COOKIE}={first}")
    }
}

// ---------------------------------------------------------------------------
// Typed views of the wire format (server: `control-plane/src/web.rs`).
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Deserialize, Serialize, PartialEq)]
pub struct Overview {
    pub console_pubkey: String,
    pub runners: Vec<Runner>,
}

#[derive(Debug, Clone, Deserialize, Serialize, PartialEq)]
pub struct Runner {
    pub name: String,
    /// "active" | "revoked".
    pub status: String,
    pub nostr_pubkey: String,
    pub enc_pubkey: Option<String>,
    pub mcp_addr: Option<String>,
    pub risk: Option<String>,
    pub secret: Option<SecretInfo>,
    /// None = shipped package unreadable (anomaly); Some(empty) = nobody
    /// (fail closed).
    pub grants: Option<Vec<String>>,
    /// The LIVE readiness probe (`status` tool over the signed channel);
    /// absent when the runner is revoked or unreachable.
    #[serde(default)]
    pub readiness: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Deserialize, Serialize, PartialEq)]
pub struct SecretInfo {
    pub name: String,
    pub kind: String,
    pub address: String,
    #[serde(default)]
    pub rotated_at: Option<i64>,
    #[serde(default)]
    pub created_at: Option<i64>,
}

// ---------------------------------------------------------------------------
// Requests (Serialize only — the server owns the validation).
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize)]
pub struct ProvisionReq {
    pub name: String,
    pub kind: String,
    pub address: String,
    pub secret: String,
    /// Absolute package dir; the server defaults to `./.freehold/runner/<name>`
    /// (CWD-relative) when absent.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub runner_dir: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub risk: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct SecretReq {
    pub name: String,
    pub secret: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct NameReq {
    pub name: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct GrantReq {
    pub name: String,
    pub pubkey: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct AddrReq {
    pub name: String,
    pub addr: String,
}

// ---------------------------------------------------------------------------
// The client.
// ---------------------------------------------------------------------------

/// A logged-in (or not) connection to one console.
#[derive(Clone)]
pub struct Client {
    agent: ureq::Agent,
    base: String,
    cookie: Option<String>,
    /// The operator pubkey the session was issued to (login only).
    pubkey: Option<String>,
}

impl Client {
    /// NIP-98 login: challenge -> sign the nonce with `secret` (the
    /// operator's 32-byte seed) -> session cookie. `secret` is used for one
    /// signature and dropped; it never leaves this process.
    pub fn login(base: &str, secret: &[u8; 32]) -> Result<Self, Error> {
        Self::login_with_timeout(base, secret, Duration::from_secs(15))
    }

    /// [`Client::login`] with a caller-chosen per-call timeout (the TUI runs
    /// its refresh in the tick loop and needs a short one).
    pub fn login_with_timeout(
        base: &str,
        secret: &[u8; 32],
        timeout: Duration,
    ) -> Result<Self, Error> {
        let agent = Self::agent(timeout);
        let base = base.trim_end_matches('/').to_string();

        let ch_resp = agent
            .get(&format!("{base}/api/auth/challenge"))
            .call()
            .map_err(|source| Error::Challenge {
                base: base.clone(),
                source,
            })?;
        let ch_status = ch_resp.status().as_u16();
        if ch_status != 200 {
            let body = ch_resp.into_body().read_to_string().unwrap_or_default();
            return Err(Error::ChallengeStatus {
                status: ch_status,
                body,
            });
        }
        let ch: serde_json::Value =
            serde_json::from_str(&ch_resp.into_body().read_to_string().unwrap_or_default())
                .map_err(|_| Error::ChallengeShape {
                    body: "unparseable challenge response".into(),
                })?;
        let nonce = ch["nonce"].as_str().ok_or_else(|| Error::ChallengeShape {
            body: ch.to_string(),
        })?;

        // NIP-98 (kind 27235, content = the issued nonce). The `u` tag is
        // the console BASE URL and the method is "login" — the same pair
        // `console-login` always sent; the server verifies against these
        // exact tags.
        let ts = now_secs();
        let tags = vec![
            vec!["u".to_string(), base.clone()],
            vec!["method".to_string(), "login".to_string()],
        ];
        let (pubkey, _, sig) = nip98::sign_event(secret, KIND_HTTP_AUTH, ts, tags.clone(), nonce)
            .map_err(|e| Error::Sign(e.to_string()))?;
        let body = serde_json::json!({
            "nonce": nonce,
            "pubkey": pubkey,
            "created_at": ts,
            "tags": tags,
            "sig": sig,
        });

        let resp = agent
            .post(&format!("{base}/api/auth/login"))
            .header("Content-Type", "application/json")
            .send(body.to_string())
            .map_err(|source| Error::Transport {
                method: "POST".into(),
                path: "/api/auth/login".into(),
                source,
            })?;
        let status = resp.status().as_u16();
        if status != 200 {
            let body = body_text(resp.into_body())?;
            return Err(Error::Login {
                status,
                body: body.clone(),
            });
        }
        let raw_cookie = resp
            .headers()
            .get("set-cookie")
            .and_then(|v| v.to_str().ok())
            .map(String::from)
            .ok_or(Error::NoCookie)?;
        Ok(Self {
            agent,
            base,
            cookie: Some(normalize_cookie(&raw_cookie)),
            pubkey: Some(pubkey),
        })
    }

    /// Attach an EXISTING session (e.g. the cookie `console-login` printed)
    /// without re-signing. `cookie` may be the full Set-Cookie value or the
    /// bare `fh_session=<token>`.
    pub fn with_cookie(base: &str, cookie: &str) -> Self {
        Self {
            agent: Self::agent(Duration::from_secs(15)),
            base: base.trim_end_matches('/').to_string(),
            cookie: Some(normalize_cookie(cookie)),
            pubkey: None,
        }
    }

    pub fn base(&self) -> &str {
        &self.base
    }

    /// The operator pubkey this session was issued to (login path only).
    pub fn pubkey(&self) -> Option<&str> {
        self.pubkey.as_deref()
    }
    /// The session cookie (`fh_session=<token>`) — what `console-login`
    /// prints for browser/curl use.
    pub fn cookie(&self) -> Option<&str> {
        self.cookie.as_deref()
    }

    pub fn overview(&self) -> Result<Overview, Error> {
        self.get_json("/api/overview")
    }

    pub fn provision(&self, req: &ProvisionReq) -> Result<serde_json::Value, Error> {
        self.post_json("/api/provision", req)
    }

    pub fn rotate(&self, req: &SecretReq) -> Result<serde_json::Value, Error> {
        self.post_json("/api/rotate", req)
    }

    pub fn revoke(&self, name: &str) -> Result<serde_json::Value, Error> {
        self.post_json("/api/revoke", &NameReq { name: name.into() })
    }

    pub fn grant(&self, name: &str, pubkey: &str) -> Result<serde_json::Value, Error> {
        self.post_json(
            "/api/grant",
            &GrantReq {
                name: name.into(),
                pubkey: pubkey.into(),
            },
        )
    }

    pub fn revoke_grant(&self, name: &str, pubkey: &str) -> Result<serde_json::Value, Error> {
        self.post_json(
            "/api/revoke-grant",
            &GrantReq {
                name: name.into(),
                pubkey: pubkey.into(),
            },
        )
    }

    pub fn runner_addr(&self, name: &str, addr: &str) -> Result<serde_json::Value, Error> {
        self.post_json(
            "/api/runner-addr",
            &AddrReq {
                name: name.into(),
                addr: addr.into(),
            },
        )
    }

    pub fn channel(&self, name: &str) -> Result<serde_json::Value, Error> {
        self.get_json(&format!("/api/runner/{name}/channel"))
    }

    /// Mint a single-use portal token and return the URL to OPEN IN A
    /// BROWSER: the browser GETs it, receives a fresh session cookie for the
    /// SAME operator, and lands on the console logged in. The NIP-98 key
    /// never leaves this process and the token dies on first use.
    pub fn portal_url(&self) -> Result<String, Error> {
        let v = self.post_json("/api/auth/portal", &serde_json::json!({}))?;
        let token = v["token"]
            .as_str()
            .ok_or_else(|| Error::PortalShape(v.to_string()))?;
        Ok(format!("{}/api/auth/portal/{token}", self.base))
    }

    // -- internals ----------------------------------------------------------

    fn agent(timeout: Duration) -> ureq::Agent {
        ureq::Agent::config_builder()
            .http_status_as_error(false)
            .timeout_global(Some(timeout))
            .build()
            .into()
    }

    fn get_json<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T, Error> {
        let mut req = self.agent.get(&format!("{}{}", self.base, path));
        if let Some(c) = &self.cookie {
            req = req.header("cookie", c);
        }
        let resp = req.call().map_err(|source| Error::Transport {
            method: "GET".into(),
            path: path.into(),
            source,
        })?;
        let status = resp.status().as_u16();
        let text = body_text(resp.into_body())?;
        if status >= 400 {
            return Err(Error::Api {
                method: "GET".into(),
                path: path.into(),
                status,
                message: error_message(&text),
            });
        }
        Ok(serde_json::from_str(&text)?)
    }

    fn post_json<T: Serialize>(&self, path: &str, body: &T) -> Result<serde_json::Value, Error> {
        let mut req = self.agent.post(&format!("{}{}", self.base, path));
        if let Some(c) = &self.cookie {
            req = req.header("cookie", c);
        }
        let body = serde_json::to_string(body)?;
        let resp = req
            .header("Content-Type", "application/json")
            .send(body)
            .map_err(|source| Error::Transport {
                method: "POST".into(),
                path: path.into(),
                source,
            })?;
        let status = resp.status().as_u16();
        let text = body_text(resp.into_body())?;
        if status >= 400 {
            return Err(Error::Api {
                method: "POST".into(),
                path: path.into(),
                status,
                message: error_message(&text),
            });
        }
        if text.trim().is_empty() {
            return Ok(serde_json::json!({}));
        }
        Ok(serde_json::from_str(&text)?)
    }
}

/// ureq 3's `Body` reads with its own error type, not `io::Error` — fold
/// it into our `Error` so `?` works everywhere.
fn body_text(mut body: ureq::Body) -> Result<String, Error> {
    body.read_to_string()
        .map_err(|e| Error::Io(std::io::Error::other(e)))
}

/// The server reports failures as `{"error": "<message>"}`.
fn error_message(text: &str) -> String {
    serde_json::from_str::<serde_json::Value>(text)
        .ok()
        .and_then(|v| v["error"].as_str().map(String::from))
        .unwrap_or_else(|| text.to_string())
}
