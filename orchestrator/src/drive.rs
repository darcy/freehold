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
    Backend, BackendKind, ExistingBackend, MountSpec, RelayChild, ResolveAction, Tenant,
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

/// Does an LVM thin pool exist in this VG? (`lvs`, looking for the
/// `pool_tmeta`/`pool_tdata` companion pair that marks a thin pool.)
pub fn vg_has_thin_pool(
    client: &McpClient,
    target: &str,
    vg: &str,
) -> Result<bool, BootstrapError> {
    let names = lvs_names(client, target, vg)?;
    Ok(names.iter().any(|n| {
        n.ends_with("_tmeta")
            && names
                .iter()
                .any(|m| m == &format!("{}_tdata", n.trim_end_matches("_tmeta")))
    }))
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
    let out = crate::bootstrap::exec(
        client,
        target,
        &format!("lvs --noheadings -o lv_name {vg} 2>/dev/null || true"),
        60,
    )?;
    Ok(out.stdout.split_whitespace().map(str::to_string).collect())
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
/// the thin pool is created in the VG once on demand, then the LV, ext4, and
/// a mount at `host_path`. Returns the host mount path (the `mpN` source).
///
/// The pool SIZE is a first-pass fixed value; the LV is named by the caller
/// (the two-place-derivable `freehold-<domain>-<tenant>` names from planebase).
pub async fn ensure_lvm_lv(
    client: &McpClient,
    target: &str,
    vg: &str,
    lv_name: &str,
    host_path: &str,
) -> Result<String, BootstrapError> {
    if !thin_lv_exists(client, target, vg, lv_name)? {
        // thin pool once per VG
        if !vg_has_thin_pool(client, target, vg)? {
            crate::bootstrap::exec_to_ok(
                client,
                target,
                &format!("lvcreate -L 40G -T {vg}/freehold-thin"),
                "lvcreate thin pool",
                300,
            )?;
        }
        let dev = format!("/dev/{vg}/{lv_name}");
        crate::bootstrap::exec_to_ok(
            client,
            target,
            &format!("lvcreate -V 20G -T {vg}/freehold-thin -n {lv_name}"),
            "lvcreate thin LV",
            120,
        )?;
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
        let dev = format!("/dev/{vg}/{lv_name}");
        crate::bootstrap::exec_to_ok(
            client,
            target,
            &format!("mount {dev} {host_path}"),
            "mount LV",
            60,
        )?;
    }
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
                (RelayChild::DockerRoot, "/var/lib/docker"),
                (RelayChild::DeployDir, "/srv/buzz-relay"),
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
                guest_path: "/srv/freehold".into(),
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
                guest_path: "/srv/data/k8s-volumes".into(),
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

/// Build the LXC `--mpN` args: each MountSpec becomes `<source>,mp=<guest>`.
pub fn lxc_mp_args(specs: &[MountSpec]) -> Vec<String> {
    specs
        .iter()
        .enumerate()
        .map(|(i, m)| format!("--mp{i}={},mp={}", m.source, m.guest_path))
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
                    RelayChild::DockerRoot => "/var/lib/docker",
                    RelayChild::DeployDir => "/srv/buzz-relay",
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
                guest_path: "/srv/freehold".into(),
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
                guest_path: "/srv/data/k8s-volumes".into(),
            }])
        }
    }
}
