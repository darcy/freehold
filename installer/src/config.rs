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
    pub runner: RunnerRef,
    pub lxc: LxcSpec,
    /// Phase 0.12 durable-volume-plane mapping — the tenant→dataset (or
    /// volume) resolution. Survives compute teardown by design (the
    /// two-place rule): teardown reads it BEFORE it deletes the config, and
    /// it is independently re-derivable from the host/provider's volume
    /// listing via the naming convention.
    #[serde(default)]
    pub plane: PlaneSpec,
    /// The pieces of the world WE operate (relay/cp today; k3s, litellm
    /// later). A relay we were INVITED to would appear in `relay_url` but
    /// not here — we don't manage it.
    pub managed: Vec<String>,
}

/// The durable volume plane (Phase 0.12). Presence = the plane was resolved;
/// absence = pre-plane legacy / VPS-downgraded world.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct PlaneSpec {
    /// The backend in use (ZFS zpool name / LVM VG name / VPS volume label).
    /// The common parent of the per-tenant datasets.
    pub backend: Option<String>,
    /// The tenant→dataset mapping, keyed by tenant (relay/cp/k3s-volumes).
    /// Derived by the naming convention; stored for the two-place rule.
    #[serde(default)]
    pub datasets: std::collections::BTreeMap<String, String>,
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
    /// k3s substrate coords — absent in pre-k3s configs (serde default).
    #[serde(default)]
    pub k3s: LxcGuest,
}

/// A managed LXC's CONNECT/status coordinates. Filled in by the configure
/// pipeline right after the boot (the vmid is auto-picked; the IP is what
/// DHCP assigned); unknown (None) before creation. Sizing (rootfs/memory)
/// is bootstrap-time only.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct LxcGuest {
    pub vmid: Option<u32>,
    pub ip: Option<String>,
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
            runner: RunnerRef {
                addr: a.serve.clone(),
                pubkey: crate::resolve_runner_pubkey(&a.runner),
                target: a.runner.clone(),
            },
            lxc: LxcSpec {
                k3s: LxcGuest {
                    vmid: a.k3s_vmid,
                    ip: a.k3s_ip.clone(),
                },
                relay: LxcGuest {
                    vmid: a.relay_vmid,
                    ip: a.relay_ip.clone(),
                },
                cp: LxcGuest {
                    vmid: a.cp_vmid,
                    ip: a.cp_ip.clone(),
                },
            },
            plane: PlaneSpec::default(),
            managed: vec!["relay".into(), "cp".into()],
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

/// Running = the world is VERIFIED, not just reachable:
///   - the relay answers its own `/_liveness` over HTTPS
///   - the CP guest reports its systemd unit `active` (through the runner)
///   - the provisioning runner's MCP port is open
///
/// A TCP-reachable proxy is NOT the relay; an empty host is NOT running.
pub fn probes_ok(cfg: &Config) -> bool {
    relay_live(cfg) && cp_live(cfg) && crate::port_open(&cfg.runner.addr)
}

/// The k3s cluster's API liveness: ANY HTTP answer from the kube-apiserver
/// (the health endpoints are auth-gated by default — a 401 means the API is
/// up and answering; only a connection failure means down).
pub fn k3s_live(cfg: &Config) -> bool {
    let Some(ip) = &cfg.lxc.k3s.ip else {
        return false;
    };
    // the recorded ip is a CIDR (192.168.30.212/24) — strip the prefix or
    // the URL parses as host:443 with "/24:6443/healthz" as the path and
    // Traefik/other 443 listeners answer for the probe.
    let host = ip.split('/').next().unwrap_or(ip);
    http_any(&format!("https://{host}:6443/healthz"))
}

/// The relay's own health endpoint (the buzz `/_liveness`).
pub fn relay_live(cfg: &Config) -> bool {
    http_ok(&format!(
        "{}/_liveness",
        cfg.relay_url.trim_end_matches('/')
    ))
}

/// The CP guest's systemd unit, asked THROUGH the provisioning runner (the
/// CP console binds loopback inside its LXC — there is no public route).
/// The CP console's `/healthz`, pinged THROUGH the provisioning runner (the
/// console binds loopback inside its LXC — there is no public route): the
/// endpoint must answer 200.
pub fn cp_live(cfg: &Config) -> bool {
    let Some(vmid) = cfg.lxc.cp.vmid else {
        return false;
    };
    let a = crate::Answers::from_config(cfg);
    let cmd = format!(
        r#"pct exec {vmid} -- bash -c 'exec 3<>/dev/tcp/127.0.0.1/8080; printf "GET /healthz HTTP/1.0\r\n\r\n" >&3; grep -m1 "^HTTP" <&3 || true'"#
    );
    match crate::run(
        &crate::bin("freehold-orchestrator"),
        &[
            "exec",
            "--addr",
            &a.serve,
            "--agent-dir",
            crate::ops_dir().to_str().unwrap(),
            &a.runner,
            &cmd,
        ],
    ) {
        Ok((true, out)) => out.contains(" 200 ") || out.contains("200 OK"),
        _ => false,
    }
}

/// HTTPS GET accepting the local-CA / operator-proxy TLS posture (the check
/// is liveness, not CA pinning), 6s timeout, 2xx-3xx = alive.
/// Any HTTP response (including the k3s API's auth-gated 401) proves the
/// endpoint answers; only a transport failure is "down".
fn http_any(url: &str) -> bool {
    use std::time::Duration;
    let Ok(client) = reqwest::blocking::Client::builder()
        .danger_accept_invalid_certs(true)
        .timeout(Duration::from_secs(6))
        .build()
    else {
        return false;
    };
    client.get(url).send().is_ok()
}

fn http_ok(url: &str) -> bool {
    use std::time::Duration;
    let Ok(client) = reqwest::blocking::Client::builder()
        .danger_accept_invalid_certs(true)
        .timeout(Duration::from_secs(6))
        .build()
    else {
        return false;
    };
    match client.get(url).send() {
        Ok(resp) => {
            let s = resp.status().as_u16();
            (200..400).contains(&s)
        }
        Err(_) => false,
    }
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
