//! Configure mode — config present, world not converged: an idempotent
//! check-then-run pipeline over the relay/cp LXCs + deploys. Every stage
//! probes first and only runs what's missing; failed stages show their tail
//! and can be retried (r).

use super::StageRunner;
use freehold_installer::config::{Config, cp_live, k3s_live, relay_live};
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
    /// frozen elapsed at completion (Ok/Failed) — the timer STOPS once done.
    pub finished_at: Option<std::time::Duration>,
}

impl CStage {
    pub fn elapsed(&self) -> Option<std::time::Duration> {
        self.started_at.map(|t| t.elapsed())
    }
    pub fn elapsed_secs(&self) -> f32 {
        self.finished_at
            .or_else(|| self.elapsed())
            .map(|d| d.as_secs_f32())
            .unwrap_or(0.0)
    }

    /// stop the stage's clock at the Ok/Failed transition.
    fn freeze(&mut self) {
        if self.finished_at.is_none() {
            self.finished_at = self.started_at.map(|t| t.elapsed());
        }
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
                name: "durable volume plane (resolve + ensure datasets)",
                status: CStatus::Pending,
                tail: String::new(),
                started_at: None,
                finished_at: None,
            },
            CStage {
                name: "relay LXC (boot if missing)",
                status: CStatus::Pending,
                tail: String::new(),
                started_at: None,
                finished_at: None,
            },
            CStage {
                name: "control-plane LXC (boot if missing)",
                status: CStatus::Pending,
                tail: String::new(),
                started_at: None,
                finished_at: None,
            },
            CStage {
                name: "k3s cluster LXC (boot if missing)",
                status: CStatus::Pending,
                tail: String::new(),
                started_at: None,
                finished_at: None,
            },
            CStage {
                name: "deploy the Buzz relay",
                status: CStatus::Pending,
                tail: String::new(),
                started_at: None,
                finished_at: None,
            },
            CStage {
                name: "deploy the control plane",
                status: CStatus::Pending,
                tail: String::new(),
                started_at: None,
                finished_at: None,
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
            s.started_at = None;
            s.finished_at = None;
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
                // storage: already present iff the plane is resolved in config
                // (idempotent re-converge confirms + creates nothing).
                0 => cfg.plane.backend.is_some(),
                1 => probe_lxc(&a, cfg.lxc.relay.vmid)?,
                2 => probe_lxc(&a, cfg.lxc.cp.vmid)?,
                // a STOPPED or broken k3s guest must NOT report "already
                // present" — the API itself has to answer (the same rule as
                // stages 4/5's live checks; there is no later stage to
                // catch a dead cluster).
                3 => match cfg.lxc.k3s.ip {
                    Some(_) => k3s_live(&cfg),
                    None => false,
                },
                4 => relay_live(&cfg),
                5 => cp_live(&cfg),
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
                freehold_installer::stage_storage(&a, true)?;
                Ok("durable volume plane ready".to_string())
            }
            1 => {
                stage_bootstrap(&a, "relay", a.relay_vmid)?;
                Ok("relay LXC ready".to_string())
            }
            2 => {
                stage_bootstrap(&a, "cp", a.cp_vmid)?;
                Ok("cp LXC ready".to_string())
            }
            3 => {
                freehold_installer::stage_k3s(&a)?;
                Ok("k3s cluster ready".to_string())
            }
            4 => {
                stage_deploy_relay(&a)?;
                Ok(format!("relay live at https://{}", a.domain))
            }
            5 => {
                stage_deploy_cp(&a)?;
                Ok(format!("control plane live at https://cp-{}", a.domain))
            }
            _ => unreachable!(),
        });
    }

    /// Returns true when everything converged (app transitions to running).
    pub fn tick(&mut self) -> bool {
        while let Ok(super::RawMsg::Done { ok, out }) = self.runner.rx.try_recv() {
            let Some(i) = self.job.take() else { continue };
            match self.stages[i].status {
                CStatus::Check => {
                    if ok && out == "1" {
                        self.auth_detected = true; // a successful probe exec = valid auth
                        self.stages[i].status = CStatus::Ok;
                        self.stages[i].tail = "already present".into();
                        self.stages[i].freeze();
                        // a REUSED LXC may predate the write-back — record
                        // its coords now so the deploy stages can run.
                        if i == 1 || i == 2 || i == 3 {
                            let role = if i == 1 {
                                "relay"
                            } else if i == 2 {
                                "cp"
                            } else {
                                "k3s"
                            };
                            if freehold_installer::write_back_lxc(
                                &self.answers,
                                &mut self.cfg,
                                role,
                            )
                            .is_ok()
                            {
                                let _ = self.cfg.save(&self.cfg_path);
                                // spawn_run clones self.answers — a stale
                                // pre-write-back copy would re-boot from
                                // scratch on a retry.
                                self.answers = Answers::from_config(&self.cfg);
                            }
                        }
                    } else if ok {
                        self.spawn_run(i);
                    } else {
                        self.stages[i].status = CStatus::Failed;
                        self.stages[i].tail = out;
                        self.stages[i].freeze();
                        self.notice = "check failed — r to retry".into();
                    }
                }
                CStatus::Run | CStatus::Failed | CStatus::Pending | CStatus::Ok => {
                    let tail = out.clone();
                    self.auth_detected = true; // a run-ok means the lane is live
                    if ok {
                        self.stages[i].status = CStatus::Ok;
                        self.stages[i].tail = tail.clone();
                        self.stages[i].freeze();
                        // record the ACTUAL post-boot coordinates (auto vmid +
                        // dhcp ip) in the config — the user asked for this the
                        // moment the LXC exists.
                        if i == 1 || i == 2 || i == 3 {
                            let role = if i == 1 {
                                "relay"
                            } else if i == 2 {
                                "cp"
                            } else {
                                "k3s"
                            };
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
                                    } else {
                                        self.answers = Answers::from_config(&self.cfg);
                                    }
                                }
                                Err(e) => self.notice = format!("write-back failed: {e}"),
                            }
                        }
                    } else {
                        self.stages[i].status = CStatus::Failed;
                        self.stages[i].tail = out;
                        self.stages[i].freeze();
                        self.notice = "stage failed — r to retry".into();
                    }
                }
            }
        }
        // advance — SEQUENTIAL: PVE's `pct` takes a global config lock, so
        // concurrent `pct create`s collide ("trying to acquire lock...") —
        // the boots stay serial (the parallel experiment was rolled back).
        if self.cur < self.stages.len() {
            match self.stages[self.cur].status {
                CStatus::Pending => self.spawn_check(self.cur),
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
    match role {
        "relay" => cfg.lxc.relay.vmid,
        "k3s" => cfg.lxc.k3s.vmid,
        _ => cfg.lxc.cp.vmid,
    }
}

fn guest_ip(cfg: &Config, role: &str) -> Option<String> {
    match role {
        "relay" => cfg.lxc.relay.ip.clone(),
        "k3s" => cfg.lxc.k3s.ip.clone(),
        _ => cfg.lxc.cp.ip.clone(),
    }
}
