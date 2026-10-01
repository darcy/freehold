// Package export holds the data-plane export's primitives: the REAL size of
// each durable-plane mount (du on the host path — bind-form mp lines carry
// no size attribute, and a thin LV's lv_size is its provisioned bound, not
// its content) and the host-side bundle shape.
package export

import (
	"fmt"
	"strconv"
	"strings"

	"freehold/platform/provisioning"
)

// ExecFunc is the transport-free host-exec seam (the drive package's alias).
type ExecFunc = provisioning.ExecFunc

// The docker storage-driver dirs INSIDE a daemon root (the relay's
// /var/lib/docker carve-out): the pulled images' unpacked layers live here —
// LIVE-VERIFIED on the librem world: 812M of a 1.0G daemon root, static
// since deploy, and `docker compose pull` re-creates it. The named volumes
// (the relay's Postgres/Redis/MinIO/git — the irreplaceable part) ride
// beside it and stay IN. Both driver names covered: fuse-overlayfs
// (unprivileged LXC) + overlay2 (privileged).
var DriverDirs = []string{"fuse-overlayfs", "overlay2"}

// MountBytes is one recorded mount's real content size, as du reads it.
type MountBytes struct {
	Source string
	Bytes  uint64
}

// EstimateMounts runs `du -sb` (apparent bytes — what tar reads) over every
// recorded plane source and fails closed: a failed du NEVER silently zeroes
// a mount in the estimate the operator confirms against. The excludes are
// du's basename patterns (the driver dirs).
func EstimateMounts(exec ExecFunc, sources, duExcludes []string) ([]MountBytes, uint64, error) {
	if len(sources) == 0 {
		return nil, 0, fmt.Errorf("no durable-plane mounts recorded — nothing to export")
	}
	prefix := "du -sb"
	for _, ex := range duExcludes {
		prefix += " --exclude=" + ex
	}
	out := make([]MountBytes, 0, len(sources))
	var total uint64
	for _, src := range sources {
		res, err := exec(prefix+" "+src, 300)
		if err != nil {
			return nil, 0, err
		}
		code := -1
		if res.ExitCode != nil {
			code = *res.ExitCode
		}
		if code != 0 {
			return nil, 0, fmt.Errorf("du %s: exit %d: %s", src, code, strings.TrimSpace(res.Stderr))
		}
		f := strings.Fields(strings.TrimSpace(res.Stdout))
		if len(f) == 0 {
			return nil, 0, fmt.Errorf("du %s: no output", src)
		}
		n, perr := strconv.ParseUint(f[0], 10, 64)
		if perr != nil {
			return nil, 0, fmt.Errorf("du %s: cannot parse %q", src, f[0])
		}
		out = append(out, MountBytes{Source: src, Bytes: n})
		total += n
	}
	return out, total, nil
}

// TarCmd builds the host-side bundle: ONE gzipped tar of every recorded
// mount (absolute sources, -C / so the members carry their real paths) into
// the dumpdir, skipping the driver dirs (the explicit-path excludes). Runs
// on the substrate host — the mounts live there.
func TarCmd(dumpdir, name string, sources, tarExcludes []string) (string, string, error) {
	if len(sources) == 0 {
		return "", "", fmt.Errorf("no mounts to tar")
	}
	archive := dumpdir + "/" + name + ".tar.gz"
	// Strip the leading slash per member (tar -C / warns otherwise); the
	// members keep their real paths so an untar restores them in place.
	parts := []string{"tar -C / -czf " + archive}
	for _, ex := range tarExcludes {
		parts = append(parts, "--exclude="+ex)
	}
	for _, s := range sources {
		parts = append(parts, strings.TrimPrefix(s, "/"))
	}
	return archive, strings.Join(parts, " "), nil
}
