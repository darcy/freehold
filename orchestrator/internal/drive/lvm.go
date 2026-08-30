package drive

import (
	"fmt"
	"strings"

	"freehold/orchestrator/internal/bootstrap"
	"freehold/orchestrator/internal/client"
	"freehold/orchestrator/internal/planebase"
)

// GuestUID is the uid/gid the guest-facing plane dirs get chowned to: the
// first unprivileged LXC maps root -> 100000 inside the guest.
const GuestUID = 100000

// FreshThinPool is the thin pool ensure carves in a VG that has none.
const FreshThinPool = "freehold-thin"

// TenantLVSizeGB / FreshPoolSizeGB are the DEFAULTS — callers (CLI
// --size-gb / --pool-size-gb, TUI prompts) override. 10 is the halved value
// proven on the test PVE box; the old Rust hardcoded 20/40.
const (
	TenantLVSizeGB  = 10
	FreshPoolSizeGB = 40
)

// EnsureLvmLv ensures ONE thin LV exists + is mounted at a HOST path
// (idempotent): the pool it lands in is resolved by the placement gate —
// thinPool == "" REUSES the VG's EXISTING thin pool (stock PVE:
// `pve/data`) and carves FreshThinPool only when the VG has none; a NAMED
// thinPool is adopted when present, carved (poolSizeGB) when not. Then
// the LV of lvSizeGB, ext4 (blkid-gated), mount (mountpoint-q gated), and
// the fstab pin. Mirrors Rust drive::ensure_lvm_lv byte-for-byte.
func EnsureLvmLv(c *client.McpClient, target, vg, lvName, hostPath string, lvSizeGB, poolSizeGB uint64, thinPool string) error {
	dev := "/dev/" + vg + "/" + lvName
	exists, err := bootstrap.ThinLVExists(c, target, vg, lvName)
	if err != nil {
		return err
	}
	if !exists {
		// Resolve the pool: a named thinPool (the placement gate's choice)
		// is adopted when present, carved (poolSizeGB) when not; empty
		// REUSES the VG's existing thin pool and carves FreshThinPool only
		// when the VG truly has none.
		pool := thinPool
		if pool == "" {
			p, hasPool, err := bootstrap.ThinPoolName(c, target, vg)
			if err != nil {
				return err
			}
			if hasPool {
				pool = p
			} else {
				pool = FreshThinPool
			}
		}
		if poolExists, err := bootstrap.ThinPoolExists(c, target, vg, pool); err != nil {
			return err
		} else if !poolExists {
			if _, err := bootstrap.ExecToOK(c, target,
				fmt.Sprintf("lvcreate -L %dG -T %s/%s", poolSizeGB, vg, pool),
				"lvcreate thin pool", 300); err != nil {
				return err
			}
		}
		if _, err := bootstrap.ExecToOK(c, target,
			fmt.Sprintf("lvcreate -V %dG -T %s/%s -n %s", lvSizeGB, vg, pool, lvName),
			"lvcreate thin LV", 120); err != nil {
			return err
		}
	}
	// mkfs GATED on blkid: runs on first create and recovers a partial
	// failure (a prior run that died between lvcreate and mkfs leaves the LV
	// without a filesystem — re-running must mkfs it, not skip).
	out, err := bootstrap.Exec(c, target, fmt.Sprintf("blkid -s TYPE -o value %s 2>/dev/null", dev), 30)
	if err != nil {
		return err
	}
	hasFS := out.ExitCode != nil && *out.ExitCode == 0
	if !hasFS {
		if _, err := bootstrap.ExecToOK(c, target, fmt.Sprintf("mkfs.ext4 -q %s", dev), "mkfs LV", 120); err != nil {
			return err
		}
	}
	// mkdir + mount at the host path (idempotent).
	if _, err := bootstrap.ExecToOK(c, target, fmt.Sprintf("mkdir -p %s", hostPath), "mkdir mount", 60); err != nil {
		return err
	}
	mp, err := bootstrap.Exec(c, target, fmt.Sprintf("mountpoint -q %s 2>/dev/null", hostPath), 30)
	if err != nil {
		return err
	}
	mounted := mp.ExitCode != nil && *mp.ExitCode == 0
	if !mounted {
		if _, err := bootstrap.ExecToOK(c, target, fmt.Sprintf("mount %s %s", dev, hostPath), "mount LV", 60); err != nil {
			return err
		}
	}
	// Record the mount in /etc/fstab (idempotent) so a HOST REBOOT restores
	// the plane — unlike ZFS (remounted by zfs-mount.service), a bare
	// `mount` of an ext4 LV does not survive reboot; without this the guest
	// would bind-mount an empty dir and write into the host root fs.
	line := fmt.Sprintf("%s %s ext4 defaults 0 2", dev, hostPath)
	if _, err := bootstrap.ExecToOK(c, target,
		fmt.Sprintf("grep -qxF '%s' /etc/fstab || echo '%s' | tee -a /etc/fstab >/dev/null", line, line),
		"record mount in /etc/fstab", 60); err != nil {
		return err
	}
	return nil
}

// ChownGuestUid chowns a mount ROOT (top dir ONLY — never -R) to the
// unprivileged-LXC shifted uid range so the guest can write it (locked:
// "verify guest-writable, never assumed"). The subtree below a mount root
// is container-owned data (docker volumes, service uids like redis 999 /
// postgres 70 / buzz 1000): a RECURSIVE chown re-roots all of it to guest
// root and every non-root service EACCESes — that is what broke a rebuild
// on the SURVIVING plane. This matches what PVE itself does for a freshly
// allocated volume (chown the volume root, non-recursive).
func ChownGuestUid(c *client.McpClient, target, path string, shiftedUID uint32) error {
	_, err := bootstrap.ExecToOK(c, target,
		fmt.Sprintf("chown %d:%d %s", shiftedUID, shiftedUID, path),
		"chown dataset to guest uid", 120)
	return err
}

// ResolveLvmMounts resolves a tenant's born-at-create MOUNTS on an
// LVM-thin backend: one thin LV per tenant (relay keeps TWO — docker data
// root + compose deploy dir, preserving the two-child blast radius), each
// created + mounted + chowned to the guest's shifted uid. `thinPool` is
// the placement gate's pool choice passed straight to EnsureLvmLv ("" =
// reuse-or-carve default). Mirrors Rust drive::resolve_lvm_mounts.
func ResolveLvmMounts(c *client.McpClient, target, vg, domain string, tenant planebase.Tenant, lvSizeGB, poolSizeGB uint64, thinPool string) ([]planebase.MountSpec, error) {
	// Host parent dir under which each tenant LV is mounted.
	base := "/freehold/" + strings.ReplaceAll(domain, ".", "-")
	type entry struct {
		lv    string
		host  string
		guest string
	}
	var entries []entry
	if tenant == planebase.TenantRelay {
		for _, pair := range []struct {
			child planebase.RelayChild
			guest string
		}{
			{planebase.RelayChildDockerRoot, planebase.GuestPathDockerRoot},
			{planebase.RelayChildDeployDir, planebase.GuestPathRelayDeploy},
		} {
			lv, err := planebase.LvmRelayChildLVName(domain, pair.child)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry{lv: lv, host: base + "/" + pair.child.String(), guest: pair.guest})
		}
	} else {
		lv, err := planebase.LvmLVName(domain, tenant)
		if err != nil {
			return nil, err
		}
		child := tenant.String() // cp | k3s-volumes
		entries = append(entries, entry{lv: lv, host: base + "/" + child, guest: tenantGuestPath(tenant)})
	}
	var mounts []planebase.MountSpec
	for _, e := range entries {
		if err := EnsureLvmLv(c, target, vg, e.lv, e.host, lvSizeGB, poolSizeGB, thinPool); err != nil {
			return nil, err
		}
		// Fresh ext4 is root-owned: chown to the guest's shifted uid, same
		// as the ZFS path (locked: guest-writable, never assumed).
		if err := ChownGuestUid(c, target, e.host, GuestUID); err != nil {
			return nil, err
		}
		mounts = append(mounts, planebase.MountSpec{Source: e.host, GuestPath: e.guest})
	}
	return mounts, nil
}

// tenantGuestPath maps a non-relay tenant to its guest mount point.
func tenantGuestPath(t planebase.Tenant) string {
	if t == planebase.TenantCp {
		return planebase.GuestPathCP
	}
	return planebase.GuestPathK8sVolumes
}

// MountpointOf returns a dataset's host-mountable path (`zfs get
// mountpoint`). PVE's `mpN` rejects a bare dataset name — it needs an
// absolute host path. Mirrors Rust drive::mountpoint_of.
func MountpointOf(c *client.McpClient, target, dataset string) (string, error) {
	out, err := bootstrap.Exec(c, target,
		fmt.Sprintf("zfs get -H -o value mountpoint %s", dataset), 60)
	if err != nil {
		return "", err
	}
	mp := strings.TrimSpace(out.Stdout)
	if mp == "" || mp == "none" || mp == "legacy" || !strings.HasPrefix(mp, "/") {
		return "", fmt.Errorf("dataset %s has no usable host mountpoint (%q) — ensure the dataset is created and mounted", dataset, mp)
	}
	return mp, nil
}

// ResolveTenantMounts resolves a tenant's born-at-create MOUNTS on a ZFS
// backend: ensure the dataset(s) exist (idempotent), chown them to the
// guest's shifted uid, and resolve each to its real host mountpoint (PVE's
// `mpN` rejects a bare dataset name). Mirrors Rust
// drive::resolve_tenant_mounts.
func ResolveTenantMounts(c *client.McpClient, target, pool, domain string, tenant planebase.Tenant) ([]planebase.MountSpec, error) {
	type entry struct {
		ds    string
		guest string
	}
	var entries []entry
	if tenant == planebase.TenantRelay {
		for _, pair := range []struct {
			child planebase.RelayChild
			guest string
		}{
			{planebase.RelayChildDockerRoot, planebase.GuestPathDockerRoot},
			{planebase.RelayChildDeployDir, planebase.GuestPathRelayDeploy},
		} {
			ds, err := planebase.RelayChildDataset(pool, domain, pair.child)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry{ds: ds, guest: pair.guest})
		}
	} else {
		ds, err := planebase.DatasetPath(pool, domain, tenant)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{ds: ds, guest: tenantGuestPath(tenant)})
	}
	var mounts []planebase.MountSpec
	for _, e := range entries {
		if err := bootstrap.EnsureDataset(c, target, e.ds); err != nil {
			return nil, err
		}
		host, err := MountpointOf(c, target, e.ds)
		if err != nil {
			return nil, err
		}
		if err := ChownGuestUid(c, target, host, GuestUID); err != nil {
			return nil, err
		}
		mounts = append(mounts, planebase.MountSpec{Source: host, GuestPath: e.guest})
	}
	return mounts, nil
}

// DestroyLvmTenant removes a tenant's thin LVs for a data+compute teardown.
// Relay destroys BOTH child LVs (docker-root + deploy), the parent-destroy
// unit. Returns (false, nil) when every target LV is ABSENT (a no-op),
// (true, nil) when at least one was removed; a real `lvremove` failure is
// an error and the tenant's data is INTACT. Mirrors Rust
// drive::destroy_lvm_tenant.
func DestroyLvmTenant(c *client.McpClient, target, vg, domain string, tenant planebase.Tenant) (bool, error) {
	var lvs []string
	if tenant == planebase.TenantRelay {
		for _, child := range []planebase.RelayChild{planebase.RelayChildDockerRoot, planebase.RelayChildDeployDir} {
			lv, err := planebase.LvmRelayChildLVName(domain, child)
			if err != nil {
				return false, err
			}
			lvs = append(lvs, lv)
		}
	} else {
		lv, err := planebase.LvmLVName(domain, tenant)
		if err != nil {
			return false, err
		}
		lvs = append(lvs, lv)
	}
	destroyed := false
	for _, lv := range lvs {
		exists, err := bootstrap.ThinLVExists(c, target, vg, lv)
		if err != nil {
			return false, err
		}
		if !exists {
			continue
		}
		dev := "/dev/" + vg + "/" + lv
		// Unmount + strip the fstab line FIRST — `lvremove -f` skips the
		// prompt, NOT the open-count check, so an LV still mounted at
		// /freehold/<domain>/... would be refused and the teardown would
		// bail with the LXCs already gone. umount failing is tolerated here
		// (the LV may never have been mounted); if it is genuinely busy the
		// lvremove below fails and surfaces it honestly.
		_, _ = bootstrap.Exec(c, target,
			fmt.Sprintf("umount %s 2>/dev/null; sed -i '\\|^%s |d' /etc/fstab; true", dev, dev), 60)
		if _, err := bootstrap.ExecToOK(c, target,
			fmt.Sprintf("lvremove -f %s/%s", vg, lv), "lvremove tenant thin LV", 120); err != nil {
			return false, fmt.Errorf("LV %s/%s EXISTS but could not be removed: %w — the tenant's data is INTACT; fix the cause or re-run", vg, lv, err)
		}
		destroyed = true
	}
	return destroyed, nil
}

// DestroyTenantDataset removes a tenant's ZFS dataset subtree (relay = both
// child datasets). Existence probe FIRST — an absent dataset is a no-op,
// not an error (the same probe EnsureDataset uses). Mirrors Rust
// drive::destroy_tenant_dataset.
func DestroyTenantDataset(c *client.McpClient, target, pool, domain string, tenant planebase.Tenant) (bool, error) {
	var datasets []string
	if tenant == planebase.TenantRelay {
		for _, child := range []planebase.RelayChild{planebase.RelayChildDockerRoot, planebase.RelayChildDeployDir} {
			ds, err := planebase.RelayChildDataset(pool, domain, child)
			if err != nil {
				return false, err
			}
			datasets = append(datasets, ds)
		}
	} else {
		ds, err := planebase.DatasetPath(pool, domain, tenant)
		if err != nil {
			return false, err
		}
		datasets = append(datasets, ds)
	}
	destroyed := false
	for _, ds := range datasets {
		out, err := bootstrap.Exec(c, target,
			fmt.Sprintf("zfs list -H -o name %s >/dev/null 2>&1", ds), 60)
		if err != nil {
			return false, err
		}
		if out.ExitCode == nil || *out.ExitCode != 0 {
			continue // absent -> no-op
		}
		if _, err := bootstrap.ExecToOK(c, target,
			fmt.Sprintf("zfs destroy -r %s", ds), "destroy tenant dataset", 120); err != nil {
			return false, fmt.Errorf("dataset %s EXISTS but could not be destroyed: %w — the tenant's data is INTACT; fix the cause (busy/ref'ed) or re-run. The teardown must not delete the config mapping for data that survived", ds, err)
		}
		destroyed = true
	}
	return destroyed, nil
}

// DestroyTenantBackend dispatches the tenant destroy on backend kind (ZFS
// `zfs destroy -r` vs LVM `lvremove`). `backend` is the pool/VG name.
// Returns (false, nil) when absent (a no-op), (true, nil) when destroyed.
func DestroyTenantBackend(c *client.McpClient, target string, kind planebase.BackendKind, backend, domain string, tenant planebase.Tenant) (bool, error) {
	switch kind {
	case planebase.KindZfs:
		return DestroyTenantDataset(c, target, backend, domain, tenant)
	case planebase.KindLvmThin:
		return DestroyLvmTenant(c, target, backend, domain, tenant)
	default:
		return false, fmt.Errorf("unknown storage backend kind %q", kind)
	}
}

// RemoveThinPool removes a thin pool from the VG — the --data half of the
// "full teardown", called only for pools freehold itself created (config
// plane.thin_pool; the CLI gates it). Absent pool = no-op. Refuses while
// any non-pool LV still rides it (destroy the tenant LVs first). Guarded
// so a full teardown NEVER leaves PVE's stock local-lvm storage dangling:
// if it still points at the doomed pool it is re-pointed to a surviving
// pool when the VG has one; when the VG would be left with NO pool (the
// stock case — local-lvm pointed at the only pool freehold carved) it is
// left alone and the next rebuild's storage stage re-points it once the
// new pool is carved.
func RemoveThinPool(c *client.McpClient, target, vg, pool string) error {
	exists, err := bootstrap.ThinPoolExists(c, target, vg, pool)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	riders, err := lvsRiders(c, target, vg, pool)
	if err != nil {
		return err
	}
	if len(riders) > 0 {
		return fmt.Errorf("thin pool %s/%s still holds %d LVs (%s) — destroy the tenant LVs first",
			vg, pool, len(riders), strings.Join(riders, ", "))
	}
	// Re-point PVE's local-lvm off the doomed pool BEFORE removing it, so
	// the full teardown never leaves storage.cfg pointing at a dead pool.
	out, err := bootstrap.Exec(c, target,
		"grep -oE 'thinpool: *[^ ]+' /etc/pve/storage.cfg 2>/dev/null | head -1 || true", 30)
	if err != nil {
		return err
	}
	if strings.Contains(out.Stdout, vg+"/"+pool) {
		other, found, err := bootstrap.ThinPoolNameOther(c, target, vg, pool)
		if err != nil {
			return err
		}
		if found {
			if _, err := bootstrap.ExecToOK(c, target,
				fmt.Sprintf("pvesm set local-lvm --thinpool %s/%s", vg, other),
				"re-point local-lvm off the doomed pool", 60); err != nil {
				return err
			}
		}
		// No surviving pool: leave local-lvm as-is; the next rebuild's
		// storage stage re-points it once the new pool is carved.
	}
	if _, err := bootstrap.ExecToOK(c, target,
		fmt.Sprintf("lvremove -f %s/%s", vg, pool), "lvremove thin pool", 300); err != nil {
		return fmt.Errorf("thin pool %s/%s EXISTS but could not be removed: %w — fix the cause (still active?) or re-run", vg, pool, err)
	}
	return nil
}

// lvsRiders lists the LVs that RIDE a specific thin pool: `lvs -o
// pool_lv,lv_name` reports each thin volume's parent pool (a blank field
// for non-thin LVs). Counting by name instead would mis-count a surviving
// pool's own LVs — and any PVE guest volume on it — as riders of the
// doomed pool, refusing removal exactly when the re-point branch needs it.
func lvsRiders(c *client.McpClient, target, vg, pool string) ([]string, error) {
	out, err := bootstrap.Exec(c, target, fmt.Sprintf("lvs --noheadings -o pool_lv,lv_name %s 2>/dev/null || true", vg), 60)
	if err != nil {
		return nil, err
	}
	var riders []string
	want := vg + "/" + pool
	for _, line := range strings.Split(out.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == want {
			riders = append(riders, fields[1])
		}
	}
	return riders, nil
}
