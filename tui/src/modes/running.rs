//! Running mode — the post-bring-up dashboard. THREE views, cycled with
//! Tab / Shift-Tab:
//!   - **Agents** — named agents stood up so far (the local agent
//!     identities; the CP-side registry the CPA writes is the follow-up).
//!   - **Services** — everything running, by name / where / status / URL:
//!     relay + control plane today, k3s/litellm/etc. as their coordinates
//!     land in the config (`managed`).
//!   - **Runners** — the console API parity (remote console list, same data
//!     as the web UI) + the local loopback list (`t` toggles the source).
//!
//! The world strip (one line) keeps the liveness glance; the Good-to-go box
//! is gone.

use freehold_console_client::{Client, ProvisionReq, SecretReq};
use freehold_core::identity::Identity;
use freehold_core::secrets::SecretPackage;
use freehold_installer::config::{Config, cp_live, k3s_live, relay_live};
use freehold_installer::port_open;
use std::path::PathBuf;
use std::time::{Duration, Instant};

pub struct Running {
    pub cfg: Option<Config>,
    /// (label, reachable) — the world strip + the services status.
    pub probes: Vec<(String, bool)>,
    pub view: DashboardView,
    pub services: Vec<ServiceRow>,
    pub agents: Vec<AgentRow>,
    /// Console API parity (the runner lists).
    pub cp: ConsolePanel,
    last_agents: Instant,
    /// per-view "last refreshed" stamps (each view refreshes on its own
    /// cadence; the timestamp tells the truth about the data shown).
    pub services_at: Instant,
    pub agents_at: Instant,
    pub runners_at: Instant,
    pub last: Instant,
    pub request_configure: bool,
}

impl Running {
    pub fn new(cfg_path: PathBuf) -> Self {
        let cfg = Config::load(&cfg_path).ok().flatten();
        let mut r = Self {
            cfg: cfg.clone(),
            probes: Vec::new(),
            view: DashboardView::default(),
            services: Vec::new(),
            agents: Vec::new(),
            cp: ConsolePanel::default(),
            last: Instant::now() - Duration::from_secs(5),
            last_agents: Instant::now() - Duration::from_secs(60),
            services_at: Instant::now(),
            agents_at: Instant::now(),
            runners_at: Instant::now(),
            request_configure: false,
        };
        if let Some(c) = &cfg {
            r.probe(c);
            r.services = build_services(c, &r.probes);
        }
        r.attach_console();
        r
    }

    pub fn with_cfg(cfg_path: PathBuf, cfg: Config) -> Self {
        let mut r = Self::new(cfg_path);
        r.cfg = Some(cfg.clone());
        r.probe(&cfg);
        r.services = build_services(&cfg, &r.probes);
        // new() already attached (and stamped the throttle) — re-attaching
        // here would be a second immediate login on entry.
        r
    }

    /// The registered AI agents, with availability the console probed
    /// against the relay (kind-9 presence). Refresh is throttled — each
    /// request makes the console probe every agent.
    fn refresh_agents(&mut self) {
        if self.last_agents.elapsed() < Duration::from_secs(15) {
            return;
        }
        self.last_agents = Instant::now();
        let Some(client) = self.cp.client.as_ref() else {
            return;
        };
        match client.agents() {
            Ok(list) => {
                self.agents_at = Instant::now();
                let now = freehold_core::auth::now_secs();
                self.agents = list
                    .into_iter()
                    .map(|a| AgentRow {
                        name: a.name,
                        pubkey: a.pubkey,
                        created: humanize((now as u64).saturating_sub(a.created_at)),
                        available: a.available,
                        note: a.note,
                    })
                    .collect();
            }
            Err(e) => self.cp.notice = format!("agents: {e}"),
        }
    }

    fn probe(&mut self, cfg: &Config) {
        // the SAME real checks the mode probe used — a reachable proxy is
        // not a running relay (this stale-TCP bug kept the old running
        // screen "Good to go!" over an empty world).
        self.probes = vec![
            ("relay".into(), relay_live(cfg)),
            ("control plane".into(), cp_live(cfg)),
            ("k3s".into(), k3s_live(cfg)),
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
        self.cp.last_login_attempt = Instant::now();
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
        // the AGENTS list is view-independent — refresh it even when the
        // Runners panel is toggled to the local loopback (r / the 15s tick
        // must still land).
        self.refresh_agents();
        if self.cp.view == PanelView::Local {
            self.cp.local = read_local(&self.cfg);
            // the stamp reflects the REAL read, not the throttle's tick.
            self.runners_at = Instant::now();
            return;
        }
        if self.cp.last_fetch.elapsed() < Duration::from_secs(2) {
            return;
        }
        // guard BEFORE the blocking call — a dead console must not make the
        // tick loop busy-fetch.
        self.cp.last_fetch = Instant::now();
        let Some(client) = self.cp.client.as_ref() else {
            return;
        };
        match client.overview() {
            Ok(ov) => {
                self.cp.overview = Some(ov);
                // stamp only when data actually landed.
                self.runners_at = Instant::now();
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
        // Keys are scoped to the ACTIVE view: navigation (Tab/BackTab) and
        // mode switches (c) work everywhere; everything else belongs to the
        // view shown — the footer hint changes per view, so a key does
        // exactly what the active view's hint says.
        match (self.view, code) {
            // global: cycle views + reconfigure.
            (_, crossterm::event::KeyCode::Tab) => {
                self.view = self.view.next();
                self.cp.notice.clear();
            }
            (_, crossterm::event::KeyCode::BackTab) => {
                self.view = self.view.prev();
                self.cp.notice.clear();
            }
            (_, crossterm::event::KeyCode::Char('c')) => self.request_configure = true,
            // refresh NOW on any view: reset every throttle and re-pull the
            // active view's data + the world strip.
            (_, crossterm::event::KeyCode::Char('r')) => self.refresh_now(),
            // runners: login, source toggle, actions, channel, web.
            (DashboardView::Runners, crossterm::event::KeyCode::Char('l')) => {
                self.cp.client = None;
                self.cp.auth = AuthState::Missing;
                self.cp.auth_reason.clear();
                self.cp.last_login_attempt = Instant::now();
                self.attach_console();
                self.refresh();
            }
            (DashboardView::Runners, crossterm::event::KeyCode::Char('t')) => {
                self.cp.view = match self.cp.view {
                    PanelView::Remote => PanelView::Local,
                    PanelView::Local => PanelView::Remote,
                };
                self.cp.notice = match self.cp.view {
                    PanelView::Remote => "runners: remote console".into(),
                    PanelView::Local => {
                        "runners: local loopback (view-only — manage via remote)".into()
                    }
                };
                self.refresh();
            }
            (DashboardView::Runners, crossterm::event::KeyCode::Char('w')) => {
                self.launch_web();
            }
            (DashboardView::Runners, crossterm::event::KeyCode::Char('p')) => {
                self.start(Flow::Provision);
            }
            (DashboardView::Runners, crossterm::event::KeyCode::Char('R')) => {
                self.start(Flow::Rotate);
            }
            (DashboardView::Runners, crossterm::event::KeyCode::Char('x')) => {
                self.start(Flow::Revoke);
            }
            (DashboardView::Runners, crossterm::event::KeyCode::Char('g')) => {
                self.start(Flow::Grant);
            }
            (DashboardView::Runners, crossterm::event::KeyCode::Char('G')) => {
                self.start(Flow::Ungrant);
            }
            (DashboardView::Runners, crossterm::event::KeyCode::Char('a')) => {
                self.start(Flow::Addr);
            }
            (DashboardView::Runners, crossterm::event::KeyCode::Char('v')) => {
                self.start(Flow::Channel);
            }
            // (Running-mode Esc is handled in App::on_key — it closes the
            // channel overlay / cancels without quitting.)
            _ => {}
        }
    }

    pub fn is_typing(&self) -> bool {
        self.cp.prompt.is_some()
    }

    fn refresh_now(&mut self) {
        if let Some(cfg) = self.cfg.clone() {
            self.probe(&cfg);
            self.services = build_services(&cfg, &self.probes);
            self.services_at = Instant::now();
        }
        self.last_agents = Instant::now() - Duration::from_secs(16);
        self.cp.last_fetch = Instant::now() - Duration::from_secs(3);
        self.refresh();
        self.cp.notice = "refreshed".into();
    }

    fn launch_web(&mut self) {
        let Some(client) = self.cp.client.as_ref() else {
            self.cp.notice = "not logged in — press l first (the web needs a session)".into();
            return;
        };
        match client.portal_url() {
            Ok(url) => {
                // only claim "opened" when the browser actually launched —
                // no xdg-open on this box => the URL stays as the manual
                // fallback (single-use token, 60s).
                let spawned = std::process::Command::new("xdg-open")
                    .arg(&url)
                    .stdout(std::process::Stdio::null())
                    .stderr(std::process::Stdio::null())
                    .spawn();
                match spawned {
                    Ok(_) => self.cp.notice = format!("web opened: {url}"),
                    Err(e) => {
                        self.cp.notice =
                            format!("no browser launcher (xdg-open: {e}) — open manually: {url}");
                    }
                }
            }
            Err(e) => self.cp.notice = format!("web launch failed: {e}"),
        }
    }

    fn start(&mut self, flow: Flow) {
        if self.cp.view == PanelView::Local {
            self.cp.notice = "local is a view-only list — t to the remote console to manage".into();
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
            self.services = build_services(&cfg, &self.probes);
            self.services_at = Instant::now();
            self.refresh();
        }
        // Auto-login retry: the console can still be warming — keep
        // attempting until a session is live (no l press needed).
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
}

// ---------------------------------------------------------------------------
// The dashboard views.
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum DashboardView {
    /// named agents stood up so far.
    Agents,
    /// everything running: name / where / status / url.
    #[default]
    Services,
    /// the runner lists (remote console API <-> local loopback).
    Runners,
}

impl DashboardView {
    pub fn next(self) -> Self {
        match self {
            DashboardView::Agents => DashboardView::Services,
            DashboardView::Services => DashboardView::Runners,
            DashboardView::Runners => DashboardView::Agents,
        }
    }

    pub fn prev(self) -> Self {
        match self {
            DashboardView::Agents => DashboardView::Runners,
            DashboardView::Services => DashboardView::Agents,
            DashboardView::Runners => DashboardView::Services,
        }
    }
}

/// One service row: what's provisioned + running, from the config's
/// `managed` pieces. relay/cp carry their LXC coords; future pieces
/// (k3s, litellm, …) appear as soon as their coordinates land in the config.
pub struct ServiceRow {
    pub name: String,
    pub location: String,
    /// None = not provisioned (yet).
    pub status: Option<bool>,
    pub url: String,
}

fn build_services(cfg: &Config, probes: &[(String, bool)]) -> Vec<ServiceRow> {
    let status_of = |key: &str| probes.iter().find(|(l, _)| l == key).map(|(_, ok)| *ok);
    let location = |vmid: &Option<u32>, ip: &Option<String>| match (vmid, ip) {
        (Some(v), Some(i)) => format!("LXC {v} · {i}"),
        (Some(v), None) => format!("LXC {v}"),
        (None, Some(i)) => i.clone(),
        (None, None) => "—".into(),
    };
    cfg.managed
        .iter()
        .map(|piece| match piece.as_str() {
            "relay" => ServiceRow {
                name: "relay".into(),
                location: location(&cfg.lxc.relay.vmid, &cfg.lxc.relay.ip),
                status: status_of("relay"),
                url: cfg.relay_url.clone(),
            },
            "cp" => ServiceRow {
                name: "control plane".into(),
                location: location(&cfg.lxc.cp.vmid, &cfg.lxc.cp.ip),
                status: status_of("control plane"),
                url: cfg.cp_url.clone(),
            },
            "k3s" => ServiceRow {
                name: "k3s (kube)".into(),
                location: location(&cfg.lxc.k3s.vmid, &cfg.lxc.k3s.ip),
                status: status_of("k3s"),
                url: cfg
                    .lxc
                    .k3s
                    .ip
                    .as_ref()
                    .map(|ip| format!("https://{}:6443", ip.split('/').next().unwrap_or(ip)))
                    .unwrap_or_else(|| "—".into()),
            },
            other => ServiceRow {
                name: other.into(),
                location: "—".into(),
                status: None,
                url: "—".into(),
            },
        })
        .collect()
}

/// A named AI agent registered with the console: the relay-addressable
/// pubkey, when it was stood up, and its LIVE availability (relay presence).
pub struct AgentRow {
    pub name: String,
    pub pubkey: String,
    pub created: String,
    pub available: Option<bool>,
    /// the console's probe failure, when available is None.
    pub note: Option<String>,
}

/// "just now" / "2s ago" / "14s ago" — the per-view last-refreshed stamps.
pub fn secs_ago(at: &Instant) -> String {
    let s = at.elapsed().as_secs();
    if s < 2 {
        "just now".into()
    } else {
        format!("{s}s ago")
    }
}

/// "just now" / "3m ago" / "2h ago" / "5d ago" — enough for the agents view.
fn humanize(secs: u64) -> String {
    if secs < 60 {
        "just now".into()
    } else if secs < 3600 {
        format!("{}m ago", secs / 60)
    } else if secs < 86400 {
        format!("{}h ago", secs / 3600)
    } else {
        format!("{}d ago", secs / 86400)
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

/// Which runner list the panel is showing.
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
    /// the local loopback runner list (built when `view == PanelView::Local`).
    pub local: Vec<LocalRunner>,
    pub auth: AuthState,
    pub auth_reason: String,
    /// transient feedback from the last action/refresh.
    pub notice: String,
    last_fetch: Instant,
    /// the last auto-login attempt — the login RETRIES while the console is
    /// still warming so the running screen really is logged in.
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
/// packages it points at. Missing state => empty (the panel says so). A
/// runner whose record never captured an address falls back to the
/// CONFIGURED provisioning runner address (the same runner in practice).
fn read_local(cfg: &Option<Config>) -> Vec<LocalRunner> {
    use freehold_control_plane::state::{RunnerStatus, StateStore};
    let fallback_addr = cfg
        .as_ref()
        .map(|c| c.runner.addr.clone())
        .unwrap_or_default();
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
            let addr = rec
                .mcp_addr
                .clone()
                .or_else(|| fallback_addr.clone().into());
            let reachable = addr.map(|a| port_open(&a)).unwrap_or(false);
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
