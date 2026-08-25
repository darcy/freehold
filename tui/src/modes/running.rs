//! Running mode — everything reachable: the "Good to go!" dashboard. Phase 1
//! shows liveness of the relay, the CP, and the provisioning runner; the CP
//! API (phase 2) feeds the full console-parity views.

use freehold_installer::config::{Config, url_reachable};
use freehold_installer::port_open;
use std::path::PathBuf;
use std::time::{Duration, Instant};

pub struct Running {
    pub cfg: Option<Config>,
    /// (label, reachable)
    pub probes: Vec<(String, bool)>,
    pub last: Instant,
    pub request_configure: bool,
}

impl Running {
    pub fn new(cfg_path: PathBuf) -> Self {
        let cfg = Config::load(&cfg_path).ok().flatten();
        let mut r = Self {
            cfg: cfg.clone(),
            probes: Vec::new(),
            last: Instant::now() - Duration::from_secs(5),
            request_configure: false,
        };
        if let Some(c) = &cfg {
            r.probe(c);
        }
        r
    }

    pub fn with_cfg(cfg_path: PathBuf, cfg: Config) -> Self {
        let mut r = Self::new(cfg_path);
        r.cfg = Some(cfg.clone());
        r.probe(&cfg);
        r
    }

    fn probe(&mut self, cfg: &Config) {
        self.probes = vec![
            ("relay".into(), url_reachable(&cfg.relay_url)),
            ("control plane".into(), url_reachable(&cfg.cp_url)),
            ("provisioning runner".into(), port_open(&cfg.runner.addr)),
        ];
        self.last = Instant::now();
    }

    pub fn on_key(&mut self, code: crossterm::event::KeyCode) {
        if code == crossterm::event::KeyCode::Char('c') {
            self.request_configure = true;
        }
    }

    pub fn tick(&mut self) {
        if self.last.elapsed() >= Duration::from_secs(2)
            && let Some(cfg) = self.cfg.clone()
        {
            self.probe(&cfg);
        }
    }

    pub fn all_ok(&self) -> bool {
        !self.probes.is_empty() && self.probes.iter().all(|(_, ok)| *ok)
    }
}
