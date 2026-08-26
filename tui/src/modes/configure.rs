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
}

pub struct ConfigureState {
    pub cfg_path: PathBuf,
    pub cfg: Config,
    pub answers: Answers,
    pub stages: Vec<CStage>,
    pub cur: usize,
    pub notice: String,
    pub transitioning: bool,
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
                name: "relay LXC (boot if missing)",
                status: CStatus::Pending,
                tail: String::new(),
            },
            CStage {
                name: "control-plane LXC (boot if missing)",
                status: CStatus::Pending,
                tail: String::new(),
            },
            CStage {
                name: "deploy the Buzz relay",
                status: CStatus::Pending,
                tail: String::new(),
            },
            CStage {
                name: "deploy the control plane",
                status: CStatus::Pending,
                tail: String::new(),
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
        while let Ok(super::RawMsg::Done { ok, out }) = self.runner.rx.try_recv() {
            let Some(i) = self.job.take() else { continue };
            match self.stages[i].status {
                CStatus::Check => {
                    if ok && out == "1" {
                        self.stages[i].status = CStatus::Ok;
                        self.stages[i].tail = "already present".into();
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
        // advance
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
