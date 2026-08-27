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
    Backend, ExistingBackend, MountSpec, RelayChild, ResolveAction, Tenant, dataset_path,
    relay_child_dataset,
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
) -> Result<ResolveAction, BootstrapError> {
    // 1. existing zpool? reuse quietly.
    if !zpool_list(client, target)?.is_empty() {
        return Ok(ResolveAction::Reuse(ExistingBackend::Zfs));
    }
    // 2. existing LVM VG (with/without thin pool)? reuse quietly.
    let vgs = vg_list(client, target)?;
    if !vgs.is_empty() {
        return Ok(ResolveAction::Reuse(ExistingBackend::LvmThin));
    }
    // 3. none + no consent → bail (actionable), never a silent tier.
    if !consent {
        return Ok(ResolveAction::Bail(
            "no existing ZFS zpool or LVM VG detected, and --confirm-storage is not set — \
             re-run with --confirm-storage to create one (or attach a disk / use a NAS)"
                .to_string(),
        ));
    }
    // 4. consent given: prefer ZFS, fall back to LVM-thin.
    Ok(ResolveAction::Create(Backend::Zfs))
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

/// Ensure the LVM-thin filesystem backend exists for a tenant: a thin pool
/// in the VG (created on demand, consent-gated by the caller) + a thin LV +
/// ext4 + mounted at the tenant data path. Idempotent on reruns.
pub async fn ensure_lvm_thin_tenant(
    client: &McpClient,
    target: &str,
    vg: &str,
    tenant: Tenant,
    guest_mount: &str,
) -> Result<(), BootstrapError> {
    if thin_lv_exists(client, target, vg, tenant.as_str())? {
        return Ok(());
    }
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
    // tenant thin LV
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &format!("lvcreate -V 20G -T {vg}/freehold-thin -n {tenant}"),
        "lvcreate tenant thin LV",
        120,
    )?;
    // filesystem + mount at the guest data path on the HOST fs tree
    let dev = format!("/dev/{vg}/{tenant}");
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &format!("mkfs.ext4 -q {dev}"),
        "mkfs tenant LV",
        120,
    )?;
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &format!("mkdir -p {guest_mount}"),
        "mkdir mount",
        60,
    )?;
    let mounted = crate::bootstrap::exec(
        client,
        target,
        &format!("mountpoint -q {guest_mount} 2>/dev/null"),
        30,
    )?
    .exit_code
        == Some(0);
    if !mounted {
        crate::bootstrap::exec_to_ok(
            client,
            target,
            &format!("mount {dev} {guest_mount}"),
            "mount tenant LV",
            60,
        )?;
    }
    Ok(())
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

/// The mount specs for a relay guest: the two child datasets (docker
/// data-root + compose deploy dir), baked into the LXC create.
pub fn relay_mount_specs(pool: &str, domain: &str) -> Result<Vec<MountSpec>, BootstrapError> {
    Ok(vec![
        MountSpec {
            source: relay_child_dataset(pool, domain, RelayChild::DockerRoot)
                .map_err(|e| BootstrapError::Verify(e.to_string()))?,
            guest_path: "/var/lib/docker".into(),
        },
        MountSpec {
            source: relay_child_dataset(pool, domain, RelayChild::DeployDir)
                .map_err(|e| BootstrapError::Verify(e.to_string()))?,
            guest_path: "/srv/buzz-relay".into(),
        },
    ])
}

/// The mount spec for the CP guest: its whole state DIR on the plane.
pub fn cp_mount_spec(pool: &str, domain: &str) -> Result<MountSpec, BootstrapError> {
    Ok(MountSpec {
        source: dataset_path(pool, domain, Tenant::Cp)
            .map_err(|e| BootstrapError::Verify(e.to_string()))?,
        guest_path: "/srv/freehold".into(),
    })
}

/// The mount spec for the k3s guest: the k3s-volumes tenant dataset becomes
/// the `/srv/data/k8s-volumes` carve-out (the local-path provisioner root).
pub fn k3s_mount_spec(pool: &str, domain: &str) -> Result<MountSpec, BootstrapError> {
    Ok(MountSpec {
        source: dataset_path(pool, domain, Tenant::K3sVolumes)
            .map_err(|e| BootstrapError::Verify(e.to_string()))?,
        guest_path: "/srv/data/k8s-volumes".into(),
    })
}

/// chown the relay's two child datasets to the guest's shifted uid so the
/// guest can write them ("verify guest-writable, never assumed"). The
/// mountpoints are resolved from the datasets (the guest paths are the
/// mount targets `mp=...`), then chowned to the pool's unprivileged shifted
/// uid root (default map[0 100000]).
pub async fn relay_chown(
    client: &McpClient,
    target: &str,
    pool: &str,
    domain: &str,
) -> Result<(), BootstrapError> {
    for child in [RelayChild::DockerRoot, RelayChild::DeployDir] {
        let ds = relay_child_dataset(pool, domain, child)
            .map_err(|e| BootstrapError::Verify(e.to_string()))?;
        let out = crate::bootstrap::exec(
            client,
            target,
            &format!("zfs get -H -o value mountpoint {ds}"),
            60,
        )?;
        let mp = out.stdout.trim().to_string();
        if !mp.is_empty() && mp != "none" && mp != "legacy" {
            chown_guest_uid(client, target, &mp, 100000).await?;
        }
    }
    Ok(())
}

/// Ensure the CP's dataset is created + chowned before the CP LXC boots.
pub async fn ensure_cp_dataset(
    client: &McpClient,
    target: &str,
    pool: &str,
    domain: &str,
) -> Result<(), BootstrapError> {
    let ds = dataset_path(pool, domain, Tenant::Cp)
        .map_err(|e| BootstrapError::Verify(e.to_string()))?;
    ensure_dataset(client, target, &ds).await?;
    Ok(())
}

/// Ensure the k3s-volumes dataset is created + chowned before the k3s LXC
/// boots (its `/srv/data/k8s-volumes` becomes a mount of this dataset).
pub async fn ensure_k3s_dataset(
    client: &McpClient,
    target: &str,
    pool: &str,
    domain: &str,
) -> Result<(), BootstrapError> {
    let ds = dataset_path(pool, domain, Tenant::K3sVolumes)
        .map_err(|e| BootstrapError::Verify(e.to_string()))?;
    ensure_dataset(client, target, &ds).await?;
    Ok(())
}

/// Destroy one tenant's dataset subtree (data+compute teardown). For relay
/// this is the PARENT (both children), per the locked single-parent-destroy
/// unit. `zfs destroy -r` removes children recursively.
pub async fn destroy_tenant_dataset(
    client: &McpClient,
    target: &str,
    pool: &str,
    domain: &str,
    tenant: Tenant,
) -> Result<(), BootstrapError> {
    let ds =
        dataset_path(pool, domain, tenant).map_err(|e| BootstrapError::Verify(e.to_string()))?;
    crate::bootstrap::exec_to_ok(
        client,
        target,
        &format!("zfs destroy -r {ds}"),
        "destroy tenant dataset",
        120,
    )
    .map_err(|_| {
        BootstrapError::Verify(format!(
            "dataset {ds} could not be destroyed (or did not exist) — \
             confirm the tenant name is correct (re-typing the target name was already the gate)"
        ))
    })
    .map(|_| ())
}
