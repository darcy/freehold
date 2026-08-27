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
    /// Background refresh: every network call lives on a worker thread —
    /// the UI event loop never blocks on a probe/fetch/login, so keys
    /// (Tab / r / anything) stay instant even when a guest is slow.
    ref_: Refresher,
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
            ref_: Refresher::default(),
            last: Instant::now() - Duration::from_secs(5),
            last_agents: Instant::now() - Duration::from_secs(60),
            services_at: Instant::now(),
            agents_at: Instant::now(),
            runners_at: Instant::now(),
            request_configure: false,
        };
        // NO probing on the UI thread — the first frame renders instantly
        // and the worker fills the world on the first drain.
        r.spawn_refresh();
        r
    }

    pub fn with_cfg(cfg_path: PathBuf, cfg: Config) -> Self {
        let mut r = Self::new(cfg_path);
        r.cfg = Some(cfg.clone());
        r
    }

    /// Spawn the background refresh (probes + services + console data all
    /// run off the UI thread; results land in the next drain).
    fn spawn_refresh(&mut self) {
        let cfg = self.cfg.clone();
        let view_local = self.cp.view == PanelView::Local;
        let ui_cookie = self
            .cp
            .client
            .as_ref()
            .and_then(|c| c.cookie().map(str::to_string));
        let needs_login = self.cp.client.is_none();
        let can_login = self
            .cfg
            .as_ref()
            .is_some_and(|c| c.operator_identity.is_some())
            || std::env::var("FREEHOLD_CONSOLE_COOKIE")
                .map(|v| !v.trim().is_empty())
                .unwrap_or(false);
        let agents_due = self.last_agents.elapsed() >= Duration::from_secs(15);
        if agents_due {
            self.last_agents = Instant::now();
        }
        self.last = Instant::now();
        let (tx, rx) = std::sync::mpsc::channel();
        self.ref_.rx = Some(rx);
        self.ref_.handle = Some(std::thread::spawn(move || {
            refresh_worker(
                cfg,
                view_local,
                ui_cookie,
                needs_login,
                can_login,
                agents_due,
                tx,
            )
        }));
    }

    /// A fresh session for a worker that needs one (the operator identity /
    /// cookie path — the SAME logic attach_console used, minus the
    /// UI-thread mutation).
    fn build_session(cfg: &Config) -> (Option<Client>, AuthState, String) {
        if let Ok(cookie) = std::env::var("FREEHOLD_CONSOLE_COOKIE")
            && !cookie.trim().is_empty()
        {
            return (
                Some(Client::with_cookie(&cfg.cp_url, &cookie)),
                AuthState::Live,
                "FREEHOLD_CONSOLE_COOKIE session".into(),
            );
        }
        let Some(dir) = &cfg.operator_identity else {
            return (
                None,
                AuthState::Missing,
                "no operator identity in config and no FREEHOLD_CONSOLE_COOKIE — l retries; \
                 see README console-login"
                    .into(),
            );
        };
        match Identity::load(dir) {
            Ok(id) => {
                let seed = id.secret_seed();
                let url = cfg.cp_url.clone();
                match Client::login_with_timeout(&url, &seed, Duration::from_secs(6)) {
                    Ok(c) => (
                        Some(c),
                        AuthState::Live,
                        format!("operator @ {}", dir.display()),
                    ),
                    Err(e) => (None, AuthState::Failed, format!("login: {e}")),
                }
            }
            Err(e) => (
                None,
                AuthState::Failed,
                format!("identity {}: {e}", dir.display()),
            ),
        }
    }

    /// Apply whatever the worker finished (all state writes land here, on
    /// the UI thread, between frames).
    fn drain_refresh(&mut self) {
        let done = self.ref_.handle.as_ref().is_some_and(|h| h.is_finished());
        if !done {
            return;
        }
        if let Some(h) = self.ref_.handle.take()
            && h.join().is_ok()
            && let Some(rx) = self.ref_.rx.take()
            && let Ok(snap) = rx.try_recv()
        {
            self.apply_snapshot(snap);
        }
        self.ref_.handle = None;
        self.ref_.rx = None;
    }

    fn apply_snapshot(&mut self, snap: Snapshot) {
        self.probes = snap.probes;
        self.services = snap.services;
        self.services_at = snap.services_at;
        self.agents = snap.agents;
        self.agents_at = snap.agents_at;
        if snap.session_dead {
            self.cp.client = None;
            self.cp.auth = AuthState::Failed;
            self.cp.auth_reason = "session expired — press l to log in again".into();
        }
        // a worker-minted fresh session gets installed; reusing the UI's
        // cookie carries None (keep the existing client).
        if let Some((client, auth, reason)) = snap.session {
            if client.is_some() {
                self.cp.client = client;
            }
            self.cp.auth = auth;
            self.cp.auth_reason = reason;
        }
        if snap.view_local {
            self.cp.local = snap.local;
            self.runners_at = snap.runners_at;
        }
        if let Some(ov) = snap.overview {
            self.cp.overview = Some(ov);
            self.runners_at = snap.runners_at;
        }
        if let Some(n) = snap.notice {
            self.cp.notice = n;
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
                self.cp.last_login_attempt = Instant::now() - Duration::from_secs(9);
                if !self.ref_.busy() {
                    self.spawn_refresh();
                }
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
                self.refresh_now();
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
        if self.ref_.busy() {
            return;
        }
        self.last_agents = Instant::now() - Duration::from_secs(16);
        self.last = Instant::now() - Duration::from_secs(3);
        self.cp.last_login_attempt = Instant::now() - Duration::from_secs(9);
        self.spawn_refresh();
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
                self.refresh_now();
            }
            Err(e) => self.cp.notice = format!("{} failed: {e}", name()),
        }
    }

    pub fn tick(&mut self) {
        // land whatever the worker finished FIRST (fresh data renders the
        // next frame), then decide whether to spin another.
        self.drain_refresh();
        let due = self.last.elapsed() >= Duration::from_secs(2);
        let needs_login = self.cp.client.is_none()
            && self.cp.auth != AuthState::Live
            && self.cp.last_login_attempt.elapsed() >= Duration::from_secs(8);
        if (due || needs_login) && !self.ref_.busy() {
            if needs_login {
                self.cp.last_login_attempt = Instant::now();
            }
            self.spawn_refresh();
        }
    }
}

/// Everything a refresh worker computes OFF the UI thread. The UI owns the
/// final state; the worker only produces facts.
struct Snapshot {
    probes: Vec<(String, bool)>,
    services: Vec<ServiceRow>,
    agents: Vec<AgentRow>,
    local: Vec<LocalRunner>,
    overview: Option<freehold_console_client::Overview>,
    /// a fresh session for the UI to install (None keeps the existing one).
    session: Option<(Option<Client>, AuthState, String)>,
    session_dead: bool,
    view_local: bool,
    notice: Option<String>,
    services_at: Instant,
    agents_at: Instant,
    runners_at: Instant,
}

#[derive(Default)]
struct Refresher {
    rx: Option<std::sync::mpsc::Receiver<Snapshot>>,
    handle: Option<std::thread::JoinHandle<()>>,
}

impl Refresher {
    fn busy(&self) -> bool {
        self.handle.as_ref().is_some_and(|h| !h.is_finished())
    }
}

/// The background refresh body: probes, services, the console overview +
/// agents (or the local loopback list), and a fresh session when needed.
/// Runs to completion on a worker thread; the event loop only drains.
fn refresh_worker(
    cfg: Option<Config>,
    view_local: bool,
    ui_cookie: Option<String>,
    needs_login: bool,
    can_login: bool,
    agents_due: bool,
    tx: std::sync::mpsc::Sender<Snapshot>,
) {
    let mut snap = Snapshot {
        probes: Vec::new(),
        services: Vec::new(),
        agents: Vec::new(),
        local: Vec::new(),
        overview: None,
        session: None,
        session_dead: false,
        view_local,
        notice: None,
        services_at: Instant::now(),
        agents_at: Instant::now(),
        runners_at: Instant::now(),
    };
    let Some(cfg) = cfg else {
        let _ = tx.send(snap);
        return;
    };
    // a session: reuse the UI's cookie (same session, no re-login) or mint
    // a fresh one (the operator key) when the UI has none.
    let (fetch_client, install, auth, reason) = if let Some(cookie) = ui_cookie {
        (
            Some(Client::with_cookie(&cfg.cp_url, &cookie)),
            None,
            AuthState::Live,
            "session cookie".into(),
        )
    } else if needs_login && can_login {
        let (client, auth, reason) = Running::build_session(&cfg);
        (client.clone(), client, auth, reason)
    } else {
        (None, None, AuthState::Missing, String::new())
    };
    snap.session = Some((install, auth, reason));
    // probes + services (the world strip):
    snap.probes = vec![
        ("relay".into(), relay_live(&cfg)),
        ("control plane".into(), cp_live(&cfg)),
        ("k3s".into(), k3s_live(&cfg)),
        ("provisioning runner".into(), port_open(&cfg.runner.addr)),
    ];
    snap.services = build_services(&cfg, &snap.probes);
    snap.services_at = Instant::now();
    // data: the local list or the console (overview + agents).
    if view_local {
        snap.local = read_local(&Some(cfg));
        snap.runners_at = Instant::now();
    } else if let Some(client) = &fetch_client {
        match client.overview() {
            Ok(ov) => {
                snap.overview = Some(ov);
                snap.runners_at = Instant::now();
            }
            Err(freehold_console_client::Error::Api { status: 401, .. }) => {
                snap.session_dead = true;
            }
            Err(e) => snap.notice = Some(format!("overview: {e}")),
        }
        if agents_due {
            match client.agents() {
                Ok(list) => {
                    snap.agents_at = Instant::now();
                    let now = freehold_core::auth::now_secs();
                    snap.agents = list
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
                Err(e) => snap.notice = Some(format!("agents: {e}")),
            }
        }
    }
    let _ = tx.send(snap);
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
