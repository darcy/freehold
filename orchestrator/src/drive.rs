//! Phase 0.12 — the exec DRIVER: turns the pure `planebase` decisions into
//! `pct`/`zfs`/`lvm`/`mount` commands through the runner's ONE `exec`
//! primitive. Detection is always read-only; CREATING a backend or dataset
//! is gated by the operator's consent (`--confirm-storage`), and a withheld
//! consent never invents a fallback tier — it surfaces the `Bail` action.
//!
//! The driver owns the exact remote command verbs; the acceptance-critical
//! semantics (order, naming, consent, idempotency) live in `planebase` so
//! they stay pure-testable.

use anyhow::Result;

use crate::bootstrap::BootstrapError;
use crate::client::McpClient;
use crate::planebase::{
    Backend, BackendKind, ExistingBackend, GUEST_PATH_CP, GUEST_PATH_DOCKER_ROOT,
    GUEST_PATH_K8S_VOLUMES, GUEST_PATH_RELAY_DEPLOY, MountSpec, RelayChild, ResolveAction, Tenant,
    dataset_path, lvm_lv_name, lvm_relay_child_lv_name, relay_child_dataset,
};

// ---------------------------------------------------------------- detection

/// List existing zpools (`-H -o name`). Read-only; empty => none present.
pub fn zpool_list(client: &McpClient, target: &str) -> Result<Vec<String>, BootstrapError> {
    let out = crate::bootstrap::exec(client, target, "zpool list -H -o name", 60)?;
    if out.exit_code != Some(0) {
        // zpool may be absent entirely (ENOENT) — treat as "no zpools".
        return Ok(vec![]);
    }
    Ok(out
        .stdout
        .lines()
        .map(str::trim)
        .filter(|l| !l.is_empty())
        .map(str::to_string)
        .collect())
}

/// Does the named zpool already exist? (idempotency: never re-create.)
pub fn zpool_exists(client: &McpClient, target: &str, pool: &str) -> Result<bool, BootstrapError> {
    Ok(zpool_list(client, target)?.iter().any(|p| p == pool))
}

/// List existing LVM volume groups (`vgs -o vg_name --noheadings`). Empty =>
/// no VG (no LVM backend present).
pub fn vg_list(client: &McpClient, target: &str) -> Result<Vec<String>, BootstrapError> {
    let out = crate::bootstrap::exec(
        client,
        target,
        "vgs --noheadings -o vg_name 2>/dev/null || true",
        60,
    )?;
    if out.exit_code != Some(0) {
        return Ok(vec![]);
    }
    Ok(out.stdout.split_whitespace().map(str::to_string).collect())
}

/// The thin pool REUSED for freehold's tenant LVs, if this VG already has
/// one (`lvs -a`, looking for the `pool_tmeta`/`pool_tdata` companion pair
/// that marks a thin pool). A stock PVE install has `pve/data` (local-lvm) —
/// it must be REUSED, never shadowed by a fresh carve-out on a near-full VG.
pub fn thin_pool_name(
    client: &McpClient,
    target: &str,
    vg: &str,
) -> Result<Option<String>, BootstrapError> {
    let names = lvs_names(client, target, vg)?;
    Ok(names
        .iter()
        .filter(|n| {
            n.ends_with("_tmeta")
                && names
                    .iter()
                    .any(|m| m == &format!("{}_tdata", n.trim_end_matches("_tmeta")))
        })
        .map(|n| n.trim_end_matches("_tmeta").to_string())
        .next())
}

/// Does a thin volume (LV) already exist for this tenant under this VG?
pub fn thin_lv_exists(
    client: &McpClient,
    target: &str,
    vg: &str,
    tenant: &str,
) -> Result<bool, BootstrapError> {
    Ok(lvs_names(client, target, vg)?.iter().any(|n| n == tenant))
}

fn lvs_names(client: &McpClient, target: &str, vg: &str) -> Result<Vec<String>, BootstrapError> {
    // `-a` so the INTERNAL thin-pool segments are visible — a plain `lvs`
    // hides them, which made a stock PVE host's existing `pve/data` thin pool
    // undetectable (and a doomed fresh-pool lvcreate followed). The hidden
    // segments print bracketed (`[data_tdata]`); strip the brackets.
    let out = crate::bootstrap::exec(
        client,
        target,
        &format!("lvs -a --noheadings -o lv_name {vg} 2>/dev/null || true"),
        60,
    )?;
    Ok(out
        .stdout
        .split_whitespace()
        .map(|n| n.trim_matches(['[', ']']))
        .map(str::to_string)
        .collect())
}

// ------------------------------------------------------------- resolution

/// Resolve the Proxmox backend: detect existing → consent-gated create →
/// bail. Pure-order preserved (ZFS → LVM-thin → bail), consent only decides
/// Create vs Bail, never reorders.
pub async fn resolve_proxmox(
    client: &McpClient,
    target: &str,
    consent: bool,
    device: Option<&str>,
) -> Result<ResolveAction, BootstrapError> {
    // 1. existing zpool? reuse quietly, carrying its name so the ensure/
    //    destroy steps drive the REAL backend (never a hardcoded 'rpool').
    let pools = zpool_list(client, target)?;
    if let Some(pool) = pools.first() {
        return Ok(ResolveAction::Reuse(ExistingBackend::Zfs, pool.clone()));
    }
    // 2. existing LVM VG (with/without thin pool)? reuse quietly.
    let vgs = vg_list(client, target)?;
    if let Some(vg) = vgs.first() {
        return Ok(ResolveAction::Reuse(ExistingBackend::LvmThin, vg.clone()));
    }
    // 3. none + no consent → bail (actionable), never a silent tier.
    if !consent {
        return Ok(ResolveAction::Bail(
            "no existing ZFS zpool or LVM VG detected, and --confirm-storage is not set — \
             re-run with --confirm-storage to create one (or attach a disk / use a NAS)"
                .to_string(),
        ));
    }
    // 4. consent given: prefer ZFS, fall back to LVM-thin (the locked order).
    //    A zpool needs a PHYSICAL device to carve — if none is supplied there
    //    is no ZFS to create, so we fall to LVM-thin (a thin LV + filesystem
    //    on existing storage needs no new device). LVM-thin creates into a
    //    NEW VG named freehold on ensure when none exists.
    // A device present means a zpool can be carved -> prefer ZFS; absent,
    // fall to LVM-thin (a thin LV + filesystem needs no new device).
    if device.is_some() {
        Ok(ResolveAction::Create(Backend::Zfs, "rpool".into()))
    } else {
        Ok(ResolveAction::Create(Backend::LvmThin, "freehold".into()))
    }
}

// --------------------------------------------------------------- creation

/// Create a zpool if absent (idempotent). The physical device is
/// operator-supplied (`--device`); the pool name is PVE-style `rpool`.
pub async fn ensure_zpool(
    client: &McpClient,
    target: &str,
    pool: &str,
    device: Option<&str>,
) -> Result<(), BootstrapError> {
    if zpool_exists(client, target, pool)? {
        return Ok(());
    }
    let Some(dev) = device else {
        return Err(BootstrapError::Verify(
            "creating a zpool needs a physical device — pass --device (e.g. /dev/sdb); \
             or use an existing zpool/LVM backend"
                .into(),
        ));
    };
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &format!("zpool create {pool} {dev}"),
        "zpool create",
        300,
    )?;
    Ok(())
}

/// Create a dataset if absent (idempotent). `--parents` builds the
/// `freehold/<domain>` chain.
pub async fn ensure_dataset(
    client: &McpClient,
    target: &str,
    dataset: &str,
) -> Result<(), BootstrapError> {
    let exists = crate::bootstrap::exec(
        client,
        target,
        &format!("zfs list -H -o name {dataset} >/dev/null 2>&1"),
        60,
    )?
    .exit_code
        == Some(0);
    if exists {
        return Ok(());
    }
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &format!("zfs create -p {dataset}"),
        "zfs create dataset",
        120,
    )?;
    Ok(())
}

/// chown a dataset's mountpoint to the unprivileged-LXC shifted uid range so
/// the guest can write it (locked: "verify guest-writable, never assumed").
/// Default shifted uid root = 100000 (map[0 100000] for a 65536-uid range).
pub async fn chown_guest_uid(
    client: &McpClient,
    target: &str,
    mountpoint: &str,
    shifted_uid: u32,
) -> Result<(), BootstrapError> {
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &format!("chown -R {shifted_uid}:{shifted_uid} {mountpoint}"),
        "chown dataset to guest uid",
        120,
    )?;
    Ok(())
}

/// Ensure ONE LVM thin LV exists + is mounted at a HOST path (idempotent):
/// the VG's EXISTING thin pool is reused (stock PVE: `pve/data`); only a VG
/// with no thin pool gets a fresh `freehold-thin` carve-out. Then the LV,
/// ext4, and a mount at `host_path`. Returns the host mount path (the `mpN`
/// source).
///
/// The fresh-pool SIZE is a first-pass fixed value; the LV is named by the
/// caller (the two-place-derivable `freehold-<domain>-<tenant>` names from
/// planebase).
pub async fn ensure_lvm_lv(
    client: &McpClient,
    target: &str,
    vg: &str,
    lv_name: &str,
    host_path: &str,
) -> Result<String, BootstrapError> {
    let dev = format!("/dev/{vg}/{lv_name}");
    if !thin_lv_exists(client, target, vg, lv_name)? {
        // REUSE the VG's existing thin pool (stock PVE: `pve/data`); only
        // carve a fresh `freehold-thin` when the VG truly has none.
        let pool = match thin_pool_name(client, target, vg)? {
            Some(existing) => existing,
            None => {
                crate::bootstrap::exec_to_ok(
                    client,
                    target,
                    &format!("lvcreate -L 40G -T {vg}/freehold-thin"),
                    "lvcreate thin pool",
                    300,
                )?;
                "freehold-thin".to_string()
            }
        };
        crate::bootstrap::exec_to_ok(
            client,
            target,
            &format!("lvcreate -V 20G -T {vg}/{pool} -n {lv_name}"),
            "lvcreate thin LV",
            120,
        )?;
    }
    // mkfs GATED on blkid: runs on first create, and recovers a partial
    // failure (a prior run that died between lvcreate and mkfs leaves the LV
    // without a filesystem — re-running must mkfs it, not skip).
    let has_fs = crate::bootstrap::exec(
        client,
        target,
        &format!("blkid -s TYPE -o value {dev} 2>/dev/null"),
        30,
    )?
    .exit_code
        == Some(0);
    if !has_fs {
        crate::bootstrap::exec_to_ok(
            client,
            target,
            &format!("mkfs.ext4 -q {dev}"),
            "mkfs LV",
            120,
        )?;
    }
    // mkdir + mount at the host path (idempotent).
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &format!("mkdir -p {host_path}"),
        "mkdir mount",
        60,
    )?;
    let mounted = crate::bootstrap::exec(
        client,
        target,
        &format!("mountpoint -q {host_path} 2>/dev/null"),
        30,
    )?
    .exit_code
        == Some(0);
    if !mounted {
        crate::bootstrap::exec_to_ok(
            client,
            target,
            &format!("mount {dev} {host_path}"),
            "mount LV",
            60,
        )?;
    }
    // Record the mount in /etc/fstab (idempotent) so a HOST REBOOT restores
    // the plane — unlike ZFS (remounted by zfs-mount.service), a bare
    // `mount` of an ext4 LV does not survive reboot; without this the guest
    // would bind-mount an empty dir and write into the host root fs.
    let fstab_line = format!("{dev} {host_path} ext4 defaults 0 2");
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &format!(
            "grep -qxF '{fstab_line}' /etc/fstab || echo '{fstab_line}' | tee -a /etc/fstab >/dev/null"
        ),
        "record mount in /etc/fstab",
        60,
    )?;
    Ok(host_path.to_string())
}

/// Resolve a tenant's born-at-create MOUNTS on an **LVM-thin** backend: one
/// thin LV per tenant (relay keeps TWO — the docker data-root + compose
/// deploy dir — preserving the two-child blast radius on LVM too), each
/// created + mounted and returned as a `<host-source>:<guest-path>` spec.
pub async fn resolve_lvm_mounts(
    client: &McpClient,
    target: &str,
    vg: &str,
    domain: &str,
    tenant: Tenant,
) -> Result<Vec<MountSpec>, BootstrapError> {
    // Host parent dir under which each tenant LV is mounted. Two-place: the
    // VG + LV names alone re-identify the world+tenant from `lvs`.
    let base = format!("/freehold/{}", domain.replace('.', "-"));
    match tenant {
        Tenant::Relay => {
            let mut out = Vec::new();
            for (child, guest) in [
                (RelayChild::DockerRoot, GUEST_PATH_DOCKER_ROOT),
                (RelayChild::DeployDir, GUEST_PATH_RELAY_DEPLOY),
            ] {
                let lv = lvm_relay_child_lv_name(domain, child)
                    .map_err(|e| BootstrapError::Verify(e.to_string()))?;
                let host = format!("{base}/{}", child.as_str());
                let source = ensure_lvm_lv(client, target, vg, &lv, &host).await?;
                // Fresh ext4 is root-owned: chown to the guest's shifted uid,
                // same as the ZFS path (locked: guest-writable, never assumed).
                chown_guest_uid(client, target, &source, 100000).await?;
                out.push(MountSpec {
                    source,
                    guest_path: guest.into(),
                });
            }
            Ok(out)
        }
        Tenant::Cp => {
            let lv = lvm_lv_name(domain, Tenant::Cp)
                .map_err(|e| BootstrapError::Verify(e.to_string()))?;
            let host = format!("{base}/cp");
            let source = ensure_lvm_lv(client, target, vg, &lv, &host).await?;
            chown_guest_uid(client, target, &source, 100000).await?;
            Ok(vec![MountSpec {
                source,
                guest_path: GUEST_PATH_CP.into(),
            }])
        }
        Tenant::K3sVolumes => {
            let lv = lvm_lv_name(domain, Tenant::K3sVolumes)
                .map_err(|e| BootstrapError::Verify(e.to_string()))?;
            let host = format!("{base}/k3s-volumes");
            let source = ensure_lvm_lv(client, target, vg, &lv, &host).await?;
            chown_guest_uid(client, target, &source, 100000).await?;
            Ok(vec![MountSpec {
                source,
                guest_path: GUEST_PATH_K8S_VOLUMES.into(),
            }])
        }
    }
}

/// Does an LV exist under this VG? (idempotency probe).
pub fn lv_exists(
    client: &McpClient,
    target: &str,
    vg: &str,
    lv_name: &str,
) -> Result<bool, BootstrapError> {
    Ok(lvs_names(client, target, vg)?.iter().any(|n| n == lv_name))
}

// ----------------------------------------------------------- mountpoints

/// Build the LXC `--mpN` args: each MountSpec becomes
/// `<source>,mp=<guest>,backup=<n>`. The `backup` flag is the ARCHITECTURE
/// split made load-bearing: vzdump EXCLUDES mount points by default, so
/// `/srv/data` mounts need `backup=1` to be IN the job and reproducible
/// mounts `backup=0` to stay out — `planebase::backup_flag` is the rule.
pub fn lxc_mp_args(specs: &[MountSpec]) -> Vec<String> {
    specs
        .iter()
        .enumerate()
        .map(|(i, m)| {
            format!(
                "--mp{i}={},mp={},backup={}",
                m.source,
                m.guest_path,
                crate::planebase::backup_flag(&m.guest_path)
            )
        })
        .collect()
}

/// Destroy one tenant's dataset subtree (data+compute teardown). For relay
/// this is the PARENT (both children), per the locked single-parent-destroy
/// unit. `zfs destroy -r` removes children recursively.
///
/// Returns `Ok(false)` when the dataset is ABSENT (nothing to destroy — a
/// no-op to the caller), `Ok(true)` when it was destroyed. A real `zfs
/// destroy` failure is an `Err` — the caller must NOT treat a no-op and a
/// failed destroy as the same outcome.
pub async fn destroy_tenant_dataset(
    client: &McpClient,
    target: &str,
    pool: &str,
    domain: &str,
    tenant: Tenant,
) -> Result<bool, BootstrapError> {
    let ds =
        dataset_path(pool, domain, tenant).map_err(|e| BootstrapError::Verify(e.to_string()))?;
    // Existence probe FIRST — an absent dataset is a no-op, not an error
    // (the same probe ensure_dataset uses). This is what lets teardown
    // tolerate a missing dataset (pre-plane config, VPS branch, unprovisioned
    // k3s) while still surfacing a genuine destroy failure.
    let exists = crate::bootstrap::exec(
        client,
        target,
        &format!("zfs list -H -o name {ds} >/dev/null 2>&1"),
        60,
    )?
    .exit_code
        == Some(0);
    if !exists {
        return Ok(false);
    }
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &format!("zfs destroy -r {ds}"),
        "destroy tenant dataset",
        120,
    )
    .map_err(|e| {
        BootstrapError::Verify(format!(
            "dataset {ds} EXISTS but could not be destroyed: {e} — \
             the tenant's data is INTACT; fix the cause (busy/ref'ed) or re-run. \
             The teardown must not delete the config mapping for data that survived."
        ))
    })
    .map(|_| true)
}

/// Destroy a tenant's LVM-thin volumes for a data+compute teardown. Relay
/// destroys BOTH child LVs (docker-root + deploy), the parent-destroy unit.
/// Returns `Ok(false)` when every target LV is ABSENT (a no-op), `Ok(true)`
/// when at least one was removed; a real `lvremove` failure is an `Err`.
pub async fn destroy_lvm_tenant(
    client: &McpClient,
    target: &str,
    vg: &str,
    domain: &str,
    tenant: Tenant,
) -> Result<bool, BootstrapError> {
    // The LVs to remove (two for relay, one otherwise). Absent ones are
    // skipped; if NONE exist this is a no-op (Ok(false)).
    let lvs: Vec<String> = match tenant {
        Tenant::Relay => [RelayChild::DockerRoot, RelayChild::DeployDir]
            .iter()
            .map(|c| {
                lvm_relay_child_lv_name(domain, *c)
                    .map_err(|e| BootstrapError::Verify(e.to_string()))
            })
            .collect::<Result<_, _>>()?,
        other => {
            vec![lvm_lv_name(domain, other).map_err(|e| BootstrapError::Verify(e.to_string()))?]
        }
    };
    let mut destroyed_any = false;
    for lv in &lvs {
        if !lv_exists(client, target, vg, lv)? {
            continue;
        }
        let dev = format!("/dev/{vg}/{lv}");
        // Unmount + strip the fstab line FIRST — `lvremove -f` skips the
        // prompt, NOT the open-count check, so an LV still mounted at
        // /freehold/<domain>/... would be refused and the teardown would
        // bail with the LXCs already gone. umount failing is tolerated here
        // (the LV may never have been mounted); if it is genuinely busy the
        // lvremove below fails and surfaces it honestly.
        let _ = crate::bootstrap::exec(
            client,
            target,
            &format!("umount {dev} 2>/dev/null; sed -i '\\|^{dev} |d' /etc/fstab; true"),
            60,
        );
        crate::bootstrap::exec_to_ok(
            client,
            target,
            &format!("lvremove -f {vg}/{lv}"),
            "lvremove tenant thin LV",
            120,
        )
        .map_err(|e| {
            BootstrapError::Verify(format!(
                "LV {vg}/{lv} EXISTS but could not be removed: {e} — \
                 the tenant's data is INTACT; fix the cause or re-run"
            ))
        })?;
        destroyed_any = true;
    }
    Ok(destroyed_any)
}

/// Destroy a tenant for a data+compute teardown, dispatching on the backend
/// kind (ZFS `zfs destroy -r` vs LVM `lvremove`). `backend` is the pool/VG
/// name. Returns `Ok(false)` when absent (a no-op), `Ok(true)` when destroyed.
pub async fn destroy_tenant_backend(
    client: &McpClient,
    target: &str,
    backend_kind: BackendKind,
    backend: &str,
    domain: &str,
    tenant: Tenant,
) -> Result<bool, BootstrapError> {
    match backend_kind {
        BackendKind::Zfs => destroy_tenant_dataset(client, target, backend, domain, tenant).await,
        BackendKind::LvmThin => destroy_lvm_tenant(client, target, backend, domain, tenant).await,
    }
}

/// A host-root mountable path for a dataset: the `zfs get mountpoint` value.
/// PVE's `mpN` rejects a bare dataset name — it needs an absolute host path.
pub fn mountpoint_of(
    client: &McpClient,
    target: &str,
    dataset: &str,
) -> Result<String, BootstrapError> {
    let out = crate::bootstrap::exec(
        client,
        target,
        &format!("zfs get -H -o value mountpoint {dataset}"),
        60,
    )?;
    let mp = out.stdout.trim().to_string();
    if mp.is_empty() || mp == "none" || mp == "legacy" || !mp.starts_with('/') {
        return Err(BootstrapError::Verify(format!(
            "dataset {dataset} has no usable host mountpoint ({mp:?}) — \
             ensure the dataset is created and mounted"
        )));
    }
    Ok(mp)
}

/// Resolve a tenant's born-at-create MOUNTS to HOST-root mountable
/// `<host-source>:<guest-path>` specs: ensure the dataset(s) exist
/// (idempotent), chown them to the guest's shifted uid, and resolve each to
/// its real host mountpoint (PVE's `mpN` rejects a bare dataset name).
///
/// Returns the guest's expected mount specs keyed to the LXC ROLE that rides
/// them (relay/cp/k3s) — the same keys the config's `plane.mounts` map uses,
/// so `stage_bootstrap` can emit `--mount` from the recorded resolution.
pub async fn resolve_tenant_mounts(
    client: &McpClient,
    target: &str,
    pool: &str,
    domain: &str,
    tenant: Tenant,
) -> Result<Vec<MountSpec>, BootstrapError> {
    match tenant {
        Tenant::Relay => {
            let mut out = Vec::new();
            for child in [RelayChild::DockerRoot, RelayChild::DeployDir] {
                let ds = relay_child_dataset(pool, domain, child)
                    .map_err(|e| BootstrapError::Verify(e.to_string()))?;
                ensure_dataset(client, target, &ds).await?;
                let host = mountpoint_of(client, target, &ds)?;
                let guest = match child {
                    RelayChild::DockerRoot => GUEST_PATH_DOCKER_ROOT,
                    RelayChild::DeployDir => GUEST_PATH_RELAY_DEPLOY,
                };
                chown_guest_uid(client, target, &host, 100000).await?;
                out.push(MountSpec {
                    source: host,
                    guest_path: guest.into(),
                });
            }
            Ok(out)
        }
        Tenant::Cp => {
            let ds = dataset_path(pool, domain, Tenant::Cp)
                .map_err(|e| BootstrapError::Verify(e.to_string()))?;
            ensure_dataset(client, target, &ds).await?;
            let host = mountpoint_of(client, target, &ds)?;
            chown_guest_uid(client, target, &host, 100000).await?;
            Ok(vec![MountSpec {
                source: host,
                guest_path: GUEST_PATH_CP.into(),
            }])
        }
        Tenant::K3sVolumes => {
            let ds = dataset_path(pool, domain, Tenant::K3sVolumes)
                .map_err(|e| BootstrapError::Verify(e.to_string()))?;
            ensure_dataset(client, target, &ds).await?;
            let host = mountpoint_of(client, target, &ds)?;
            chown_guest_uid(client, target, &host, 100000).await?;
            Ok(vec![MountSpec {
                source: host,
                guest_path: GUEST_PATH_K8S_VOLUMES.into(),
            }])
        }
    }
}

// ----------------------------------------------------------- storage info

/// One mount's live usage: capacity + consumed, host-side (the source) and
/// whether the guest's bind mount is actually live (a `pct exec mountpoint`
/// probe — the bind-mount proof, distinct from the host numbers).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MountUsage {
    /// The LXC role riding it (relay/cp/k3s) — the plane.mounts key.
    pub role: String,
    /// The HOST-side source (ZFS dataset or host mount path).
    pub source: String,
    /// The guest mount point (the /srv/data convention path).
    pub guest: String,
    pub size: Option<u64>,
    pub used: Option<u64>,
    /// None = no vmid recorded / probe unreachable; Some(false) = the guest
    /// is down or the mount is missing inside it.
    pub guest_mounted: Option<bool>,
}

/// A read-only snapshot of the durable plane: host capacity + one
/// [`MountUsage`] per recorded plane mount. The DATA tab (TUI) and the
/// `storage info` subcommand read this same shape. EVERY probe degrades —
/// a stopped guest or unreachable command yields None fields, never an
/// error; only transport failure (runner down) surfaces as Err.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StorageInfo {
    /// Human host-capacity summary (zpool or VG + thin pool).
    pub capacity: String,
    pub mounts: Vec<MountUsage>,
}

/// Short capacity string ("1.2T" / "814.7M" / "40K" / "512B") for the tab.
pub fn human_bytes(b: u64) -> String {
    const KIB: f64 = 1024.0;
    let v = b as f64;
    if v >= KIB.powi(4) {
        format!("{:.1}T", v / KIB.powi(4))
    } else if v >= KIB.powi(3) {
        format!("{:.1}G", v / KIB.powi(3))
    } else if v >= KIB.powi(2) {
        format!("{:.0}M", v / KIB.powi(2))
    } else if v >= KIB {
        format!("{:.0}K", v / KIB)
    } else {
        format!("{b}B")
    }
}

/// Host-level capacity summary: ZFS = the pool's alloc/size/free; LVM =
/// VG size/free + the thin pool's data% (the real constraint). Degrades to
/// a "—" string on probe failure — capacity is advisory, not load-bearing.
pub fn host_capacity(client: &McpClient, target: &str, kind: BackendKind, pool: &str) -> String {
    match kind {
        BackendKind::Zfs => {
            let out = crate::bootstrap::exec(
                client,
                target,
                &format!("zpool list -H -p -o size,alloc,free {pool} 2>/dev/null || true"),
                60,
            );
            match out {
                Ok(o) if o.exit_code == Some(0) => {
                    let nums: Vec<u64> = o
                        .stdout
                        .split_ascii_whitespace()
                        .filter_map(|s| s.parse().ok())
                        .collect();
                    if nums.len() >= 3 {
                        return format!(
                            "zpool {pool} · {} alloc of {} · {} free",
                            human_bytes(nums[1]),
                            human_bytes(nums[0]),
                            human_bytes(nums[2])
                        );
                    }
                    "zpool capacity unreadable".into()
                }
                _ => "zpool capacity unreadable".into(),
            }
        }
        BackendKind::LvmThin => {
            let vgs = crate::bootstrap::exec(
                client,
                target,
                &format!(
                    "vgs --noheadings --units b -o vg_size,vg_free {pool} 2>/dev/null || true"
                ),
                60,
            );
            let (size, free) = match vgs {
                Ok(o) if o.exit_code == Some(0) => {
                    let nums: Vec<u64> = o
                        .stdout
                        .split_ascii_whitespace()
                        .filter_map(|s| s.trim_end_matches(['B', 'b']).parse().ok())
                        .collect();
                    if nums.len() >= 2 {
                        (Some(nums[0]), Some(nums[1]))
                    } else {
                        (None, None)
                    }
                }
                _ => (None, None),
            };
            // The thin pool's data% is the constraint that actually bites.
            let pct = thin_pool_data_pct(client, target, pool);
            let mut s = String::new();
            if let (Some(sz), Some(fr)) = (size, free) {
                s.push_str(&format!(
                    "vg {pool} · {} of {} · {} free",
                    human_bytes(sz.saturating_sub(fr)),
                    human_bytes(sz),
                    human_bytes(fr)
                ));
            } else {
                s.push_str(&format!("vg {pool} · size unreadable"));
            }
            if let Some(p) = pct {
                s.push_str(&format!(" · thin pool data {p:.1}%"));
            }
            s
        }
    }
}

/// The VG's thin-pool `data_percent`, or None — advisory only. The pool is
/// named by its `[xxx_tmeta]`/`xxx_tdata` companion pair (same rule as
/// `thin_pool_name`), but `data_percent` lives on the POOL row itself —
/// `lvs` prints the bracketed segment rows with an EMPTY percent column, so
/// reading them (the old bug) always returned None.
fn thin_pool_data_pct(client: &McpClient, target: &str, vg: &str) -> Option<f64> {
    let out = crate::bootstrap::exec(
        client,
        target,
        &format!("lvs -a --noheadings -o lv_name,data_percent {vg} 2>/dev/null || true"),
        60,
    )
    .ok()?;
    parse_lvs_data_pct(&out.stdout)
}

/// Parse `lvs -a -o lv_name,data_percent` output for the VG's thin pool
/// fill. The pool is named by its `_tmeta`/`_tdata` companion pair (the
/// same rule as `thin_pool_name`), but the percent lives on the POOL row
/// itself — the bracketed segment rows print an EMPTY percent column.
fn parse_lvs_data_pct(stdout: &str) -> Option<f64> {
    let rows: Vec<(String, Option<f64>)> = stdout
        .lines()
        .filter_map(|line| {
            let mut parts = line.split_ascii_whitespace();
            let name = parts.next()?.trim_matches(['[', ']']).to_string();
            let pct = parts.next().and_then(|s| s.parse().ok());
            Some((name, pct))
        })
        .collect();
    let pool = rows
        .iter()
        .find(|(n, _)| n.ends_with("_tmeta"))
        .map(|(n, _)| n.trim_end_matches("_tmeta").to_string())
        .filter(|pool| rows.iter().any(|(n, _)| n == &format!("{pool}_tdata")))?;
    rows.iter().find(|(n, _)| n == &pool).and_then(|(_, p)| *p)
}

/// Parse one `H <source> <size> <used>` probe line (— for missing).
fn parse_h_line(line: &str) -> Option<(&str, Option<u64>, Option<u64>)> {
    let rest = line.strip_prefix("H ")?;
    let mut parts = rest.split_ascii_whitespace();
    let src = parts.next()?;
    let size = parts
        .next()
        .and_then(|s| if s == "-" { None } else { s.parse().ok() });
    let used = parts
        .next()
        .and_then(|s| if s == "-" { None } else { s.parse().ok() });
    Some((src, size, used))
}

/// Parse one `G <vmid> <guest> ok|absent|down` probe line.
fn parse_g_line(line: &str) -> Option<(u32, &str, bool)> {
    let rest = line.strip_prefix("G ")?;
    let mut parts = rest.split_ascii_whitespace();
    let vmid: u32 = parts.next()?.parse().ok()?;
    let mp = parts.next()?;
    let ok = parts.next()? == "ok";
    Some((vmid, mp, ok))
}

/// The live, read-only plane snapshot. `mounts` = (role, source, guest,
/// vmid) — the recorded `plane.mounts` + the LXC coords; vmid None skips
/// the guest probe. Three host execs total (sources, guests, capacity).
pub fn storage_info(
    client: &McpClient,
    target: &str,
    kind: BackendKind,
    pool: &str,
    mounts: &[(String, String, String, Option<u32>)],
) -> Result<StorageInfo, BootstrapError> {
    // 1. host-side size/used per source. ZFS dataset sources answer via
    //    `zfs list` (used+avail = effective size); LVM host paths via df
    //    on the mount. Either missing => "H <src> - -".
    let srcs = mounts
        .iter()
        .map(|(_, src, _, _)| src.as_str())
        .collect::<Vec<_>>()
        .join(" ");
    let h_out = crate::bootstrap::exec(
        client,
        target,
        &format!(
            "for p in {srcs}; do \
             if zfs list -H \"$p\" >/dev/null 2>&1; then \
             echo \"H $p $(zfs list -H -p -o used,avail \"$p\" | awk '{{print $1+$2\" \"$1}}')\"; \
             elif mountpoint -q \"$p\" 2>/dev/null; then \
             echo \"H $p $(df -B1 \"$p\" | tail -1 | awk '{{print $2\" \"$3}}')\"; \
             else echo \"H $p - -\"; fi; done"
        ),
        120,
    )?;
    let mut host: std::collections::BTreeMap<&str, (Option<u64>, Option<u64>)> =
        std::collections::BTreeMap::new();
    for line in h_out.stdout.lines() {
        if let Some((src, size, used)) = parse_h_line(line) {
            host.insert(src, (size, used));
        }
    }
    // 2. guest-side bind-mount liveness: one loop over vmid:path pairs.
    let pairs: Vec<(u32, &str)> = mounts
        .iter()
        .filter_map(|(_, _, guest, vmid)| vmid.map(|v| (v, guest.as_str())))
        .collect();
    let mut guests: std::collections::BTreeMap<(u32, String), bool> =
        std::collections::BTreeMap::new();
    if !pairs.is_empty() {
        let spec = pairs
            .iter()
            .map(|(v, mp)| format!("{v}:{mp}"))
            .collect::<Vec<_>>()
            .join(" ");
        let g_out = crate::bootstrap::exec(
            client,
            target,
            &format!(
                "for s in {spec}; do vmid=${{s%%:*}}; mp=${{s#*:}}; \
                 if pct status \"$vmid\" 2>/dev/null | grep -q running; then \
                 if pct exec \"$vmid\" -- mountpoint -q \"$mp\" 2>/dev/null; then \
                 echo \"G $vmid $mp ok\"; else echo \"G $vmid $mp down\"; fi; \
                 else echo \"G $vmid $mp down\"; fi; done"
            ),
            120,
        )?;
        for line in g_out.stdout.lines() {
            if let Some((vmid, mp, ok)) = parse_g_line(line) {
                guests.insert((vmid, mp.to_string()), ok);
            }
        }
    }
    // 3. host capacity + assemble.
    let capacity = host_capacity(client, target, kind, pool);
    let rows = mounts
        .iter()
        .map(|(role, src, guest, vmid)| {
            let (size, used) = host.get(src.as_str()).copied().unwrap_or((None, None));
            let guest_mounted = vmid.and_then(|v| guests.get(&(v, guest.clone())).copied());
            MountUsage {
                role: role.clone(),
                source: src.clone(),
                guest: guest.clone(),
                size,
                used,
                guest_mounted,
            }
        })
        .collect();
    Ok(StorageInfo {
        capacity,
        mounts: rows,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn human_bytes_formats_scales() {
        assert_eq!(human_bytes(0), "0B");
        assert_eq!(human_bytes(512), "512B");
        assert_eq!(human_bytes(40 * 1024), "40K");
        assert_eq!(human_bytes(20 * 1024 * 1024), "20M");
        assert_eq!(human_bytes(40 * 1024 * 1024 * 1024), "40.0G");
        assert_eq!(human_bytes(2 * 1024 * 1024 * 1024 * 1024), "2.0T");
    }

    #[test]
    fn parse_h_line_reads_size_and_used() {
        assert_eq!(
            parse_h_line("H /freehold/x/cp 4096 1024"),
            Some(("/freehold/x/cp", Some(4096), Some(1024)))
        );
        // missing source => both dashes, still a row (degrade, not error)
        assert_eq!(
            parse_h_line("H /freehold/x/cp - -"),
            Some(("/freehold/x/cp", None, None))
        );
        assert_eq!(parse_h_line("garbage"), None);
        assert_eq!(parse_h_line(""), None);
    }

    #[test]
    fn parse_g_line_reads_vmid_mount_and_liveness() {
        assert_eq!(
            parse_g_line("G 100 /var/lib/docker ok"),
            Some((100, "/var/lib/docker", true))
        );
        assert_eq!(
            parse_g_line("G 102 /srv/data/cp down"),
            Some((102, "/srv/data/cp", false))
        );
        assert_eq!(parse_g_line("G 102 /srv/data/cp"), None);
        assert_eq!(parse_g_line("not-g 100 /x ok"), None);
    }

    #[test]
    fn parse_lvs_data_pct_reads_pool_row_not_segments() {
        // The exact shape `lvs -a -o lv_name,data_percent` prints on a real
        // PVE host: the thin POOL is named `data` (data% 2.80 on its own
        // row); the bracketed `[data_tdata]` / `[data_tmeta]` segment rows
        // print an EMPTY percent column. The old bug read the segments and
        // always returned None.
        let out = "  data                                                 2.80 \n\
                   [data_tdata]                                                \n\
                   [data_tmeta]                                                \n\
                   root                                                        \n";
        assert_eq!(parse_lvs_data_pct(out), Some(2.80));
        // no thin pool at all => None, not an error
        assert_eq!(parse_lvs_data_pct("  root\n  swap\n"), None);
        // a _tmeta without its _tdata is not a pool
        assert_eq!(parse_lvs_data_pct("  foo_tmeta 1.0\n"), None);
    }
}
