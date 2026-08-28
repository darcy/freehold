//! Phase 0.12 — the durable volume plane: purely-derived naming and
//! resolution DECISIONS, decoupled from the exec driver so the ordering and
//! consent semantics are hermetic-testable without a host.
//!
//! Everything here takes parsed state + a consent decision and returns a
//! decision; the exec driver (`drive.rs`) turns decisions into `pct`/`zfs`/
//! `lvm`/`curl` commands through the runner. Splitting them this way keeps
//! the acceptance-critical rules (naming convention, backend order, consent
//! gating, teardown scope semantics, idempotency-on-rerun) testable in pure
//! unit tests — the same hermetic-first discipline the repo applies to the
//! Vultr/B2/relay surfaces.

use anyhow::Result;

/// One reference MOUNT of a durable dataset into a guest, born at LXC create
/// (the locked "born on the plane, never `pct set` post-hoc" rule).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MountSpec {
    /// The dataset/volume to bind (e.g. `rpool/freehold/t-d/relay/docker-root`).
    pub source: String,
    /// The guest mount point (e.g. `/var/lib/docker`, `/srv/data/k8s-volumes`).
    pub guest_path: String,
}

/// Tenant kinds that get their own durable dataset under a common parent.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Tenant {
    Relay,
    Cp,
    K3sVolumes,
}

impl Tenant {
    pub fn as_str(self) -> &'static str {
        match self {
            Tenant::Relay => "relay",
            Tenant::Cp => "cp",
            Tenant::K3sVolumes => "k3s-volumes",
        }
    }

    /// The tenant's LXC role suffix (matches bootstrap's `<domain>-<role>`
    /// guest naming — `lxc_name`). k3s-volumes rides the k3s guest.
    pub fn lxc_role(self) -> &'static str {
        match self {
            Tenant::Relay => "relay",
            Tenant::Cp => "cp",
            Tenant::K3sVolumes => "k3s",
        }
    }

    pub const ALL: [Tenant; 3] = [Tenant::Relay, Tenant::Cp, Tenant::K3sVolumes];
}

impl std::fmt::Display for Tenant {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Relay is TWO child datasets under its tenant parent (locked decision):
/// the docker data root (named volumes live under /var/lib/docker/volumes)
/// and the compose deploy dir (where `.env` — the signing identity + every
/// DB/S3 credential — must sit OUTSIDE the wipeable docker root).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum RelayChild {
    DockerRoot,
    DeployDir,
}

impl RelayChild {
    pub fn as_str(self) -> &'static str {
        match self {
            RelayChild::DockerRoot => "docker-root",
            RelayChild::DeployDir => "deploy",
        }
    }
}

/// The Proxmox backend resolution ORDER (locked: `ZFS → LVM-thin → bail`,
/// identically on the per-branch list and in the decision). No silent fourth
/// tier: "existing viable backend" is a DETECT outcome, never a new rung.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Backend {
    Zfs,
    LvmThin,
}

/// A healthy-consuming backend the resolution DETECTED already present.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ExistingBackend {
    /// An existing zpool is present and viable.
    Zfs,
    /// An existing LVM VG with a thin pool is present and viable.
    LvmThin,
}

/// Which durable-storage DRIVER a tenant's plane uses. Recorded in the
/// config's `plane.backend_kind` so dispatch (ensure vs destroy) and teardown
/// pick the right verbs (zfs vs lvm-thin) instead of inferring from a name.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum BackendKind {
    Zfs,
    LvmThin,
}

/// What the resolution stage decides to DO, given the detected host state
/// and the operator's consent. A pure function of the inputs — the driver
/// maps each arm to the exact remote commands.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ResolveAction {
    /// Use an existing backend, create nothing. Idempotent rerun outcome.
    /// The `String` is the DETECTED backend identity (the zpool name, or the
    /// LVM VG name) — not a hardcoded default, so a stock PVE `pve` VG or any
    /// zpool name flows through to the ensure/destroy steps correctly.
    Reuse(ExistingBackend, String),
    /// Create a backend. Only reachable with consent == true. The `String`
    /// is the pool/VG name to create under.
    Create(Backend, String),
    /// No viable backend and no consent to create one: fail-closed with an
    /// actionable message. Overriding consent is NOT a tier — the ordering
    /// is unchanged; it only decides the Create vs Bail arm.
    Bail(String),
}

/// The VPS storage outcome (first-pass: block volume → explicitly-downgraded
/// local directory → bail).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum VpsStorage {
    /// The provider block volume IS the durable root.
    BlockVolume {
        /// Provider volume id/label for the mapping.
        volume_id: String,
        /// Mount point of the volume on the instance.
        mount: String,
    },
    /// Plain directory on the instance disk; durability EXPLICITLY downgraded.
    LocalDir(String),
}

/// The naming convention — `<pool>/freehold/<domain-with-dashes>/<tenant>`.
///
/// This is the two-place rule's second place: the tenant→dataset mapping is
/// DERIVABLE from the host/provider's volume listing ALONE (acceptance item),
/// because the dataset path carries both the world (domain) and the tenant.
pub fn dataset_path(pool: &str, domain: &str, tenant: Tenant) -> Result<String> {
    let dom = normalize_domain(domain)?;
    Ok(format!("{pool}/freehold/{dom}/{}", tenant.as_str()))
}

/// The VPS FLATTENED block-volume label: `fh-<domain-with-dashes>-<tenant>`.
/// Provider volume labels are flat with a restricted charset (no slashes) —
/// the same '.' → '-' normalization the guest names already use.
pub fn vps_volume_label(domain: &str, tenant: Tenant) -> Result<String> {
    let dom = normalize_domain(domain)?;
    Ok(format!("fh-{dom}-{}", tenant.as_str()))
}

/// Freehold's relay has TWO child datasets under its tenant parent: the
/// docker data root and the compose deploy dir (the `.env` must sit outside
/// the wipeable docker root, but still on the plane).
pub fn relay_child_dataset(pool: &str, domain: &str, child: RelayChild) -> Result<String> {
    Ok(format!(
        "{}/{}",
        dataset_path(pool, domain, Tenant::Relay)?,
        child.as_str()
    ))
}

/// The LVM thin-LV NAME for a tenant under a VG. Flat with a restricted
/// charset (LV names allow no `/`), and TWO-PLACE derivable: the name encodes
/// the world (domain) + tenant, so it is recoverable from `lvs` alone.
/// `freehold-<domain-with-dashes>-<tenant>` (e.g. `freehold-freehold-test-darcydev-net-relay`).
pub fn lvm_lv_name(domain: &str, tenant: Tenant) -> Result<String> {
    Ok(format!(
        "freehold-{}-{}",
        normalize_domain(domain)?,
        tenant.as_str()
    ))
}

/// The LVM thin-LV name for one of the relay's two child datasets. Relay
/// keeps the two-child blast-radius separation (docker data-root + compose
/// deploy dir) on LVM too — two thin LVs, each independently destroyable.
pub fn lvm_relay_child_lv_name(domain: &str, child: RelayChild) -> Result<String> {
    Ok(format!(
        "freehold-{}-relay-{}",
        normalize_domain(domain)?,
        child.as_str()
    ))
}

fn normalize_domain(domain: &str) -> Result<String> {
    let normalized = domain.replace('.', "-");
    if normalized.is_empty() || normalized.len() > 48 {
        anyhow::bail!("domain {domain:?} yields an invalid dataset name");
    }
    if !normalized
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || c == '-')
    {
        anyhow::bail!("domain {domain:?} contains characters not allowed in a dataset name");
    }
    Ok(normalized)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dataset_path_carries_world_and_tenant() {
        assert_eq!(
            dataset_path("rpool", "freehold-test.darcydev.net", Tenant::Relay).unwrap(),
            "rpool/freehold/freehold-test-darcydev-net/relay"
        );
        assert_eq!(
            dataset_path("rpool", "freehold-test.darcydev.net", Tenant::Cp).unwrap(),
            "rpool/freehold/freehold-test-darcydev-net/cp"
        );
        assert_eq!(
            dataset_path("rpool", "freehold-test.darcydev.net", Tenant::K3sVolumes).unwrap(),
            "rpool/freehold/freehold-test-darcydev-net/k3s-volumes"
        );
    }

    #[test]
    fn relay_child_sits_under_relay_tenant_parent() {
        assert_eq!(
            relay_child_dataset("rpool", "t.d", RelayChild::DockerRoot).unwrap(),
            "rpool/freehold/t-d/relay/docker-root"
        );
        assert_eq!(
            relay_child_dataset("rpool", "t.d", RelayChild::DeployDir).unwrap(),
            "rpool/freehold/t-d/relay/deploy"
        );
    }

    #[test]
    fn vps_label_is_flat_with_dashes() {
        assert_eq!(
            vps_volume_label("freehold-test.darcydev.net", Tenant::Relay).unwrap(),
            "fh-freehold-test-darcydev-net-relay"
        );
        assert!(
            !vps_volume_label("a.b", Tenant::Relay)
                .unwrap()
                .contains('.'),
            "flattened label has no dots"
        );
    }

    #[test]
    fn lvm_lv_name_is_flat_and_derivable() {
        assert_eq!(
            lvm_lv_name("freehold-test.darcydev.net", Tenant::Relay).unwrap(),
            "freehold-freehold-test-darcydev-net-relay"
        );
        assert_eq!(
            lvm_relay_child_lv_name("t.d", RelayChild::DockerRoot).unwrap(),
            "freehold-t-d-relay-docker-root"
        );
        assert_eq!(
            lvm_relay_child_lv_name("t.d", RelayChild::DeployDir).unwrap(),
            "freehold-t-d-relay-deploy"
        );
        assert!(!lvm_lv_name("a.b", Tenant::Cp).unwrap().contains('.'));
        assert!(!lvm_lv_name("a.b", Tenant::Cp).unwrap().contains('/'));
    }

    #[test]
    fn bad_domain_is_rejected() {
        assert!(dataset_path("rpool", "has space", Tenant::Cp).is_err());
        assert!(dataset_path("rpool", "", Tenant::Cp).is_err());
    }
}
