//! Running mode — the "Good to go!" dashboard PLUS full console parity:
//! services-at-a-glance and the same manage actions as the web UI, all
//! driven by the console API through the shared `freehold-console-client`.
//! The web page and this TUI are two clients of ONE contract.

use freehold_console_client::{Client, ProvisionReq, SecretReq};
use freehold_core::identity::Identity;
use freehold_core::secrets::SecretPackage;
use freehold_installer::config::{Config, cp_live, relay_live};
use freehold_installer::port_open;
use std::path::PathBuf;
use std::time::{Duration, Instant};

pub struct Running {
    pub cfg: Option<Config>,
    /// (label, reachable) — the bring-up probes.
    pub probes: Vec<(String, bool)>,
    pub last: Instant,
    pub request_configure: bool,
    /// Console API parity (the web UI's data through the same client).
    pub cp: ConsolePanel,
}

impl Running {
    pub fn new(cfg_path: PathBuf) -> Self {
        let cfg = Config::load(&cfg_path).ok().flatten();
        let mut r = Self {
            cfg: cfg.clone(),
            probes: Vec::new(),
            last: Instant::now() - Duration::from_secs(5),
            request_configure: false,
            cp: ConsolePanel::default(),
        };
        if let Some(c) = &cfg {
            r.probe(c);
        }
        r.attach_console();
        r
    }

    pub fn with_cfg(cfg_path: PathBuf, cfg: Config) -> Self {
        let mut r = Self::new(cfg_path);
        r.cfg = Some(cfg.clone());
        r.probe(&cfg);
        r.attach_console();
        r
    }

    fn probe(&mut self, cfg: &Config) {
        // the SAME real checks the mode probe uses — a reachable proxy is
        // not a running relay (this stale-TCP bug kept the running screen
        // "Good to go!" over an empty world).
        self.probes = vec![
            ("relay (/_liveness)".into(), relay_live(cfg)),
            ("control plane (/healthz)".into(), cp_live(cfg)),
            ("provisioning runner".into(), port_open(&cfg.runner.addr)),
        ];
        self.last = Instant::now();
    }

    /// Attach a console session: an explicit `FREEHOLD_CONSOLE_COOKIE`
    /// (login elsewhere with `console-login`, paste the session here) wins;
    /// otherwise log in with the operator identity the config records
    /// (`operator_identity` dir, minted at bootstrap). The key never leaves
    /// this machine — only the session cookie travels.
    fn attach_console(&mut self) {
        let Some(cfg) = &self.cfg else {
            self.cp.auth = AuthState::Missing;
            return;
        };
        if let Ok(cookie) = std::env::var("FREEHOLD_CONSOLE_COOKIE")
            && !cookie.trim().is_empty()
        {
            self.cp.client = Some(Client::with_cookie(&cfg.cp_url, &cookie));
            self.cp.auth = AuthState::Live;
            self.cp.auth_reason = "FREEHOLD_CONSOLE_COOKIE session".into();
            return;
        }
        let Some(dir) = &cfg.operator_identity else {
            self.cp.auth = AuthState::Missing;
            self.cp.auth_reason =
                "no operator identity in config and no FREEHOLD_CONSOLE_COOKIE — l retries; \
                 see README console-login"
                    .into();
            return;
        };
        match Identity::load(dir) {
            Ok(id) => {
                let seed = id.secret_seed();
                let url = cfg.cp_url.clone();
                match Client::login_with_timeout(&url, &seed, Duration::from_secs(6)) {
                    Ok(c) => {
                        self.cp.client = Some(c);
                        self.cp.auth = AuthState::Live;
                        self.cp.auth_reason = format!("operator @ {}", dir.display());
                    }
                    Err(e) => {
                        self.cp.auth = AuthState::Failed;
                        self.cp.auth_reason = format!("login: {e}");
                    }
                }
            }
            Err(e) => {
                self.cp.auth = AuthState::Failed;
                self.cp.auth_reason = format!("identity {}: {e}", dir.display());
            }
        }
    }

    fn refresh(&mut self) {
        if self.cp.view == PanelView::Local {
            self.cp.local = read_local();
            return;
        }
        let Some(client) = self.cp.client.as_ref() else {
            return;
        };
        if self.cp.last_fetch.elapsed() < Duration::from_secs(2) {
            return;
        }
        // guard BEFORE the blocking call — a dead console must not make the
        // tick loop busy-fetch.
        self.cp.last_fetch = Instant::now();
        match client.overview() {
            Ok(ov) => {
                self.cp.overview = Some(ov);
            }
            Err(freehold_console_client::Error::Api { status: 401, .. }) => {
                // the session died — stop retrying until the operator logs
                // in again.
                self.cp.client = None;
                self.cp.auth = AuthState::Failed;
                self.cp.auth_reason = "session expired — press l to log in again".into();
            }
            Err(e) => self.cp.notice = format!("overview: {e}"),
        }
    }

    pub fn on_key(&mut self, code: crossterm::event::KeyCode) {
        // typing in a prompt consumes everything: Esc cancels, Enter
        // submits, the char feeds the buffer.
        if self.cp.prompt.is_some() {
            match code {
                crossterm::event::KeyCode::Esc => {
                    self.cp.prompt = None;
                    self.cp.notice = "cancelled".into();
                }
                crossterm::event::KeyCode::Enter => {
                    let flow = self.cp.prompt.as_ref().map(|p| p.flow).unwrap();
                    let done = self
                        .cp
                        .prompt
                        .as_mut()
                        .map(|p| p.take_field())
                        .unwrap_or(false);
                    let values = std::mem::take(
                        &mut self
                            .cp
                            .prompt
                            .as_mut()
                            .map(|p| p.values.clone())
                            .unwrap_or_default(),
                    );
                    if done {
                        self.cp.prompt = None;
                        self.run_action(flow, values);
                    }
                }
                crossterm::event::KeyCode::Backspace => {
                    if let Some(p) = &mut self.cp.prompt {
                        p.buf.pop();
                    }
                }
                crossterm::event::KeyCode::Char(c) => {
                    if let Some(p) = &mut self.cp.prompt {
                        p.buf.push(c);
                    }
                }
                _ => {}
            }
            return;
        }
        match code {
            crossterm::event::KeyCode::Char('c') => self.request_configure = true,
            crossterm::event::KeyCode::Char('l') => {
                self.cp.client = None;
                self.cp.auth = AuthState::Missing;
                self.cp.auth_reason.clear();
                self.cp.last_login_attempt = Instant::now();
                self.attach_console();
                self.refresh();
            }
            crossterm::event::KeyCode::Char('r') => self.refresh(),
            crossterm::event::KeyCode::Char('p') => self.start(Flow::Provision),
            crossterm::event::KeyCode::Char('R') => self.start(Flow::Rotate),
            crossterm::event::KeyCode::Char('x') => self.start(Flow::Revoke),
            crossterm::event::KeyCode::Char('g') => self.start(Flow::Grant),
            crossterm::event::KeyCode::Char('G') => self.start(Flow::Ungrant),
            crossterm::event::KeyCode::Char('a') => self.start(Flow::Addr),
            crossterm::event::KeyCode::Char('v') => self.start(Flow::Channel),
            crossterm::event::KeyCode::Tab => {
                self.cp.view = match self.cp.view {
                    PanelView::Remote => PanelView::Local,
                    PanelView::Local => PanelView::Remote,
                };
                self.cp.notice = match self.cp.view {
                    PanelView::Remote => "remote console list".into(),
                    PanelView::Local => {
                        "local loopback list (view-only — manage via remote)".into()
                    }
                };
                self.refresh();
            }
            crossterm::event::KeyCode::Char('w') => {
                if self.cp.view == PanelView::Local {
                    self.cp.notice =
                        "local has no web UI (loopback posture) — Tab to the remote list".into();
                    return;
                }
                let Some(client) = self.cp.client.as_ref() else {
                    self.cp.notice =
                        "not logged in — press l first (the web needs a session)".into();
                    return;
                };
                match client.portal_url() {
                    Ok(url) => {
                        self.cp.notice = format!("web opened: {url}");
                        // detached + silent — the browser outlives the TUI
                        // and must not scribble into the TUI's screen.
                        let _ = std::process::Command::new("xdg-open")
                            .arg(&url)
                            .stdout(std::process::Stdio::null())
                            .stderr(std::process::Stdio::null())
                            .spawn()
                            .map_err(|e| format!("xdg-open: {e}"));
                    }
                    Err(e) => self.cp.notice = format!("web launch failed: {e}"),
                }
            }
            crossterm::event::KeyCode::Esc => self.cp.channel = None,
            _ => {}
        }
    }

    pub fn is_typing(&self) -> bool {
        self.cp.prompt.is_some()
    }

    fn start(&mut self, flow: Flow) {
        if self.cp.view == PanelView::Local {
            self.cp.notice =
                "local is a view-only list — Tab to the remote console to manage".into();
            return;
        }
        if self.cp.client.is_none() {
            self.cp.notice = "not logged in — press l first".into();
            return;
        }
        self.cp.prompt = Some(Prompt::new(flow));
    }

    fn run_action(&mut self, flow: Flow, values: Vec<String>) {
        let name = || flow_name(flow);
        if flow == Flow::Channel {
            let Some(client) = self.cp.client.as_ref() else {
                self.cp.notice = "not logged in — press l".into();
                return;
            };
            match client.channel(&values[0]) {
                Ok(v) => {
                    self.cp.channel =
                        Some(serde_json::to_string_pretty(&v).unwrap_or_else(|_| v.to_string()))
                }
                Err(e) => self.cp.notice = format!("channel: {e}"),
            }
            return;
        }
        let Some(client) = self.cp.client.as_ref() else {
            self.cp.notice = "not logged in — press l".into();
            return;
        };
        let res = match flow {
            Flow::Provision => client.provision(&ProvisionReq {
                name: values[0].clone(),
                kind: values[1].clone(),
                address: values[2].clone(),
                secret: values[3].clone(),
                runner_dir: None,
                risk: values.get(4).filter(|s| !s.is_empty()).cloned(),
            }),
            Flow::Rotate => client.rotate(&SecretReq {
                name: values[0].clone(),
                secret: values[1].clone(),
            }),
            Flow::Revoke => client.revoke(&values[0]),
            Flow::Grant => client.grant(&values[0], &values[1]),
            Flow::Ungrant => client.revoke_grant(&values[0], &values[1]),
            Flow::Addr => client.runner_addr(&values[0], &values[1]),
            Flow::Channel => unreachable!(),
        };
        match res {
            Ok(v) => {
                self.cp.notice = format!("{} ok: {}", name(), clip(&v.to_string(), 96));
                self.refresh();
            }
            Err(e) => self.cp.notice = format!("{} failed: {e}", name()),
        }
    }

    pub fn tick(&mut self) {
        if self.last.elapsed() >= Duration::from_secs(2)
            && let Some(cfg) = self.cfg.clone()
        {
            self.probe(&cfg);
            self.refresh();
        }
        // Auto-login retry: the console can still be warming the moment the
        // running screen shows — keep attempting until a session is live so
        // Good to go is logged in without pressing l.
        let can_login = self
            .cfg
            .as_ref()
            .is_some_and(|c| c.operator_identity.is_some())
            || std::env::var("FREEHOLD_CONSOLE_COOKIE")
                .map(|v| !v.trim().is_empty())
                .unwrap_or(false);
        if can_login
            && self.cp.auth != AuthState::Live
            && self.cp.client.is_none()
            && self.cp.last_login_attempt.elapsed() >= Duration::from_secs(8)
        {
            self.cp.last_login_attempt = Instant::now();
            self.attach_console();
            self.refresh();
        }
    }

    pub fn all_ok(&self) -> bool {
        !self.probes.is_empty() && self.probes.iter().all(|(_, ok)| *ok)
    }
}

// ---------------------------------------------------------------------------
// The console panel state.
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AuthState {
    /// session live (client present).
    Live,
    /// no key source / not attempted.
    Missing,
    /// login failed or the session died.
    Failed,
}

/// Which runner list the console panel is showing.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum PanelView {
    /// the DEPLOYED console, via its API (the same data the web UI renders).
    #[default]
    Remote,
    /// the LOCAL loopback world: ~/.freehold/control-plane state + the
    /// shipped packages' grants, read directly (view-only — manage via the
    /// remote tab).
    Local,
}

pub struct ConsolePanel {
    pub client: Option<Client>,
    pub overview: Option<freehold_console_client::Overview>,
    pub view: PanelView,
    /// the local loopback runner list (built when `view == Local`).
    pub local: Vec<LocalRunner>,
    pub auth: AuthState,
    pub auth_reason: String,
    /// transient feedback from the last action/refresh.
    pub notice: String,
    last_fetch: Instant,
    /// the last auto-login attempt — the login RETRIES while the console is
    /// still warming so "Good to go" really is logged in.
    last_login_attempt: Instant,
    /// a text-input prompt in progress (Esc cancels / Enter submits).
    pub prompt: Option<Prompt>,
    /// an open channel view (pretty JSON) — replaces the table.
    pub channel: Option<String>,
}

impl Default for ConsolePanel {
    fn default() -> Self {
        Self {
            client: None,
            overview: None,
            view: PanelView::Remote,
            local: Vec::new(),
            auth: AuthState::Missing,
            auth_reason: String::new(),
            notice: String::new(),
            last_fetch: Instant::now() - Duration::from_secs(60),
            last_login_attempt: Instant::now() - Duration::from_secs(60),
            prompt: None,
            channel: None,
        }
    }
}

/// One runner in the LOCAL loopback view — the same columns as the remote
/// overview, read straight off the local CP state + the shipped package
/// (no server, no session: the loopback IS the authn here).
pub struct LocalRunner {
    pub name: String,
    pub status: String,
    pub risk: Option<String>,
    pub secret: Option<(String, String, String)>, // name · kind · address
    pub grants: Option<Vec<String>>,
    pub reachable: bool,
}

/// The local world: ~/.freehold/control-plane/state.json + the runner
/// packages it points at. Missing state => empty (the panel says so).
fn read_local() -> Vec<LocalRunner> {
    use freehold_control_plane::state::{RunnerStatus, StateStore};
    let dir = freehold_installer::freehold_home().join("control-plane");
    if !dir.join("state.json").exists() {
        return Vec::new();
    }
    let Ok(store) = StateStore::open(&dir) else {
        return Vec::new();
    };
    let snap = store.snapshot();
    snap.runners
        .iter()
        .map(|(name, rec)| {
            let secret = snap
                .secrets
                .get(name)
                .map(|s| (s.runner.clone(), s.kind.clone(), s.address.clone()));
            let grants = SecretPackage::load(&rec.package_dir).ok().map(|p| p.grants);
            let status = match rec.status {
                RunnerStatus::Active => "active",
                RunnerStatus::Revoked => "revoked",
            };
            let reachable = rec
                .mcp_addr
                .as_deref()
                .map(freehold_installer::port_open)
                .unwrap_or(false);
            LocalRunner {
                name: name.clone(),
                status: status.into(),
                risk: rec.risk_level.clone(),
                secret,
                grants,
                reachable,
            }
        })
        .collect()
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Flow {
    Provision,
    Rotate,
    Revoke,
    Grant,
    Ungrant,
    Addr,
    Channel,
}

impl Flow {
    fn labels(self) -> &'static [&'static str] {
        match self {
            Flow::Provision => &[
                "runner name",
                "kind (local | ssh | vultr | b2)",
                "address",
                "secret (empty ok for local)",
                "risk (optional — Enter to skip)",
            ],
            Flow::Rotate => &["runner name", "new secret"],
            Flow::Revoke => &["runner name"],
            Flow::Grant => &["runner name", "agent pubkey (64-hex)"],
            Flow::Ungrant => &["runner name", "agent pubkey (64-hex)"],
            Flow::Addr => &["runner name", "new mcp addr"],
            Flow::Channel => &["runner name"],
        }
    }
}

pub struct Prompt {
    pub flow: Flow,
    pub step: usize,
    pub buf: String,
    pub values: Vec<String>,
}

impl Prompt {
    fn new(flow: Flow) -> Self {
        Self {
            flow,
            step: 0,
            buf: String::new(),
            values: Vec::new(),
        }
    }

    pub fn label(&self) -> &str {
        self.flow.labels()[self.step.min(self.flow.labels().len() - 1)]
    }

    /// Consume the buffer into the collected values; true when the flow is
    /// complete (the caller dispatches the action).
    fn take_field(&mut self) -> bool {
        self.values.push(std::mem::take(&mut self.buf));
        self.step += 1;
        self.step >= self.flow.labels().len()
    }
}

fn flow_name(flow: Flow) -> &'static str {
    match flow {
        Flow::Provision => "provision",
        Flow::Rotate => "rotate",
        Flow::Revoke => "revoke",
        Flow::Grant => "grant",
        Flow::Ungrant => "ungrant",
        Flow::Addr => "addr",
        Flow::Channel => "channel",
    }
}

/// Truncate a display string to `n` chars (char-based, appends "…").
pub fn clip(s: &str, n: usize) -> String {
    if s.chars().count() <= n {
        s.to_string()
    } else {
        let head: String = s.chars().take(n.saturating_sub(1)).collect();
        format!("{head}…")
    }
}
