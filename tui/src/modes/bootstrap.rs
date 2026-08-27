//! Bootstrap mode — no config yet: collect the decisions on a form, confirm,
//! then run the bring-up stages (provision → door → grant → serve → verify)
//! and write `~/.config/freehold/config.toml`.

use super::StageRunner;
use freehold_installer::{
    Answers, DoorProbe, config, mint_identity, ops_pubkey, stage_grant, stage_provision,
    stage_serve, verify_door_once,
};
use std::path::PathBuf;

#[derive(Clone)]
pub struct Field {
    pub label: &'static str,
    pub value: String,
}

#[derive(Clone)]
pub struct Form {
    pub fields: Vec<Field>,
    pub sel: usize,
    pub err: Option<String>,
}

impl Form {
    pub fn new() -> Self {
        let d = Answers::defaults();
        Self {
            fields: vec![
                Field {
                    label: "Proxmox host (the runner SSHs in)",
                    value: d.host,
                },
                Field {
                    label: "Runner name",
                    value: d.runner,
                },
                Field {
                    label: "Runner MCP address (loopback)",
                    value: d.serve,
                },
                Field {
                    label: "Relay domain (must resolve to your host)",
                    value: d.domain,
                },
                Field {
                    label: "Relay LXC IP (empty = DHCP)",
                    value: d.relay_ip.unwrap_or_default(),
                },
                Field {
                    label: "Control-plane LXC IP (empty = DHCP)",
                    value: d.cp_ip.unwrap_or_default(),
                },
                Field {
                    label: "LXC gateway (static only)",
                    value: d.relay_gw,
                },
                Field {
                    label: "LXC rootfs size (GB)",
                    value: d.rootfs_gb.to_string(),
                },
                Field {
                    label: "LXC memory (MB)",
                    value: d.memory_mb.to_string(),
                },
            ],
            sel: 0,
            err: None,
        }
    }

    pub fn into_answers(self) -> Result<Answers, String> {
        let v = |i: usize| self.fields[i].value.trim().to_string();
        let num = |i: usize| {
            self.fields[i]
                .value
                .trim()
                .parse::<u32>()
                .map_err(|_| format!("{} must be a number", self.fields[i].label))
        };
        Ok(Answers {
            host: v(0),
            runner: v(1),
            serve: v(2),
            domain: v(3),
            // vmids auto-picked (recorded after boot); a SPECIFIED ip is
            // STATIC (proxy/DNS must target it), empty = DHCP.
            relay_vmid: None,
            relay_ip: opt_ip(&self.fields[4].value),
            cp_vmid: None,
            cp_ip: opt_ip(&self.fields[5].value),
            relay_gw: v(6),
            rootfs_gb: num(7)?,
            memory_mb: num(8)?,
            operator_pk: String::new(),
            operator_generated: false,
            operator_dir: PathBuf::new(),
        })
    }
}

#[derive(Clone, PartialEq, Debug)]
pub enum Status {
    Pending,
    Running,
    Ok,
    Failed,
}

pub struct StageUi {
    pub name: &'static str,
    pub status: Status,
    pub tail: String,
}

#[derive(Clone, PartialEq, Debug)]
pub enum Step {
    Form,
    /// Operator identity: pick have-a-key / generate.
    Operator,
    Confirm,
    /// Sequential stage list (provision → … → write config).
    Stages,
    /// New SSH key printed — wait for the operator to install it.
    Door,
    /// Door probe failed — wait for a retry.
    Verify,
    Written,
}

#[derive(Clone, PartialEq, Debug)]
enum Job {
    None,
    Mint,
    Stage(usize),
    Probe,
}

/// The stage closures — a seam for hermetic mechanism tests (the defaults
/// are the real freehold_installer stages driving the world binaries).
type FnWithPk = std::sync::Arc<dyn Fn(&Answers, &str) -> anyhow::Result<String> + Send + Sync>;
type FnSimple = std::sync::Arc<dyn Fn(&Answers) -> anyhow::Result<String> + Send + Sync>;

#[derive(Clone)]
pub struct StageFns {
    pub provision: FnWithPk,
    pub grant: FnSimple,
    pub serve: FnSimple,
    pub verify: FnSimple,
}

impl Default for StageFns {
    fn default() -> Self {
        Self {
            provision: std::sync::Arc::new(|a, pk| {
                let pubk = stage_provision(a, pk)?;
                Ok(format!("pub:{}", pubk.unwrap_or_default()))
            }),
            grant: std::sync::Arc::new(|a| {
                stage_grant(a)?;
                Ok(format!("ops agent granted on {}", a.runner))
            }),
            serve: std::sync::Arc::new(stage_serve),
            verify: std::sync::Arc::new(|a| match verify_door_once(a)? {
                DoorProbe::Ok => Ok("ok".into()),
                DoorProbe::AuthFailed(t) => Ok(format!("auth:{t}")),
                DoorProbe::Failed(t) => Ok(format!("fail:{t}")),
            }),
        }
    }
}

pub struct Bootstrap {
    pub cfg_path: PathBuf,
    pub step: Step,
    pub form: Form,
    /// Operator identity: 0 = have a key, 1 = generate.
    pub op_sel: usize,
    pub op_input: String,
    pub op_err: Option<String>,
    pub answers: Answers,
    pub stages: Vec<StageUi>,
    pub cur: usize,
    pub door_pubkey: String,
    pub door_tries: u32,
    pub notice: String,
    pub transitioning: bool,
    pub quit_requested: bool,
    /// valid ssh auth proven (a real exec through the runner returned ok) —
    /// the install timer starts here on a fresh bootstrap.
    pub auth_ok: bool,
    runner: StageRunner,
    job: Job,
    fns: StageFns,
}

impl Bootstrap {
    pub fn new(cfg_path: PathBuf) -> Self {
        Self::with_fns(cfg_path, StageFns::default())
    }

    fn with_fns(cfg_path: PathBuf, fns: StageFns) -> Self {
        Self {
            cfg_path,
            step: Step::Form,
            form: Form::new(),
            op_sel: 0,
            op_input: String::new(),
            op_err: None,
            answers: Answers::defaults(),
            stages: vec![
                StageUi {
                    name: "provision runner (SSH door)",
                    status: Status::Pending,
                    tail: String::new(),
                },
                StageUi {
                    name: "grant the ops agent",
                    status: Status::Pending,
                    tail: String::new(),
                },
                StageUi {
                    name: "start the runner in the background",
                    status: Status::Pending,
                    tail: String::new(),
                },
                StageUi {
                    name: "verify the SSH door (real exec)",
                    status: Status::Pending,
                    tail: String::new(),
                },
                StageUi {
                    name: "write ~/.config/freehold/config.toml",
                    status: Status::Pending,
                    tail: String::new(),
                },
            ],
            cur: 0,
            door_pubkey: String::new(),
            door_tries: 0,
            notice: String::new(),
            transitioning: false,
            quit_requested: false,
            auth_ok: false,
            runner: StageRunner::new(),
            job: Job::None,
            fns,
        }
    }

    // ------------------------------------------------------------ input

    pub fn on_key(&mut self, code: crossterm::event::KeyCode) {
        match &self.step {
            Step::Form => self.form_key(code),
            Step::Operator => self.op_key(code),
            Step::Confirm => match code {
                crossterm::event::KeyCode::Enter => self.begin_stages(),
                crossterm::event::KeyCode::Esc => self.step = Step::Operator,
                crossterm::event::KeyCode::Char('q') => {
                    self.quit_requested = true;
                }
                _ => {}
            },
            Step::Door => match code {
                crossterm::event::KeyCode::Enter => {
                    self.step = Step::Stages;
                    self.advance();
                }
                crossterm::event::KeyCode::Char('q') | crossterm::event::KeyCode::Esc => {
                    self.notice =
                        "aborted — door not installed (add the key, then re-run freehold)".into();
                    self.quit_requested = true;
                }
                _ => {}
            },
            Step::Verify => match code {
                crossterm::event::KeyCode::Enter => self.spawn_probe(),
                crossterm::event::KeyCode::Char('q') | crossterm::event::KeyCode::Esc => {
                    self.notice = "aborted at the door check".into();
                    self.quit_requested = true;
                }
                _ => {}
            },
            _ => {}
        }
    }

    fn form_key(&mut self, code: crossterm::event::KeyCode) {
        self.form.err = None;
        match code {
            crossterm::event::KeyCode::Up => self.form.sel = self.form.sel.saturating_sub(1),
            crossterm::event::KeyCode::Down | crossterm::event::KeyCode::Enter => {
                if self.form.sel + 1 < self.form.fields.len() {
                    self.form.sel += 1;
                } else {
                    match self.form.clone().into_answers() {
                        Ok(a) => {
                            self.answers = a;
                            self.step = Step::Operator;
                        }
                        Err(e) => self.form.err = Some(e),
                    }
                }
            }
            crossterm::event::KeyCode::Backspace => {
                self.form.fields[self.form.sel].value.pop();
            }
            crossterm::event::KeyCode::Char('u') => self.form.fields[self.form.sel].value.clear(),
            crossterm::event::KeyCode::Char(c) => self.form.fields[self.form.sel].value.push(c),
            _ => {}
        }
    }

    fn op_key(&mut self, code: crossterm::event::KeyCode) {
        self.op_err = None;
        match code {
            crossterm::event::KeyCode::Up | crossterm::event::KeyCode::Down => {
                if self.op_input.is_empty() {
                    self.op_sel = 1 - self.op_sel;
                }
            }
            crossterm::event::KeyCode::Enter => {
                if self.op_sel == 0 {
                    if self.op_input.trim().is_empty() {
                        self.op_err = Some("paste your npub1… or 64-hex pubkey".into());
                        return;
                    }
                    match freehold_core::identity::parse_pubkey_input(&self.op_input) {
                        Ok(pk) => {
                            self.answers.operator_pk = pk;
                            self.step = Step::Confirm;
                        }
                        Err(e) => self.op_err = Some(format!("invalid pubkey: {e}")),
                    }
                } else {
                    self.answers.operator_generated = true;
                    self.answers.operator_dir = freehold_installer::operator_dir();
                    self.job = Job::Mint;
                    let dir = self.answers.operator_dir.clone();
                    self.runner.spawn(move || {
                        let id = mint_identity(&dir)?;
                        Ok(format!("{}|{}", id.nostr_pubkey_hex(), dir.display()))
                    });
                }
            }
            crossterm::event::KeyCode::Backspace => {
                self.op_input.pop();
            }
            crossterm::event::KeyCode::Char(c) => self.op_input.push(c),
            _ => {}
        }
    }

    // ------------------------------------------------------------- flow

    fn begin_stages(&mut self) {
        self.step = Step::Stages;
        self.cur = 0;
        self.advance();
    }

    /// Spawn the stage at `self.cur` (the Done handler advanced the pointer;
    /// door/verify are interactive pauses that don't).
    fn advance(&mut self) {
        if self.cur >= self.stages.len() {
            return;
        }
        let answers = self.answers.clone();
        match self.cur {
            0 => {
                self.stages[0].status = Status::Running;
                self.job = Job::Stage(0);
                let pk = ops_pubkey().unwrap_or_default();
                let f = self.fns.provision.clone();
                self.runner.spawn(move || f(&answers, &pk));
            }
            1 => {
                self.stages[1].status = Status::Running;
                self.job = Job::Stage(1);
                let f = self.fns.grant.clone();
                self.runner.spawn(move || f(&answers));
            }
            2 => {
                self.stages[2].status = Status::Running;
                self.job = Job::Stage(2);
                let f = self.fns.serve.clone();
                self.runner.spawn(move || f(&answers));
            }
            3 => self.spawn_probe(),
            4 => {
                self.stages[4].status = Status::Running;
                self.job = Job::Stage(4);
                let cfg = config::Config::from_answers(&answers);
                let path = self.cfg_path.clone();
                self.runner.spawn(move || {
                    cfg.save(&path)?;
                    Ok(format!("wrote {}", path.display()))
                });
            }
            _ => {}
        }
    }

    fn spawn_probe(&mut self) {
        match &self.step {
            Step::Verify | Step::Stages => {}
            _ => return,
        }
        self.stages[3].status = Status::Running;
        self.job = Job::Probe;
        let answers = self.answers.clone();
        let f = self.fns.verify.clone();
        self.runner.spawn(move || f(&answers));
    }

    /// Drain worker messages; called every render tick.
    pub fn tick(&mut self) {
        while let Ok(msg) = self.runner.rx.try_recv() {
            match msg {
                super::RawMsg::Done { ok, out } => match self.job.clone() {
                    Job::Mint => {
                        // answered even on failure: op_err → stay on Operator
                        if ok {
                            let mut it = out.splitn(2, '|');
                            let pk = it.next().unwrap_or_default().to_string();
                            let dir = it.next().unwrap_or_default().to_string();
                            self.answers.operator_pk = pk;
                            self.answers.operator_dir = PathBuf::from(&dir);
                            self.step = Step::Confirm;
                        } else {
                            self.op_err = Some(out);
                            self.answers.operator_generated = false;
                        }
                        self.job = Job::None;
                    }
                    Job::Probe => {
                        self.door_tries += 1;
                        if out == "ok" {
                            self.auth_ok = true;
                            self.stages[3].status = Status::Ok;
                            self.job = Job::None;
                            self.cur = 4;
                            self.advance();
                        } else {
                            let (fail, tail) = if let Some(t) = out.strip_prefix("auth:") {
                                (true, format!("authentication failed:\n{t}"))
                            } else if let Some(t) = out.strip_prefix("fail:") {
                                (true, format!("exec failed:\n{t}"))
                            } else {
                                (false, out.clone())
                            };
                            if !fail {
                                self.stages[3].status = Status::Ok;
                                self.job = Job::None;
                                self.cur = 4;
                                self.advance();
                            } else {
                                self.stages[3].status = Status::Failed;
                                self.stages[3].tail = tail;
                                self.step = Step::Verify;
                                self.job = Job::None;
                            }
                        }
                    }
                    Job::Stage(i) => {
                        {
                            let st = &mut self.stages[i];
                            if ok {
                                st.status = Status::Ok;
                                st.tail = out;
                            } else {
                                st.status = Status::Failed;
                                st.tail = out;
                            }
                        }
                        self.job = Job::None;
                        // the door: a FRESH key needs installing before grant
                        let door: Option<String> = if i == 0 {
                            self.stages[0]
                                .tail
                                .strip_prefix("pub:")
                                .map(|s| s.to_string())
                                .filter(|pk| !pk.is_empty())
                        } else {
                            None
                        };
                        // advance the pointer, then (re)start the next stage
                        self.cur = i + 1;
                        if let Some(pk) = door {
                            self.door_pubkey = pk;
                            self.step = Step::Door;
                            return;
                        }
                        self.advance();
                    }
                    Job::None => {}
                },
            }
        }
        if self.step == Step::Written && !self.transitioning {
            self.transitioning = true;
        }
        if self.step == Step::Stages && self.cur >= 4 && self.stages[4].status == Status::Ok {
            self.step = Step::Written;
            self.advance(); // no-op; signals transition
        }
    }
}

/// empty = DHCP; a bare "192.168.30.8" is normalized to a /24 CIDR.
fn opt_ip(raw: &str) -> Option<String> {
    let t = raw.trim();
    if t.is_empty() {
        return None;
    }
    Some(if t.contains('/') {
        t.to_string()
    } else {
        format!("{t}/24")
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pipeline_advances_with_canned_stages() {
        let cfg = std::env::temp_dir().join("fh-hermetic.toml");
        let _ = std::fs::remove_file(&cfg);
        let fns = StageFns {
            provision: std::sync::Arc::new(|_, _| Ok("pub:".into())),
            grant: std::sync::Arc::new(|a| Ok(format!("granted {}", a.runner))),
            serve: std::sync::Arc::new(|a| Ok(a.serve.clone())),
            verify: std::sync::Arc::new(|_| Ok("ok".into())),
        };
        let mut bs = Bootstrap::with_fns(cfg.clone(), fns);
        bs.step = Step::Confirm;
        bs.answers = freehold_installer::Answers::defaults();
        bs.on_key(crossterm::event::KeyCode::Enter); // begin_stages
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
        let mut last = String::new();
        while std::time::Instant::now() < deadline {
            bs.tick();
            let state = format!(
                "step={:?} s0={:?} s1={:?} s2={:?} s3={:?} s4={:?} cfg={} trans={}",
                bs.step,
                bs.stages[0].status,
                bs.stages[1].status,
                bs.stages[2].status,
                bs.stages[3].status,
                bs.stages[4].status,
                cfg.exists(),
                bs.transitioning
            );
            if state != last {
                last = state;
            }
            if bs.transitioning && cfg.exists() {
                return;
            }
            std::thread::sleep(std::time::Duration::from_millis(10));
        }
        panic!("pipeline never converged: {last}");
    }

    /// Requires the LIVE world: the proxmox-box runner serving on 8787 with
    /// the SSH door installed. Run explicitly, not in CI.
    #[test]
    #[ignore]
    fn stages_advance_on_done() {
        let mut bs = Bootstrap::new(PathBuf::from("/tmp/fh-none.toml"));
        bs.step = Step::Confirm;
        bs.answers = freehold_installer::Answers::defaults();
        bs.answers.operator_pk =
            "1dc07610f40192b0cef0d008cab7f2e86000682ade2eca5bc52ea2d04b6da157".into();
        bs.on_key(crossterm::event::KeyCode::Enter); // begin_stages
        // drain until the first stage resolves (provision reuses or errors out)
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
        while bs.step == Step::Stages
            && bs.stages[0].status == Status::Running
            && std::time::Instant::now() < deadline
        {
            bs.tick();
            std::thread::sleep(std::time::Duration::from_millis(20));
        }
        assert_ne!(
            bs.stages[0].status,
            Status::Running,
            "provision stage never resolved"
        );
    }

    /// Live-world: drives the bring-up stages against the real runner/door
    /// and asserts the config lands in a temp path. Run explicitly.
    #[test]
    #[ignore]
    fn full_pipeline_writes_config() {
        let cfg = std::env::temp_dir().join("fh-pipe-test.toml");
        let _ = std::fs::remove_file(&cfg);
        let mut bs = Bootstrap::new(cfg.clone());
        bs.step = Step::Confirm;
        bs.answers = freehold_installer::Answers::defaults();
        bs.answers.operator_pk =
            "1dc07610f40192b0cef0d008cab7f2e86000682ade2eca5bc52ea2d04b6da157".into();
        bs.on_key(crossterm::event::KeyCode::Enter); // begin_stages
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(25);
        let mut last = String::new();
        while std::time::Instant::now() < deadline {
            bs.tick();
            let state = format!(
                "step={:?} s0={:?} s1={:?} s2={:?} s3={:?} s4={:?} cfg={} trans={} tail={:?}",
                bs.step,
                bs.stages[0].status,
                bs.stages[1].status,
                bs.stages[2].status,
                bs.stages[3].status,
                bs.stages[4].status,
                cfg.exists(),
                bs.transitioning,
                bs.stages[3].tail.get(0..120)
            );
            if state != last {
                eprintln!("PIPE: {state}");
                last = state;
            }
            if bs.transitioning && cfg.exists() {
                eprintln!("PIPE: config written OK");
                return;
            }
            std::thread::sleep(std::time::Duration::from_millis(50));
        }
        panic!("pipeline never wrote the config: {last}");
    }
}
