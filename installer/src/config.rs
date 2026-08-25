//! The `~/.config/freehold/config.toml` connection profile — the desired
//! state of the world. Present + converged → running; present + not → the
//! configure pipeline; absent → bootstrap.

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::net::TcpStream;
use std::path::{Path, PathBuf};

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Config {
    /// The relay identity domain (the A4 gate's subject).
    pub domain: String,
    pub relay_url: String,
    /// The relay's signing pubkey (trust anchor for rosters; best-effort).
    pub relay_pubkey: Option<String>,
    pub cp_url: String,
    /// Operator Nostr pubkey (64-hex) — console admin + relay owner.
    pub operator_pubkey: String,
    /// Where the operator's key lives (minted at bootstrap or pointed at).
    pub operator_identity: Option<PathBuf>,
    /// Where the local CP state + runner packages live.
    pub state_dir: PathBuf,
    pub runner: RunnerRef,
    pub lxc: LxcSpec,
    /// The desired stages the configure pipeline converges to.
    pub desired: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct RunnerRef {
    /// MCP address of the runner that execs into the PVE host.
    pub addr: String,
    /// The runner's Nostr pubkey (resolved at bootstrap; 64-hex).
    pub pubkey: String,
    /// The runner's package/target name.
    pub target: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct LxcSpec {
    pub relay: LxcGuest,
    pub cp: LxcGuest,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct LxcGuest {
    pub vmid: u32,
    pub ip: String,
    pub gw: String,
    pub rootfs_gb: u32,
    pub memory_mb: u32,
}

impl Config {
    pub fn default_path() -> PathBuf {
        if let Some(xdg) = std::env::var_os("XDG_CONFIG_HOME") {
            PathBuf::from(xdg).join("freehold").join("config.toml")
        } else {
            PathBuf::from(std::env::var_os("HOME").unwrap_or_else(|| "/root".into()))
                .join(".config")
                .join("freehold")
                .join("config.toml")
        }
    }

    /// Load the config; None when the file doesn't exist.
    pub fn load(path: &Path) -> Result<Option<Config>> {
        if !path.exists() {
            return Ok(None);
        }
        let raw =
            std::fs::read_to_string(path).with_context(|| format!("reading {}", path.display()))?;
        let cfg = toml::from_str(&raw).with_context(|| format!("parsing {}", path.display()))?;
        Ok(Some(cfg))
    }

    pub fn save(&self, path: &Path) -> Result<()> {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        let raw = toml::to_string_pretty(self)?;
        std::fs::write(path, raw).with_context(|| format!("writing {}", path.display()))
    }

    /// The config a bootstrap that answered exactly the defaults describes.
    pub fn from_answers(a: &crate::Answers) -> Config {
        Config {
            domain: a.domain.clone(),
            relay_url: format!("https://{}", a.domain),
            relay_pubkey: None,
            cp_url: format!("https://cp-{}", a.domain),
            operator_pubkey: a.operator_pk.clone(),
            operator_identity: a.operator_generated.then(|| a.operator_dir.clone()),
            state_dir: crate::state_dir(),
            runner: RunnerRef {
                addr: a.serve.clone(),
                pubkey: String::new(), // filled once the runner is provisioned
                target: a.runner.clone(),
            },
            lxc: LxcSpec {
                relay: LxcGuest {
                    vmid: a.relay_vmid,
                    ip: a.relay_ip.clone(),
                    gw: a.relay_gw.clone(),
                    rootfs_gb: a.rootfs_gb,
                    memory_mb: a.memory_mb,
                },
                cp: LxcGuest {
                    vmid: a.cp_vmid,
                    ip: a.cp_ip.clone(),
                    gw: a.relay_gw.clone(),
                    rootfs_gb: a.rootfs_gb,
                    memory_mb: a.memory_mb,
                },
            },
            desired: vec!["relay".into(), "cp".into()],
        }
    }
}

/// Minimal host:port probing — no TLS handshake, just reachability. Good
/// enough for v1 mode detection; the CP API (phase 2) replaces it with real
/// authenticated health checks.
#[derive(Debug, Clone, PartialEq)]
pub enum Mode {
    /// No config: collect inputs, provision the door, write the config.
    Bootstrap,
    /// Config present but the world isn't converged: run the pipeline.
    Configure,
    /// Config present + everything reachable: "Good to go!".
    Running,
}

/// Mode probe — the config file is the authoritative presence signal.
pub fn probe_mode(path: &Path) -> Result<Mode> {
    let Some(cfg) = Config::load(path)? else {
        return Ok(Mode::Bootstrap);
    };
    if probes_ok(&cfg) {
        Ok(Mode::Running)
    } else {
        Ok(Mode::Configure)
    }
}

/// Running = relay + cp + the provisioning runner all reachable.
pub fn probes_ok(cfg: &Config) -> bool {
    url_reachable(&cfg.relay_url)
        && url_reachable(&cfg.cp_url)
        && crate::port_open(&cfg.runner.addr)
}

/// https://host[:port] / http://host[:port] → TCP connect.
pub fn url_reachable(url: &str) -> bool {
    let Some((host, port)) = split_url(url) else {
        return false;
    };
    TcpStream::connect((host.as_str(), port)).is_ok()
}

pub fn split_url(url: &str) -> Option<(String, u16)> {
    let rest = url
        .strip_prefix("https://")
        .or_else(|| url.strip_prefix("http://"))?;
    let (host, port) = match rest.split_once(':') {
        Some((h, p)) => (h.to_string(), p.trim_end_matches('/').parse().ok()?),
        None => (rest.trim_end_matches('/').to_string(), 0),
    };
    let port = if port == 0 {
        if url.starts_with("https://") { 443 } else { 80 }
    } else {
        port
    };
    Some((host, port))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn config_roundtrips() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.toml");
        let a = crate::Answers::defaults();
        let mut cfg = Config::from_answers(&a);
        cfg.relay_pubkey = Some("ab".repeat(32));
        cfg.runner.pubkey = "cd".repeat(32);
        cfg.save(&path).unwrap();
        let back = Config::load(&path).unwrap().unwrap();
        assert_eq!(cfg, back);
        assert_eq!(back.domain, "freehold-test.darcydev.net");
    }

    #[test]
    fn missing_config_is_bootstrap() {
        let dir = tempfile::tempdir().unwrap();
        assert_eq!(
            probe_mode(&dir.path().join("nope.toml")).unwrap(),
            Mode::Bootstrap
        );
    }

    #[test]
    fn split_url_forms() {
        assert_eq!(
            split_url("https://relay.example"),
            Some(("relay.example".into(), 443))
        );
        assert_eq!(split_url("http://x:3000/"), Some(("x".into(), 3000)));
        assert_eq!(split_url("cp-rs.example/path"), None);
    }
}
