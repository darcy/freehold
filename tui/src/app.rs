//! The app shell: mode state machine, event loop, and rendering.

use crate::modes::{
    bootstrap::{Bootstrap, Status},
    configure::{CStatus, ConfigureState},
    running::Running,
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
        Mode::Running => draw_running(chunks[1], f, &mut app.rn, app.install_time),
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
        Mode::Running => "c reconfigure · q quit",
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
                lines.push(Line::from(Span::styled(
                    format!(" pubkey: {}", bs.op_input),
                    Style::new().fg(Color::LightCyan),
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
            format!(" doing: {}", cf.stages[cf.cur.min(3)].name),
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

fn draw_running<'a>(
    area: Rect,
    f: &mut Frame<'a>,
    rn: &mut Running,
    install_time: Option<std::time::Duration>,
) {
    let color = if rn.all_ok() {
        Color::Green
    } else {
        Color::Yellow
    };
    let block = panel(
        if rn.all_ok() {
            "Good to go!"
        } else {
            "still bringing things up…"
        },
        color,
    );
    let inner = block.inner(area);
    f.render_widget(block, area);

    let mut lines = vec![
        Line::from(vec![
            Span::styled("        ", Style::new().fg(color)),
            Span::styled(
                "Everything is running",
                Style::new().fg(color).add_modifier(Modifier::BOLD),
            ),
            Span::raw("  — freehold is up."),
        ]),
        Line::from(Span::raw("")),
    ];
    for (label, ok) in &rn.probes {
        lines.push(Line::from(vec![
            Span::styled(
                if *ok { "● " } else { "○ " },
                Style::new().fg(if *ok { Color::Green } else { Color::Red }),
            ),
            Span::styled(label.to_string(), Style::new().fg(Color::White)),
        ]));
    }
    if let Some(t) = install_time {
        lines.push(Line::from(Span::raw("")));
        lines.push(Line::from(Span::styled(
            format!("  door → good to go: {:.1}s", t.as_secs_f32()),
            Style::new().fg(Color::Cyan).add_modifier(Modifier::BOLD),
        )));
    }
    if let Some(cfg) = &rn.cfg {
        lines.push(Line::from(Span::raw("")));
        lines.push(Line::from(Span::styled(
            format!("  relay:   {}", cfg.relay_url),
            Style::new().fg(Color::DarkGray),
        )));
        lines.push(Line::from(Span::styled(
            format!("  console: {}", cfg.cp_url),
            Style::new().fg(Color::DarkGray),
        )));
        lines.push(Line::from(Span::styled(
            format!("  runner:  {} (proxmox-box)", cfg.runner.addr),
            Style::new().fg(Color::DarkGray),
        )));
    }
    f.render_widget(Paragraph::new(lines), inner);
}
