package drive

import (
	"fmt"
	"path/filepath"
	"strings"

	"freehold/platform/provisioning/planebase"
)

// DirPlaneBase is the host root of the directory backend's plane: the
// domain-keyed dirs live under it, one level per domain so multiple worlds
// on one host never share data. Same discipline as the LVM base
// (/freehold/<domain>) — a different root because the VPS root disk has no
// VG to carve from.
const DirPlaneBase = "/srv/data/planes"

// DirMountHostPath is a tenant child's host path on the dir backend:
// <DirPlaneBase>/<domain-slug>/<child>.
func DirMountHostPath(domain, child string) (string, error) {
	dom, err := planebase.NormalizeDomain(domain)
	if err != nil {
		return "", err
	}
	return filepath.Join(DirPlaneBase, dom, child), nil
}

// ResolveDirMounts resolves a tenant's born-at-create MOUNTS on the
// directory backend: one host dir per child (relay keeps TWO — docker root +
// compose deploy dir, preserving the two-child blast radius), created
// idempotently and chowned to the guest's shifted uid. Mirrors
// ResolveLvmMounts' contract; the sources are plain host paths, so PVE binds
// them (backup stays flagged per the /srv/data rule; vzdump skips binds —
// restic is the dir backend's backup story).
func ResolveDirMounts(exec ExecFunc, domain string, tenant planebase.Tenant) ([]planebase.MountSpec, error) {
	type entry struct {
		child string
		guest string
	}
	var entries []entry
	if tenant == planebase.TenantRelay {
		entries = append(entries,
			entry{child: planebase.RelayChildDockerRoot.String(), guest: planebase.GuestPathDockerRoot},
			entry{child: planebase.RelayChildDeployDir.String(), guest: planebase.GuestPathRelayDeploy},
		)
	} else {
		entries = append(entries, entry{child: tenant.String(), guest: tenantGuestPath(tenant)})
	}
	var mounts []planebase.MountSpec
	for _, e := range entries {
		host, err := DirMountHostPath(domain, e.child)
		if err != nil {
			return nil, err
		}
		if _, err := execToOK(exec, fmt.Sprintf("mkdir -p %s && chown %d:%d %s", host, GuestUID, GuestUID, host), "ensure dir-backend mount", 120); err != nil {
			return nil, err
		}
		mounts = append(mounts, planebase.MountSpec{Source: host, GuestPath: e.guest})
	}
	return mounts, nil
}

// DestroyDirTenant removes a tenant's dir-backend subtree for a data teardown.
// Returns (false, nil) when nothing exists (a no-op); the removal is guarded
// to the domain-keyed path (NormalizeDomain rejects anything outside
// [a-z0-9-], so the composed command cannot escape the plane).
func DestroyDirTenant(exec ExecFunc, domain string, tenant planebase.Tenant) (bool, error) {
	var children []string
	if tenant == planebase.TenantRelay {
		children = []string{planebase.RelayChildDockerRoot.String(), planebase.RelayChildDeployDir.String()}
	} else {
		children = []string{tenant.String()}
	}
	destroyed := false
	for _, child := range children {
		host, err := DirMountHostPath(domain, child)
		if err != nil {
			return destroyed, err
		}
		out, err := exec(fmt.Sprintf("[ -d %s ] && echo present || echo absent", host), 60)
		if err != nil {
			return destroyed, err
		}
		if strings.TrimSpace(out.Stdout) != "present" {
			continue
		}
		if _, err := execToOK(exec, "rm -rf "+host, "destroy dir-backend tenant", 300); err != nil {
			return destroyed, fmt.Errorf("%s EXISTS but could not be removed: %w — the tenant's data is INTACT; fix the cause or re-run", host, err)
		}
		destroyed = true
	}
	return destroyed, nil
}
