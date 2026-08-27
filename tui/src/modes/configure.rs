//! Configure mode — config present, world not converged: an idempotent
//! check-then-run pipeline over the relay/cp LXCs + deploys. Every stage
//! probes first and only runs what's missing; failed stages show their tail
//! and can be retried (r).

use super::StageRunner;
use freehold_installer::config::{Config, cp_live, relay_live};
use freehold_installer::{
    Answers, probe_lxc, stage_bootstrap, stage_deploy_cp, stage_deploy_relay,
};
use std::path::PathBuf;

#[derive(Clone, PartialEq)]
pub enum CStatus {
    Pending,
    Check,
    Run,
    Ok,
    Failed,
}

pub struct CStage {
    pub name: &'static str,
    pub status: CStatus,
    pub tail: String,
    /// when the stage's check/run started — live ticking while it runs.
    pub started_at: Option<std::time::Instant>,
}

impl CStage {
    pub fn elapsed(&self) -> Option<std::time::Duration> {
        self.started_at.map(|t| t.elapsed())
    }
    pub fn elapsed_secs(&self) -> f32 {
        self.elapsed().map(|d| d.as_secs_f32()).unwrap_or(0.0)
    }
}

pub struct ConfigureState {
    pub cfg_path: PathBuf,
    pub cfg: Config,
    pub answers: Answers,
    pub stages: Vec<CStage>,
    pub cur: usize,
    pub notice: String,
    pub transitioning: bool,
    /// set the moment a successful exec through the runner proves valid ssh
    /// auth — the global "install" timer starts here (first run: door
    /// verify; reruns: the first check probe).
    pub auth_detected: bool,
    runner: StageRunner,
    job: Option<usize>,
    /// the two boot stages run CONCURRENTLY (each its own lane/runner) —
    /// the docker installs in the guests parallelize instead of serializing.
    boot: [Option<std::sync::mpsc::Receiver<super::RawMsg>>; 2],
}

impl ConfigureState {
    pub fn new(cfg_path: PathBuf) -> Self {
        // The mode probe decided Configure; a missing/invalid config here
        // still renders defaults (worst case the user re-bootstraps).
        let cfg = match Config::load(&cfg_path) {
            Ok(Some(cfg)) => cfg,
            _ => Config::from_answers(&Answers::defaults()),
        };
        Self::with_cfg(cfg_path, cfg)
    }

    pub fn with_cfg(cfg_path: PathBuf, cfg: Config) -> Self {
        let answers = Answers::from_config(&cfg);
        let stages = vec![
            CStage {
                name: "relay LXC (boot if missing)",
                status: CStatus::Pending,
                tail: String::new(),
                started_at: None,
            },
            CStage {
                name: "control-plane LXC (boot if missing)",
                status: CStatus::Pending,
                tail: String::new(),
                started_at: None,
            },
            CStage {
                name: "deploy the Buzz relay",
                status: CStatus::Pending,
                tail: String::new(),
                started_at: None,
            },
            CStage {
                name: "deploy the control plane",
                status: CStatus::Pending,
                tail: String::new(),
                started_at: None,
            },
        ];
        Self {
            cfg_path,
            cfg,
            answers,
            stages,
            cur: 0,
            notice: String::new(),
            transitioning: false,
            auth_detected: false,
            runner: StageRunner::new(),
            job: None,
            boot: [None, None],
        }
    }

    /// Rebuild stage state from the (possibly rewritten) config.
    pub fn restart(&mut self) {
        if let Ok(Some(cfg)) = Config::load(&self.cfg_path) {
            self.cfg = cfg;
            self.answers = Answers::from_config(&self.cfg);
        }
        for s in &mut self.stages {
            s.status = CStatus::Pending;
            s.tail.clear();
        }
        self.cur = 0;
        self.transitioning = false;
        self.notice.clear();
    }

    pub fn on_key(&mut self, code: crossterm::event::KeyCode) {
        if code == crossterm::event::KeyCode::Char('r') {
            self.restart();
        }
    }

    // ------------------------------------------------------------ flow

    /// Spawn a boot stage on its own runner: check first (an exec through
    /// the runner proves auth), then the boot if the LXC is missing — all on
    /// a SEPARATE channel so the other boot proceeds concurrently.
    fn spawn_boot(&mut self, i: usize) {
        let (tx, rx) = std::sync::mpsc::channel();
        self.boot[i] = Some(rx);
        let a = self.answers.clone();
        let cfg = self.cfg.clone();
        self.stages[i].status = CStatus::Check;
        self.stages[i].started_at = Some(std::time::Instant::now());
        std::thread::spawn(move || {
            // plain branches: check (proves auth via the lane), then boot.
            let out = (|| -> anyhow::Result<String> {
                if i == 0 {
                    if probe_lxc(&a, cfg.lxc.relay.vmid)? {
                        return Ok("already present".into());
                    }
                    stage_bootstrap(&a, "relay", a.relay_vmid)?;
                    Ok("relay LXC ready".into())
                } else {
                    if probe_lxc(&a, cfg.lxc.cp.vmid)? {
                        return Ok("already present".into());
                    }
                    stage_bootstrap(&a, "cp", a.cp_vmid)?;
                    Ok("cp LXC ready".into())
                }
            })();
            let msg = match out {
                Ok(t) => super::RawMsg::Done { ok: true, out: t },
                Err(e) => super::RawMsg::Done {
                    ok: false,
                    out: format!("{e:#}"),
                },
            };
            let _ = tx.send(msg);
        });
    }

    fn spawn_check(&mut self, i: usize) {
        let a = self.answers.clone();
        let cfg = self.cfg.clone();
        self.stages[i].status = CStatus::Check;
        self.stages[i].started_at = Some(std::time::Instant::now());
        self.job = Some(i);
        self.runner.spawn(move || {
            // the SAME real probes the running mode uses — a reachable proxy
            // must not let the pipeline SKIP a deploy that never happened.
            let present = match i {
                0 => probe_lxc(&a, cfg.lxc.relay.vmid)?,
                1 => probe_lxc(&a, cfg.lxc.cp.vmid)?,
                2 => relay_live(&cfg),
                3 => cp_live(&cfg),
                _ => false,
            };
            Ok(if present { "1".into() } else { "0".into() })
        });
    }

    fn spawn_run(&mut self, i: usize) {
        let a = self.answers.clone();
        self.stages[i].status = CStatus::Run;
        self.stages[i].started_at = Some(std::time::Instant::now());
        self.job = Some(i);
        self.runner.spawn(move || match i {
            0 => {
                stage_bootstrap(&a, "relay", a.relay_vmid)?;
                Ok("relay LXC ready".to_string())
            }
            1 => {
                stage_bootstrap(&a, "cp", a.cp_vmid)?;
                Ok("cp LXC ready".to_string())
            }
            2 => {
                stage_deploy_relay(&a)?;
                Ok(format!("relay live at https://{}", a.domain))
            }
            3 => {
                stage_deploy_cp(&a)?;
                Ok(format!("control plane live at https://cp-{}", a.domain))
            }
            _ => unreachable!(),
        });
    }

    /// Returns true when everything converged (app transitions to running).
    pub fn tick(&mut self) -> bool {
        // drain the parallel boot lanes first (relay + cp, own channels).
        // `take()` moves the receiver out so the Done may clear the lane.
        for i in 0..2 {
            if let Some(rx) = self.boot[i].take() {
                // one result per boot — plain if-let
                if let Ok(super::RawMsg::Done { ok, out }) = rx.try_recv() {
                    {
                        let st = &mut self.stages[i];
                        st.status = if ok { CStatus::Ok } else { CStatus::Failed };
                        st.tail = if ok { out } else { format!("failed: {out}") };
                    }
                    if ok {
                        let role = if i == 0 { "relay" } else { "cp" };
                        let _ =
                            freehold_installer::write_back_lxc(&self.answers, &mut self.cfg, role);
                        let _ = self.cfg.save(&self.cfg_path);
                        self.auth_detected = true;
                    }
                } else {
                    // still running — put the lane back
                    self.boot[i] = Some(rx);
                }
            }
        }
        while let Ok(super::RawMsg::Done { ok, out }) = self.runner.rx.try_recv() {
            let Some(i) = self.job.take() else { continue };
            match self.stages[i].status {
                CStatus::Check => {
                    if ok && out == "1" {
                        self.auth_detected = true; // a successful probe exec = valid auth
                        self.stages[i].status = CStatus::Ok;
                        self.stages[i].tail = "already present".into();
                        // a REUSED LXC may predate the write-back — record
                        // its coords now so the deploy stages can run.
                        if i == 0 || i == 1 {
                            let role = if i == 0 { "relay" } else { "cp" };
                            if freehold_installer::write_back_lxc(
                                &self.answers,
                                &mut self.cfg,
                                role,
                            )
                            .is_ok()
                            {
                                let _ = self.cfg.save(&self.cfg_path);
                            }
                        }
                    } else if ok {
                        self.spawn_run(i);
                    } else {
                        self.stages[i].status = CStatus::Failed;
                        self.stages[i].tail = out;
                        self.notice = "check failed — r to retry".into();
                    }
                }
                CStatus::Run | CStatus::Failed | CStatus::Pending | CStatus::Ok => {
                    let tail = out.clone();
                    self.auth_detected = true; // a run-ok means the lane is live
                    if ok {
                        self.stages[i].status = CStatus::Ok;
                        self.stages[i].tail = tail.clone();
                        // record the ACTUAL post-boot coordinates (auto vmid +
                        // dhcp ip) in the config — the user asked for this the
                        // moment the LXC exists.
                        if i == 0 || i == 1 {
                            let role = if i == 0 { "relay" } else { "cp" };
                            match freehold_installer::write_back_lxc(
                                &self.answers,
                                &mut self.cfg,
                                role,
                            ) {
                                Ok(()) => {
                                    self.stages[i].tail = format!(
                                        "{tail} — recorded in config (vmid {:?}, ip {:?})",
                                        guest_vmid(&self.cfg, role),
                                        guest_ip(&self.cfg, role)
                                    );
                                    if let Err(e) = self.cfg.save(&self.cfg_path) {
                                        self.notice = format!("config save failed: {e}");
                                    }
                                }
                                Err(e) => self.notice = format!("write-back failed: {e}"),
                            }
                        }
                    } else {
                        self.stages[i].status = CStatus::Failed;
                        self.stages[i].tail = out;
                        self.notice = "stage failed — r to retry".into();
                    }
                }
            }
        }
        // advance — the two BOOT stages (0/1) run CONCURRENTLY once started.
        if self.cur < self.stages.len() {
            match self.stages[self.cur].status {
                CStatus::Pending => {
                    let start = self.cur == 0 || self.cur == 1;
                    if start {
                        // kick BOTH boots at once (parallel guest installs)
                        self.spawn_boot(0);
                        self.spawn_boot(1);
                    } else {
                        self.spawn_check(self.cur);
                    }
                }
                CStatus::Ok => {
                    self.cur += 1;
                    if self.cur >= self.stages.len() {
                        self.notice = "world converged — entering running mode".into();
                        self.transitioning = true;
                    }
                }
                _ => {}
            }
        }
        self.transitioning
    }
}

fn guest_vmid(cfg: &Config, role: &str) -> Option<u32> {
    if role == "relay" {
        cfg.lxc.relay.vmid
    } else {
        cfg.lxc.cp.vmid
    }
}

fn guest_ip(cfg: &Config, role: &str) -> Option<String> {
    if role == "relay" {
        cfg.lxc.relay.ip.clone()
    } else {
        cfg.lxc.cp.ip.clone()
    }
}
