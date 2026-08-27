//! The app shell: mode state machine, event loop, and rendering.

use crate::modes::{
    bootstrap::{Bootstrap, Status},
    configure::{CStatus, ConfigureState},
    running::{AuthState, DashboardView, PanelView, Running, clip, secs_ago},
};
use anyhow::Result;
use crossterm::event::{self, Event, KeyCode, KeyEventKind};
use freehold_installer::config::{Config, Mode, probe_mode};
use ratatui::layout::{Constraint, Layout, Rect};
use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span};
use ratatui::widgets::{Block, Borders, Paragraph, Wrap};
use ratatui::{DefaultTerminal, Frame};
use std::path::PathBuf;
use std::time::Duration;

pub struct App {
    pub mode: Mode,
    pub cfg_path: PathBuf,
    pub bs: Bootstrap,
    pub cf: ConfigureState,
    pub rn: Running,
    pub quit: bool,
    /// set at the first VALID ssh auth (door verify on bootstrap; the first
    /// successful lane exec during configure) — the install timer's origin.
    pub first_auth: Option<std::time::Instant>,
    /// the install total, FROZEN the moment the running screen appears.
    pub install_time: Option<std::time::Duration>,
}

impl App {
    pub fn new(cfg_path: PathBuf) -> Result<App> {
        let mode = probe_mode(&cfg_path)?;
        Ok(App {
            cfg_path: cfg_path.clone(),
            bs: Bootstrap::new(cfg_path.clone()),
            cf: ConfigureState::new(cfg_path.clone()),
            rn: Running::new(cfg_path.clone()),
            mode,
            quit: false,
            first_auth: None,
            install_time: None,
        })
    }

    fn to(&mut self, mode: Mode) {
        self.mode = mode.clone();
        // Re-read the config on every transition — bootstrap just wrote it.
        match mode {
            Mode::Configure => {
                let cfg = Config::load(&self.cfg_path).ok().flatten();
                self.cf = ConfigureState::with_cfg(
                    self.cfg_path.clone(),
                    cfg.unwrap_or_else(|| Config::from_answers(&self.bs.answers.clone())),
                );
            }
            Mode::Running => {
                // the clock STOPS here — the total is what it was the moment
                // the running screen showed.
                if self.install_time.is_none() {
                    self.install_time = self.first_auth.map(|t0| t0.elapsed());
                }
                let cfg = Config::load(&self.cfg_path).ok().flatten();
                if let Some(c) = cfg {
                    self.rn = Running::with_cfg(self.cfg_path.clone(), c);
                } else {
                    self.rn = Running::new(self.cfg_path.clone());
                }
            }
            Mode::Bootstrap => {}
        }
    }

    pub fn on_key(&mut self, code: KeyCode) {
        // while a console prompt is open, ALL keys belong to it (Esc cancels,
        // Enter submits) — never quit/reconfigure mid-typing.
        if matches!(self.mode, Mode::Running) && self.rn.is_typing() {
            self.rn.on_key(code);
            return;
        }
        match code {
            // 'q' quits only on RENDERED screens — never while typing text
            // (a pubkey/npub can legitimately contain 'q').
            KeyCode::Char('q') if !matches!(self.mode, Mode::Bootstrap) => self.quit = true,
            KeyCode::Esc => self.quit = true,
            KeyCode::Char('c') if matches!(self.mode, Mode::Running) => {
                self.rn.request_configure = true;
            }
            _ => match self.mode {
                Mode::Bootstrap => self.bs.on_key(code),
                Mode::Configure => self.cf.on_key(code),
                Mode::Running => self.rn.on_key(code),
            },
        }
    }

    pub fn tick(&mut self) {
        match self.mode {
            Mode::Bootstrap => {
                self.bs.tick();
                if self.first_auth.is_none() && self.bs.auth_ok {
                    self.first_auth = Some(std::time::Instant::now());
                }
                if self.bs.quit_requested {
                    self.quit = true;
                }
                if self.bs.transitioning {
                    self.bs.transitioning = false;
                    self.to(Mode::Configure);
                }
            }
            Mode::Configure => {
                if self.first_auth.is_none() && self.cf.auth_detected {
                    self.first_auth = Some(std::time::Instant::now());
                }
                if self.cf.tick() {
                    self.cf.transitioning = false;
                    self.to(Mode::Running);
                }
            }
            Mode::Running => {
                self.rn.tick();
                if self.rn.request_configure {
                    self.rn.request_configure = false;
                    self.to(Mode::Configure);
                }
            }
        }
    }
}

pub fn run(terminal: &mut DefaultTerminal, cfg_path: PathBuf) -> Result<()> {
    let mut app = App::new(cfg_path)?;
    loop {
        terminal.draw(|f| draw(f, &mut app))?;
        if event::poll(Duration::from_millis(120))?
            && let Event::Key(k) = event::read()?
            && k.kind == KeyEventKind::Press
        {
            if k.modifiers
                .contains(crossterm::event::KeyModifiers::CONTROL)
                && k.code == KeyCode::Char('c')
            {
                app.quit = true; // Ctrl-C quits from ANY screen (raw mode)
            } else {
                app.on_key(k.code);
            }
        }
        app.tick();
        if app.quit {
            break;
        }
    }
    Ok(())
}

// ------------------------------------------------------------- rendering

fn header<'a>(area: Rect, f: &mut Frame<'a>, mode: &Mode, domain: &str) {
    let mode_str = match mode {
        Mode::Bootstrap => "bootstrapping",
        Mode::Configure => "configuring",
        Mode::Running => "running",
    };
    let title = Line::from(vec![
        Span::styled(
            " freehold ",
            Style::new()
                .fg(Color::Black)
                .bg(Color::Cyan)
                .add_modifier(Modifier::BOLD),
        ),
        Span::raw("  "),
        Span::styled(
            format!("[{mode_str}]"),
            Style::new().fg(Color::Cyan).add_modifier(Modifier::BOLD),
        ),
        Span::raw("  "),
        Span::styled(domain, Style::new().fg(Color::DarkGray)),
    ]);
    f.render_widget(Paragraph::new(title), area);
}

fn footer<'a>(area: Rect, f: &mut Frame<'a>, hint: &str) {
    f.render_widget(
        Paragraph::new(Line::from(Span::styled(
            hint,
            Style::new().fg(Color::DarkGray),
        ))),
        area,
    );
}

fn panel<'a>(title: &str, border: Color) -> Block<'a> {
    Block::default()
        .borders(Borders::ALL)
        .border_style(Style::new().fg(border))
        .title(Span::styled(
            format!(" {title} "),
            Style::new().fg(border).add_modifier(Modifier::BOLD),
        ))
}

pub fn draw<'a>(f: &mut Frame<'a>, app: &mut App) {
    let area = f.area();
    let chunks = Layout::vertical([
        Constraint::Length(1),
        Constraint::Min(0),
        Constraint::Length(1),
    ])
    .split(area);

    let cfg = Config::load(&app.cfg_path).ok().flatten();
    let domain = cfg
        .map(|c| c.domain)
        .unwrap_or_else(|| app.bs.answers.domain.clone());
    header(chunks[0], f, &app.mode, &domain);

    match app.mode {
        Mode::Bootstrap => draw_bootstrap(chunks[1], f, &mut app.bs),
        Mode::Configure => draw_configure(chunks[1], f, &mut app.cf, app.first_auth),
        Mode::Running => draw_running(chunks[1], f, &mut app.rn),
    }

    let hint = match app.mode {
        Mode::Bootstrap => match app.bs.step {
            crate::modes::bootstrap::Step::Form => {
                "↑↓ move · type to edit · Enter next field (last confirms)"
            }
            crate::modes::bootstrap::Step::Operator => "↑↓ choose · Enter confirm · q quit",
            crate::modes::bootstrap::Step::Confirm => "Enter proceed · Esc back · q quit",
            crate::modes::bootstrap::Step::Door => "Enter = key installed · q quit",
            crate::modes::bootstrap::Step::Verify => "Enter = retry the probe · q quit",
            crate::modes::bootstrap::Step::Stages => "stages running — q quits",
            crate::modes::bootstrap::Step::Written => "…",
        },
        Mode::Configure => "r retry failed stages · q quit",
        Mode::Running => {
            // the hint mirrors the ACTIVE view's keys (scoped input).
            match app.rn.view {
                crate::modes::running::DashboardView::Agents => {
                    "Tab/Shift-Tab views · r refresh · agents are view-only for now · q quit"
                }
                crate::modes::running::DashboardView::Services => {
                    "Tab/Shift-Tab views · r refresh · services are view-only for now · q quit"
                }
                crate::modes::running::DashboardView::Runners => {
                    "Tab/Shift-Tab views · r refresh · l login · t local/remote · p provision · R rotate · x revoke · g/G grant · a addr · v channel · w web · c reconfigure · q quit"
                }
            }
        }
    };
    footer(chunks[2], f, hint);
}

fn status_span(status: &Status) -> Span<'static> {
    match status {
        crate::modes::bootstrap::Status::Pending => {
            Span::styled("pending ", Style::new().fg(Color::DarkGray))
        }
        crate::modes::bootstrap::Status::Running => {
            Span::styled("running ", Style::new().fg(Color::Yellow))
        }
        crate::modes::bootstrap::Status::Ok => {
            Span::styled("ok      ", Style::new().fg(Color::Green))
        }
        crate::modes::bootstrap::Status::Failed => {
            Span::styled("failed  ", Style::new().fg(Color::Red))
        }
    }
}

fn draw_bootstrap<'a>(area: Rect, f: &mut Frame<'a>, bs: &mut Bootstrap) {
    use crate::modes::bootstrap::Step;
    match bs.step {
        Step::Form => {
            let block = panel("Welcome to Freehold — a few details", Color::Cyan);
            let inner = block.inner(area);
            f.render_widget(block, area);
            let mut lines = Vec::new();
            for (i, field) in bs.form.fields.iter().enumerate() {
                let sel = i == bs.form.sel;
                let label = Span::styled(
                    format!(" {} ", field.label),
                    Style::new().fg(if sel { Color::Cyan } else { Color::White }),
                );
                let value = Span::styled(
                    format!(" {}", field.value),
                    Style::new().fg(if sel { Color::LightCyan } else { Color::Gray }),
                );
                lines.push(Line::from(vec![label, value]));
            }
            if let Some(e) = &bs.form.err {
                lines.push(Line::from(Span::styled(e, Style::new().fg(Color::Red))));
            }
            f.render_widget(
                Paragraph::new(lines)
                    .block(Block::default().borders(Borders::NONE))
                    .wrap(Wrap { trim: false }),
                inner,
            );
        }
        Step::Operator => {
            let block = panel("Operator identity", Color::Cyan);
            let inner = block.inner(area);
            f.render_widget(block, area);
            let mut lines = vec![
                Line::from(vec![
                    if bs.op_sel == 0 {
                        Span::styled("❯ ", Style::new().fg(Color::Cyan))
                    } else {
                        Span::raw("  ")
                    },
                    Span::raw(" I have a Nostr key already (paste npub or hex)"),
                ]),
                Line::from(vec![
                    if bs.op_sel == 1 {
                        Span::styled("❯ ", Style::new().fg(Color::Cyan))
                    } else {
                        Span::raw("  ")
                    },
                    Span::raw(" Generate one for me (an identity dir we keep for you)"),
                ]),
            ];
            if bs.op_sel == 0 {
                lines.push(Line::from(Span::raw("")));
                lines.push(Line::from(vec![
                    Span::styled(
                        " pubkey: ",
                        Style::new().fg(if bs.op_field == 0 {
                            Color::Cyan
                        } else {
                            Color::DarkGray
                        }),
                    ),
                    Span::styled(
                        bs.op_input.clone(),
                        Style::new().fg(if bs.op_field == 0 {
                            Color::LightCyan
                        } else {
                            Color::Gray
                        }),
                    ),
                ]));
                lines.push(Line::from(vec![
                    Span::styled(
                        " nsec:   ",
                        Style::new().fg(if bs.op_field == 1 {
                            Color::Cyan
                        } else {
                            Color::DarkGray
                        }),
                    ),
                    Span::styled(
                        "•".repeat(bs.op_input.chars().count()),
                        Style::new().fg(if bs.op_field == 1 {
                            Color::Yellow
                        } else {
                            Color::DarkGray
                        }),
                    ),
                ]));
                lines.push(Line::from(Span::styled(
                    "         (optional — persist YOUR key locally (0600) so every launch logs in; must match the pubkey)",
                    Style::new().fg(Color::DarkGray),
                )));
            }
            if let Some(e) = &bs.op_err {
                lines.push(Line::from(Span::styled(e, Style::new().fg(Color::Red))));
            }
            f.render_widget(Paragraph::new(lines), inner);
        }
        Step::Confirm => {
            let block = panel("Does this look right?", Color::Cyan);
            let inner = block.inner(area);
            f.render_widget(block, area);
            let a = &bs.answers;
            let rows = [
                ("host", a.host.clone()),
                ("runner", a.runner.clone()),
                ("serve", a.serve.clone()),
                ("domain", a.domain.clone()),
                (
                    "relay LXC",
                    match &a.relay_ip {
                        Some(ip) => format!("static {ip}"),
                        None => "dhcp".to_string(),
                    },
                ),
                (
                    "cp LXC",
                    match &a.cp_ip {
                        Some(ip) => format!("static {ip}"),
                        None => "dhcp".to_string(),
                    },
                ),
                ("operator pk", a.operator_pk.clone()),
            ];
            let mut rows = rows.to_vec();
            if a.operator_generated {
                rows.push(("identity", a.operator_dir.display().to_string()));
            }
            let mut lines: Vec<Line> = rows
                .iter()
                .map(|(k, v)| {
                    Line::from(vec![
                        Span::styled(format!(" {k:<12}"), Style::new().fg(Color::Cyan)),
                        Span::styled(format!(" {v}"), Style::new().fg(Color::White)),
                    ])
                })
                .collect();
            if a.relay_ip.is_none() || a.cp_ip.is_none() {
                lines.push(Line::from(Span::raw("")));
                lines.push(Line::from(Span::styled(
                    " ⚠  DHCP ASSIGNED IPs — point your DNS/proxy at whatever DHCP gives",
                    Style::new().fg(Color::Yellow).add_modifier(Modifier::BOLD),
                )));
                lines.push(Line::from(Span::styled(
                    "     (the real addresses are recorded in the config right after each boot)",
                    Style::new().fg(Color::Yellow),
                )));
            }
            f.render_widget(Paragraph::new(lines), inner);
        }
        Step::Stages | Step::Door | Step::Verify | Step::Written => {
            let block = panel("Bringing up freehold", Color::Cyan);
            let inner = block.inner(area);
            let chunks = Layout::vertical([Constraint::Min(0), Constraint::Length(4)]).split(inner);
            f.render_widget(block, area);

            let lines: Vec<Line> = bs
                .stages
                .iter()
                .map(|s| {
                    Line::from(vec![
                        status_span(&s.status),
                        Span::styled(s.name.to_string(), Style::new().fg(Color::White)),
                    ])
                })
                .collect();
            f.render_widget(Paragraph::new(lines), chunks[0]);

            let bottom = match bs.step {
                Step::Door => vec![
                    Line::from(Span::styled(
                        " Install the door on the host:",
                        Style::new().fg(Color::Yellow),
                    )),
                    Line::from(Span::raw("")),
                    Line::from(Span::styled(
                        format!("   {}", bs.door_pubkey),
                        Style::new().fg(Color::Green),
                    )),
                    Line::from(Span::styled(
                        format!(
                            " …then press Enter (authorized_keys on {}).",
                            bs.answers.host
                        ),
                        Style::new().fg(Color::DarkGray),
                    )),
                ],
                Step::Verify => {
                    let mut v = vec![
                        Line::from(Span::styled(
                            " The door check failed:",
                            Style::new().fg(Color::Red),
                        )),
                        Line::from(Span::raw("")),
                    ];
                    for l in bs.stages[3].tail.lines().take(2) {
                        v.push(Line::from(Span::styled(
                            format!("   {l}"),
                            Style::new().fg(Color::Gray),
                        )));
                    }
                    if bs.door_tries >= 3 {
                        v.push(Line::from(Span::styled(
                            " (still failing? the runner key may predate the ssh-key fix — wipe ./.freehold and re-run)",
                            Style::new().fg(Color::Yellow),
                        )));
                    }
                    v.push(Line::from(Span::styled(
                        " fix authorized_keys, then press Enter to probe again.",
                        Style::new().fg(Color::DarkGray),
                    )));
                    v
                }
                Step::Written => vec![Line::from(Span::styled(
                    " config written — entering configure mode…",
                    Style::new().fg(Color::Green),
                ))],
                _ => vec![Line::from(Span::styled(
                    " stages run sequentially; failed stages keep their tail inline.",
                    Style::new().fg(Color::DarkGray),
                ))],
            };
            f.render_widget(Paragraph::new(bottom).wrap(Wrap { trim: false }), chunks[1]);
        }
    }
    if !bs.notice.is_empty() {
        let n = bs.notice.clone();
        f.render_widget(
            Paragraph::new(Line::from(Span::styled(n, Style::new().fg(Color::Yellow)))),
            Rect::new(
                area.x,
                area.y + area.height.saturating_sub(2),
                area.width,
                1,
            ),
        );
    }
}

fn cstatus_span(status: &crate::modes::configure::CStatus) -> Span<'static> {
    use crate::modes::configure::CStatus;
    match status {
        CStatus::Pending => Span::styled("pending ", Style::new().fg(Color::DarkGray)),
        CStatus::Check => Span::styled("check   ", Style::new().fg(Color::Yellow)),
        CStatus::Run => Span::styled("running ", Style::new().fg(Color::Yellow)),
        CStatus::Ok => Span::styled("ok      ", Style::new().fg(Color::Green)),
        CStatus::Failed => Span::styled("failed  ", Style::new().fg(Color::Red)),
    }
}

fn draw_configure<'a>(
    area: Rect,
    f: &mut Frame<'a>,
    cf: &mut ConfigureState,
    first_auth: Option<std::time::Instant>,
) {
    let block = panel("Converging the world to the config", Color::Yellow);
    let inner = block.inner(area);
    let chunks = Layout::vertical([Constraint::Min(0), Constraint::Length(5)]).split(inner);
    f.render_widget(block, area);

    let mut lines: Vec<Line> = cf
        .stages
        .iter()
        .map(|s| {
            let timing = match &s.status {
                CStatus::Check | CStatus::Run => format!("  — {:.1}s…", s.elapsed_secs()),
                CStatus::Ok | CStatus::Failed => format!("  ({:.1}s)", s.elapsed_secs()),
                _ => String::new(),
            };
            Line::from(vec![
                cstatus_span(&s.status),
                Span::styled(s.name.to_string(), Style::new().fg(Color::White)),
                Span::styled(timing, Style::new().fg(Color::DarkGray)),
            ])
        })
        .collect();
    for s in cf
        .stages
        .iter()
        .filter(|s| matches!(s.status, CStatus::Failed))
    {
        for l in s.tail.lines().take(3) {
            lines.push(Line::from(Span::styled(
                format!("   {l}"),
                Style::new().fg(Color::Gray),
            )));
        }
    }
    f.render_widget(Paragraph::new(lines), chunks[0]);

    let mut meta = vec![
        Line::from(Span::styled(
            format!(" doing: {}", cf.stages[cf.cur.min(4)].name),
            Style::new().fg(Color::DarkGray),
        )),
        Line::from(Span::styled(
            format!(
                " relays: {} · cp: {} · runner: {}",
                cf.cfg.relay_url, cf.cfg.cp_url, cf.cfg.runner.addr
            ),
            Style::new().fg(Color::DarkGray),
        )),
    ];
    if let Some(t0) = first_auth {
        meta.push(Line::from(Span::styled(
            format!(
                " since first valid auth: {:.1}s",
                t0.elapsed().as_secs_f32()
            ),
            Style::new().fg(Color::Cyan),
        )));
    }
    f.render_widget(Paragraph::new(meta), chunks[1]);

    if !cf.notice.is_empty() {
        let n = cf.notice.clone();
        f.render_widget(
            Paragraph::new(Line::from(Span::styled(n, Style::new().fg(Color::Yellow)))),
            Rect::new(
                area.x + 1,
                area.y + area.height.saturating_sub(3),
                area.width.saturating_sub(2),
                1,
            ),
        );
    }
}

fn draw_running<'a>(area: Rect, f: &mut Frame<'a>, rn: &mut Running) {
    // one-line world strip (the liveness glance; the Good-to-go box is gone).
    let strip = format!(
        " {}",
        rn.probes
            .iter()
            .map(|(label, ok)| format!("{label} {}", if *ok { "●" } else { "○" }))
            .collect::<Vec<_>>()
            .join("  ·  ")
    );
    f.render_widget(
        Paragraph::new(Line::from(Span::styled(
            strip,
            Style::new().fg(Color::DarkGray),
        ))),
        Rect::new(area.x, area.y, area.width, 1),
    );
    let panel_area = Rect::new(
        area.x,
        area.y + 1,
        area.width,
        area.height.saturating_sub(1),
    );
    match rn.view {
        DashboardView::Agents => draw_agents(panel_area, f, rn),
        DashboardView::Services => draw_services(panel_area, f, rn),
        DashboardView::Runners => draw_console(panel_area, f, rn),
    }
}

fn draw_services<'a>(area: Rect, f: &mut Frame<'a>, rn: &Running) {
    let block = panel("services", Color::Cyan);
    let inner = block.inner(area);
    f.render_widget(block, area);
    let mut lines: Vec<Line> = vec![
        Line::from(Span::styled(
            format!(" services · last refreshed {}", secs_ago(&rn.services_at)),
            Style::new().fg(Color::DarkGray),
        )),
        Line::from(Span::styled(
            format!(
                " {}{}{}{}",
                col("service", 18),
                col("where", 24),
                col("status", 8),
                col("url", 40),
            ),
            Style::new().fg(Color::Cyan).add_modifier(Modifier::BOLD),
        )),
    ];
    for svc in &rn.services {
        let (status_ch, color) = match svc.status {
            Some(true) => ("●", Color::Green),
            Some(false) => ("○", Color::Red),
            None => ("—", Color::DarkGray),
        };
        lines.push(Line::from(vec![
            Span::styled(col(&svc.name, 18), Style::new().fg(Color::White)),
            Span::styled(col(&svc.location, 24), Style::new().fg(Color::Gray)),
            Span::styled(col(status_ch, 8), Style::new().fg(color)),
            Span::styled(col(&svc.url, 40), Style::new().fg(Color::LightBlue)),
        ]));
    }
    f.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }), inner);
}

fn draw_agents<'a>(area: Rect, f: &mut Frame<'a>, rn: &Running) {
    let block = panel("agents", Color::Cyan);
    let inner = block.inner(area);
    f.render_widget(block, area);
    let mut lines: Vec<Line> = vec![];
    if rn.cp.client.is_none() {
        lines.push(Line::from(Span::styled(
            " the agent registry lives on the console — log in on Runners (l), then Tab back",
            Style::new().fg(Color::DarkGray),
        )));
    } else {
        lines.push(Line::from(Span::styled(
            format!(
                " AI agents · availability = relay presence (120s) · last refreshed {}",
                secs_ago(&rn.agents_at)
            ),
            Style::new().fg(Color::DarkGray),
        )));
        lines.push(Line::from(Span::styled(
            format!(
                " {}{}{}{}",
                col("agent", 18),
                col("pubkey", 22),
                col("status", 12),
                col("created", 12),
            ),
            Style::new().fg(Color::Cyan).add_modifier(Modifier::BOLD),
        )));
        if rn.agents.is_empty() {
            lines.push(Line::from(Span::styled(
                " no agents standing yet — a stood-up agent (delegate-peer / CPA) registers here",
                Style::new().fg(Color::DarkGray),
            )));
        }
        for a in &rn.agents {
            let (label, color) = match a.available {
                Some(true) => ("● available", Color::Green),
                Some(false) => ("○ unavailable", Color::Red),
                None => ("? unknown", Color::DarkGray),
            };
            let ident = match (&a.available, &a.note) {
                (None, Some(n)) => clip(&format!("? {n}"), 60),
                _ => clip(&a.pubkey, 20),
            };
            lines.push(Line::from(vec![
                Span::styled(col(&a.name, 18), Style::new().fg(Color::White)),
                Span::styled(col(&ident, 22), Style::new().fg(Color::Gray)),
                Span::styled(col(label, 12), Style::new().fg(color)),
                Span::styled(col(&a.created, 12), Style::new().fg(Color::Gray)),
            ]));
        }
    }
    f.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }), inner);
}

fn val_text(v: &serde_json::Value) -> String {
    match v {
        serde_json::Value::String(s) => s.clone(),
        other => other.to_string(),
    }
}

/// The runner's readiness probe: an object of check-label -> state (the
/// runner's OWN self-check, with its traffic lights). An error/note key
/// carries console-side diagnosis.
fn readiness_text(rd: &Option<serde_json::Value>) -> String {
    match rd {
        Some(serde_json::Value::Object(m)) if !m.is_empty() => m
            .iter()
            .map(|(k, v)| format!("{k}: {}", val_text(v)))
            .collect::<Vec<_>>()
            .join(" · "),
        _ => "—".into(),
    }
}

fn grants_text(g: &Option<Vec<String>>) -> String {
    match g {
        None => "pkg unreadable".into(),
        Some(v) if v.is_empty() => "nobody (fail closed)".into(),
        Some(v) => format!(
            "{} · {}…",
            v.len(),
            v[0].chars().take(8).collect::<String>()
        ),
    }
}

/// Fixed-width column: pad to `w` chars, or clip with an ellipsis.
fn col(s: &str, w: usize) -> String {
    if s.chars().count() <= w {
        format!("{s:<w$}")
    } else {
        let head: String = s.chars().take(w.saturating_sub(1)).collect();
        format!("{head}…")
    }
}

fn draw_console<'a>(area: Rect, f: &mut Frame<'a>, rn: &mut Running) {
    let border = match rn.cp.auth {
        AuthState::Live => Color::Cyan,
        AuthState::Failed => Color::Red,
        AuthState::Missing => Color::DarkGray,
    };
    let title = match rn.cp.view {
        PanelView::Remote => "runners · remote console API",
        PanelView::Local => "runners · local loopback",
    };
    let block = panel(title, border);
    let inner = block.inner(area);
    f.render_widget(block, area);

    let mut lines: Vec<Line> = Vec::new();
    if let Some(ch) = &rn.cp.channel {
        for l in ch.lines().take(inner.height.saturating_sub(1) as usize) {
            lines.push(Line::from(Span::styled(
                "  ".to_string() + l,
                Style::new().fg(Color::Gray),
            )));
        }
        lines.push(Line::from(Span::styled(
            "  (Esc closes)",
            Style::new().fg(Color::DarkGray),
        )));
        f.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }), inner);
        return;
    }
    if rn.cp.view == PanelView::Local {
        lines.push(Line::from(Span::styled(
            format!(
                " local loopback · {} · last refreshed {}",
                freehold_installer::freehold_home()
                    .join("control-plane")
                    .display(),
                secs_ago(&rn.runners_at)
            ),
            Style::new().fg(Color::DarkGray),
        )));
        if rn.cp.local.is_empty() {
            lines.push(Line::from(Span::styled(
                " no local runners — nothing provisioned on this machine yet",
                Style::new().fg(Color::DarkGray),
            )));
        } else {
            lines.push(Line::from(Span::styled(
                format!(
                    " {}{}{}{}{}{}",
                    col("runner", 16),
                    col("status", 7),
                    col("risk", 6),
                    col("secret", 30),
                    col("readiness", 34),
                    col("grants", 22),
                ),
                Style::new().fg(Color::Cyan).add_modifier(Modifier::BOLD),
            )));
            for r in &rn.cp.local {
                let secret = r
                    .secret
                    .as_ref()
                    .map(|(n, k, a)| format!("{n} · {k} · {a}"))
                    .unwrap_or_else(|| "—".into());
                let reach = if r.reachable {
                    "reachable"
                } else {
                    "unreachable"
                };
                lines.push(Line::from(vec![
                    Span::styled(col(&r.name, 16), Style::new().fg(Color::White)),
                    Span::styled(
                        col(&r.status, 7),
                        Style::new().fg(if r.status == "revoked" {
                            Color::Red
                        } else {
                            Color::Green
                        }),
                    ),
                    Span::raw(col(r.risk.as_deref().unwrap_or("?"), 6)),
                    Span::raw(col(&secret, 30)),
                    Span::raw(col(reach, 34)),
                    Span::raw(col(&grants_text(&r.grants), 22)),
                ]));
            }
        }
        f.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }), inner);
        render_prompt_notice(inner, f, &mut rn.cp);
        return;
    }
    match rn.cp.auth {
        AuthState::Missing => {
            lines.push(Line::from(Span::styled(
                " not logged in — l login (operator identity dir or FREEHOLD_CONSOLE_COOKIE)",
                Style::new().fg(Color::DarkGray),
            )));
        }
        AuthState::Failed => {
            lines.push(Line::from(Span::styled(
                format!(" login unavailable: {}", clip(&rn.cp.auth_reason, 80)),
                Style::new().fg(Color::Red),
            )));
            lines.push(Line::from(Span::styled(
                "  (l retries with the config identity; FREEHOLD_CONSOLE_COOKIE overrides)",
                Style::new().fg(Color::DarkGray),
            )));
        }
        AuthState::Live => {
            if let Some(c) = &rn.cp.client {
                let pk = c
                    .pubkey()
                    .map(|p| format!("{}…", clip(p, 16)))
                    .unwrap_or_else(|| "session".into());
                lines.push(Line::from(Span::styled(
                    format!(
                        " session: {pk} @ {} · last refreshed {}",
                        c.base(),
                        secs_ago(&rn.runners_at)
                    ),
                    Style::new().fg(Color::DarkGray),
                )));
            }
            match &rn.cp.overview {
                None => lines.push(Line::from(Span::styled(
                    " loading overview…",
                    Style::new().fg(Color::DarkGray),
                ))),
                Some(ov) if ov.runners.is_empty() => lines.push(Line::from(Span::styled(
                    " no runners yet — provision one (p)",
                    Style::new().fg(Color::DarkGray),
                ))),
                Some(ov) => {
                    lines.push(Line::from(Span::styled(
                        format!(
                            " {}{}{}{}{}{}",
                            col("runner", 16),
                            col("status", 7),
                            col("risk", 6),
                            col("secret", 30),
                            col("readiness", 34),
                            col("grants", 22),
                        ),
                        Style::new().fg(Color::Cyan).add_modifier(Modifier::BOLD),
                    )));
                    for r in &ov.runners {
                        let status = if r.status == "revoked" {
                            "revoked"
                        } else {
                            "active"
                        };
                        let secret = r
                            .secret
                            .as_ref()
                            .map(|s| format!("{} · {} · {}", s.name, s.kind, s.address))
                            .unwrap_or_else(|| "—".into());
                        lines.push(Line::from(vec![
                            Span::styled(col(&r.name, 16), Style::new().fg(Color::White)),
                            Span::styled(
                                col(status, 7),
                                Style::new().fg(if r.status == "revoked" {
                                    Color::Red
                                } else {
                                    Color::Green
                                }),
                            ),
                            Span::raw(col(r.risk.as_deref().unwrap_or("?"), 6)),
                            Span::raw(col(&secret, 30)),
                            Span::raw(col(&readiness_text(&r.readiness), 34)),
                            Span::raw(col(&grants_text(&r.grants), 22)),
                        ]));
                    }
                }
            }
        }
    }
    f.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }), inner);

    render_prompt_notice(inner, f, &mut rn.cp);
}

/// The prompt / notice line sits on the panel's last row.
fn render_prompt_notice<'a>(
    inner: Rect,
    f: &mut Frame<'a>,
    cp: &mut crate::modes::running::ConsolePanel,
) {
    if let Some(p) = &cp.prompt {
        let line = format!(" {}: {} ▌", p.label(), p.buf);
        f.render_widget(
            Paragraph::new(Line::from(Span::styled(
                line,
                Style::new().fg(Color::Yellow),
            ))),
            Rect::new(
                inner.x,
                inner.y + inner.height.saturating_sub(1),
                inner.width,
                1,
            ),
        );
    } else if !cp.notice.is_empty() {
        let n = cp.notice.clone();
        f.render_widget(
            Paragraph::new(Line::from(Span::styled(
                format!(" {}", clip(&n, inner.width.saturating_sub(1) as usize)),
                Style::new().fg(Color::Yellow),
            ))),
            Rect::new(
                inner.x,
                inner.y + inner.height.saturating_sub(1),
                inner.width,
                1,
            ),
        );
    }
}
