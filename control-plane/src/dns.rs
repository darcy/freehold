//! CP-owned internal DNS resolver (C0 core service).
//!
//! The CP runs an authoritative-but-narrow resolver (dnsmasq) inside ITS OWN
//! LXC: `dns_records` (state.dns) holds explicit name → IP entries written at
//! the SAME trigger points as target registration (record_lxc, the litellm
//! apply). The resolver renders those into dnsmasq's addn-hosts and forwards
//! EVERYTHING else upstream (split horizon, explicit records only — absent =
//! no answer, never a stale/typo name silently resolving).
//!
//! Every LXC/kube gets the CP resolver's IP as its nameserver at
//! provisioning; k3s coredns points its `forward .` at it. Readiness is a
//! tcp/53 probe from the services view. Deliberately NO wildcard and no
//! Postgres dependency — the state table is the local mirror (the Postgres
//! `dns_records` table for the distributed form is a named future move).

use std::path::{Path, PathBuf};

use crate::state::{DnsRecord, StateStore, StateError};

#[derive(Debug, thiserror::Error)]
pub enum DnsError {
    #[error("invalid record name {name:?}: must match [a-z0-9-]+, 1-63 chars, no leading/trailing dash")]
    InvalidName { name: String },
    #[error("invalid record IP {ip:?}: expected an IPv4 literal")]
    InvalidIp { ip: String },
    #[error("state error: {0}")]
    State(#[from] StateError),
    #[error("resolver error: {0}")]
    Resolver(String),
}

/// Validates a bare hostname record name: lowercase letters, digits, dashes.
pub fn validate_name(name: &str) -> Result<(), DnsError> {
    if name.is_empty()
        || name.len() > 63
        || !name
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')
        || name.starts_with('-')
        || name.ends_with('-')
    {
        return Err(DnsError::InvalidName { name: name.into() });
    }
    Ok(())
}

/// Validates an IPv4 literal (records are A entries today).
pub fn validate_ip(ip: &str) -> Result<(), DnsError> {
    let ok = ip
        .split('.')
        .map(|o| o.parse::<u8>().is_ok())
        .collect::<Vec<_>>();
    if ok.len() != 4 || !ok.iter().all(|b| *b) {
        return Err(DnsError::InvalidIp { ip: ip.into() });
    }
    Ok(())
}

/// addn-hosts lines for the CURRENT records: `<ip> <name>` per line, one per
/// record — the shape `/etc/hosts` and dnsmasq's `addn-hosts=` accept. Never
/// renders a wildcard; records are the entire explicit surface.
pub fn render_addn_hosts(records: &std::collections::BTreeMap<String, DnsRecord>) -> String {
    let mut out = String::new();
    for (name, rec) in records {
        out.push_str(&format!("{} {}\n", rec.ip, name));
    }
    out
}

/// Adds/updates one record, persists state, and re-syncs the resolver.
/// Registration-owned: same trigger points that write LXC coords.
pub fn upsert(
    store: &StateStore,
    name: &str,
    ip: &str,
    source: &str,
) -> Result<DnsRecord, DnsError> {
    validate_name(name)?;
    validate_ip(ip)?;
    let now = crate::state::now_secs();
    let rec = DnsRecord {
        ip: ip.to_string(),
        source: source.to_string(),
        created_at: now,
    };
    store.insert_dns(name, rec.clone());
    if let Err(e) = store.save() {
        store.remove_dns(name);
        return Err(e.into());
    }
    Ok(rec)
}

/// Removes a record + re-syncs the resolver. Missing = Ok (idempotent).
pub fn remove(store: &StateStore, name: &str) -> Result<(), DnsError> {
    store.remove_dns(name);
    store.save()?;
    Ok(())
}

/// The dnsmasq addn-hosts file the resolver watches (resolver write path).
pub fn addn_hosts_path(state_dir: &Path) -> PathBuf {
    state_dir.join("dnsmasq.addn-hosts")
}



/// Writes the addn-hosts file (the resolver auto-reloads hosts changes via
/// hostsdir semantics; a SIGHUP forces the reload). Pure — takes a write
/// closure so the CLI/TUI can inject the target's local exec.
pub fn sync_resolver(
    state_dir: &Path,
    records: &std::collections::BTreeMap<String, DnsRecord>,
    write: &dyn Fn(&Path, &str) -> Result<(), String>,
    reload: &dyn Fn() -> Result<(), String>,
) -> Result<(), DnsError> {
    let path = addn_hosts_path(state_dir);
    write(&path, &render_addn_hosts(records)).map_err(DnsError::Resolver)?;
    ensure_resolver_readable(&path).map_err(|e| DnsError::Resolver(e.to_string()))?;
    reload().map_err(DnsError::Resolver)?;
    Ok(())

}

/// dnsmasq runs as its own uid and must be able to TRAVERSE every ancestor
/// of the addn-hosts file and READ the file itself. Deploy-created dirs can
/// arrive 0700 — dnsmasq then fails the addn-hosts load with "Permission
/// denied" and serves nothing while the resolver still answers the READY
/// probe (tcp/53), so the failure is silent. After writing, grant o+x on any
/// ancestor directory missing it (never strip bits) and ensure the file is
/// world-readable.
fn ensure_resolver_readable(path: &Path) -> std::io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    let mut dir = path.parent();
    while let Some(d) = dir {
        let md = std::fs::metadata(d)?;
        if md.is_dir() {
            let mode = md.permissions().mode();
            if mode & 0o011 == 0 {
                std::fs::set_permissions(d, std::fs::Permissions::from_mode(mode | 0o011))?;
            }
        }
        dir = d.parent();
    }
    if path.exists() {
        let md = std::fs::metadata(&path)?;
        let mode = md.permissions().mode();
        if mode & 0o444 != 0o444 {
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(mode | 0o444))?;
        }
    }
    Ok(())
}

#[cfg(test)]
mod dns_tests {
    use super::*;
    use crate::state::StateStore;

    fn test_store() -> (StateStore, tempfile::TempDir) {
        let tmp = tempfile::tempdir().unwrap();
        (StateStore::open(tmp.path()).unwrap(), tmp)
    }

    #[test]
    fn name_validation() {
        for ok in ["relay", "litellm", "cp-2", "a1"] {
            validate_name(ok).unwrap_or_else(|e| panic!("{ok}: {e}"));
        }
        for bad in ["", ".start", "end.", "UPPER", "has space", "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "a/b", "dash-"] {
            assert!(validate_name(bad).is_err(), "{bad:?} must be rejected");
        }
    }

    #[test]
    fn ip_validation() {
        validate_ip("192.168.30.8").unwrap();
        for bad in ["192.168.30", "192.168.30.256", "1.2.3", "", "host", "::1"] {
            assert!(validate_ip(bad).is_err(), "{bad:?} must be rejected");
        }
    }

    #[test]
    fn upsert_roundtrip_and_render() {
        let (store, _tmp) = test_store();
        let rec = upsert(&store, "relay", "192.168.30.8", "record_lxc relay").unwrap();
        assert_eq!(rec.ip, "192.168.30.8");
        assert_eq!(store.get_dns("relay").unwrap().source, "record_lxc relay");

        // upsert overwrites (rebuild re-registers the same name at a new ip)
        upsert(&store, "relay", "192.168.30.9", "record_lxc relay").unwrap();
        assert_eq!(store.get_dns("relay").unwrap().ip, "192.168.30.9");

        upsert(&store, "litellm", "192.168.30.7", "litellm-apply").unwrap();
        let snap = store.snapshot();
        assert_eq!(snap.dns.len(), 2);
        let rendered = render_addn_hosts(&snap.dns);
        assert!(rendered.contains("192.168.30.9 relay"));
        assert!(rendered.contains("192.168.30.7 litellm"));
        // Explicit records only: never a wildcard line.
        assert!(!rendered.contains("*"));
    }

    #[test]
    fn remove_is_idempotent() {
        let (store, _tmp) = test_store();
        remove(&store, "nope").unwrap();
        upsert(&store, "cp", "192.168.30.9", "record_lxc cp").unwrap();
        remove(&store, "cp").unwrap();
        assert!(store.get_dns("cp").is_none());
    }

    #[test]
    fn sync_writes_and_reloads() {
        let (store, tmp) = test_store();
        upsert(&store, "relay", "192.168.30.8", "t").unwrap();
        let wrote = std::cell::RefCell::new(String::new());
        let reloaded = std::cell::RefCell::new(false);
        sync_resolver(
            tmp.path(),
            &store.snapshot().dns,
            &|p: &Path, body: &str| {
                assert_eq!(p, &addn_hosts_path(tmp.path()));
                *wrote.borrow_mut() = body.to_string();
                Ok(())
            },
            &|| {
                *reloaded.borrow_mut() = true;
                Ok(())
            },
        )
        .unwrap();
        assert!(wrote.borrow().contains("192.168.30.8 relay"));
        assert!(*reloaded.borrow());
    }
    #[test]
    fn resolver_readable_opens_0700_deploy_dirs() {
        // The deploy lands the state dir 0700-root; dnsmasq (uid dnsmasq)
        // silently fails its addn-hosts load without o+x on the ancestor
        // chain and o+r on the file. ensure_resolver_readable must repair
        // exactly that, stripping nothing.
        use std::os::unix::fs::PermissionsExt;
        let tmp = tempfile::tempdir().unwrap();
        let dir = tmp.path().join("srv").join("data").join("cp");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::set_permissions(tmp.path(), std::fs::Permissions::from_mode(0o700)).unwrap();
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700)).unwrap();
        let file = dir.join("dnsmasq.addn-hosts");
        std::fs::write(&file, "192.168.30.8 relay\n").unwrap();
        std::fs::set_permissions(&file, std::fs::Permissions::from_mode(0o600)).unwrap();

        ensure_resolver_readable(&file).unwrap();

        // every ancestor from the file up to the tmp root gained o+x (the
        // file gained o+r); no bit was stripped: 0o700 | 0o011 == 0o711.
        let mut d = file.parent().unwrap();
        loop {
            let mode = std::fs::metadata(d).unwrap().permissions().mode();
            assert_eq!(mode & 0o011, 0o011, "{d:?} should be traversable");
            if d == tmp.path() {
                break;
            }
            d = d.parent().unwrap();
        }
        let fmode = std::fs::metadata(&file).unwrap().permissions().mode();
        assert_eq!(fmode & 0o444, 0o444);
        assert_eq!(
            std::fs::metadata(&dir).unwrap().permissions().mode() & 0o777,
            0o711
        );
    }
}
