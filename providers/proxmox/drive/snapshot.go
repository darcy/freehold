package drive

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"freehold/contract/client"
	"freehold/platform/provisioning/planebase"
)

// SnapshotNamePrefix tags every freehold snapshot so list/rm/rollback only
// ever touch their own names.
const SnapshotNamePrefix = "fh-"

// PlaneVolume is one recorded durable-plane mount resolved to its backing
// volume on the host. Exactly one side is set: Zfs (the dataset name) for a
// ZFS backend, VG+LV for LVM-thin.
type PlaneVolume struct {
	Source string // the recorded host path (cfg.Plane.Mounts)
	Zfs    string // zfs dataset name (zfs backend)
	VG     string // volume group (lvm-thin backend)
	LV     string // thin LV name (lvm-thin backend)
}

// SnapshotInfo describes one plane snapshot: a NAME spanning every volume,
// taken at Created (epoch seconds). Used is the per-backend CONSUMPTION
// string (ZFS: the snapshot's CoW bytes, humanized; LVM: the thin
// snapshot's data_percent — its share of the pool) as the backend reports
// it; "" when unknown.
type SnapshotInfo struct {
	Name    string `json:"name"`
	Created int64  `json:"created"` // epoch seconds (0 when unknown)
	// Present is how many volumes hold the snapshot; Volumes is how many
	// volumes the plane has — Present < Volumes marks a partial (a create
	// that died mid-way).
	Present int    `json:"present"`
	Volumes int    `json:"volumes"`
	Used    string `json:"used,omitempty"`
}

// SnapshotName is the plane snapshot name for a time (+ optional label).
func SnapshotName(t time.Time, label string) string {
	name := t.UTC().Format("20060102T150405Z")
	if label != "" {
		name += "-" + label
	}
	return SnapshotNamePrefix + name
}

// ValidateSnapshotLabel rejects a label outside [A-Za-z0-9-] — it becomes a
// zfs/LV name component.
func ValidateSnapshotLabel(label string) error {
	for _, c := range label {
		ok := c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !ok {
			return fmt.Errorf("snapshot label %q may only contain letters, digits, and dashes", label)
		}
	}
	return nil
}

// ResolvePlaneVolumes resolves each recorded mount source to its backing
// volume. kind is the recorded backend kind (`zfs` | `lvmth`): ZFS sources
// resolve via `zfs list -o name <path>`, LVM-thin via the mount's device
// (mapper names are unescaped back to vg/lv).
func ResolvePlaneVolumes(exec ExecFunc, kind string, sources []string) ([]PlaneVolume, error) {
	if len(sources) == 0 {
		return nil, fmt.Errorf("no durable-plane mounts recorded — nothing to snapshot")
	}
	volumes := make([]PlaneVolume, 0, len(sources))
	for _, src := range sources {
		switch kind {
		case string(planebase.KindZfs):
			out, err := execToOK(exec, "zfs list -H -o name "+src, "resolve zfs dataset "+src, 60)
			if err != nil {
				return nil, err
			}
			ds := strings.TrimSpace(out.Stdout)
			if ds == "" {
				return nil, fmt.Errorf("%s resolved to an empty zfs dataset name", src)
			}
			volumes = append(volumes, PlaneVolume{Source: src, Zfs: ds})
		case string(planebase.KindLvmThin):
			out, err := execToOK(exec, "findmnt -n -o SOURCE "+src, "resolve mount device "+src, 60)
			if err != nil {
				return nil, err
			}
			// A stacked (double) mount reports one line per entry — take the
			// first; they are the same device.
			dev := strings.TrimSpace(strings.SplitN(out.Stdout, "\n", 2)[0])
			vg, lv, err := mapperVolume(dev)
			if err != nil {
				return nil, fmt.Errorf("%s is mounted from %q: %w", src, dev, err)
			}
			volumes = append(volumes, PlaneVolume{Source: src, VG: vg, LV: lv})
		default:
			return nil, fmt.Errorf("unknown plane backend kind %q (want zfs or lvmth)", kind)
		}
	}
	return volumes, nil
}

// mapperVolume unescapes a /dev/mapper device path (`vg-lv` with every
// literal dash doubled: `pve-freehold--dom--relay` = vg `pve`, lv
// `freehold-dom-relay`) back to vg + lv names.
func mapperVolume(dev string) (vg, lv string, err error) {
	dev = strings.TrimPrefix(dev, "/dev/mapper/")
	if dev == "" || strings.Contains(dev, "/") {
		return "", "", fmt.Errorf("not an LVM device path")
	}
	var parts []string
	for i := 0; i < len(dev); i++ {
		if dev[i] == '-' && i+1 < len(dev) && dev[i+1] == '-' {
			parts = append(parts, "-")
			i++
			continue
		}
		if dev[i] == '-' {
			parts = append(parts, "\x00")
			continue
		}
		parts = append(parts, string(dev[i]))
	}
	words := strings.SplitN(strings.Join(parts, ""), "\x00", 2)
	if len(words) != 2 || words[0] == "" || words[1] == "" {
		return "", "", fmt.Errorf("cannot split into vg/lv")
	}
	return words[0], words[1], nil
}

// snapLVName is the thin-snapshot LV name for an origin: `<lv>_<snapshot>`.
// LV names are unique per VG and every tenant LV shares one VG, so the
// origin rides in the name.
func snapLVName(origin, snapshot string) string { return origin + "_" + snapshot }

// SnapshotCreate snapshots every volume under one name (idempotent: a name
// already on a volume is kept, not re-created — a retried run after a
// mid-flight failure finishes the set).
func SnapshotCreate(exec ExecFunc, volumes []PlaneVolume, name string) error {
	for _, v := range volumes {
		switch {
		case v.Zfs != "":
			if err := zfsSnap(exec, v.Zfs, name); err != nil {
				return err
			}
		default:
			if err := lvmSnap(exec, v.VG, v.LV, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func zfsSnap(exec ExecFunc, ds, name string) error {
	exists, err := exec("zfs list -H -o name "+ds+"@"+name+" >/dev/null 2>&1", 60)
	if err != nil {
		return err
	}
	if exists.ExitCode != nil && *exists.ExitCode == 0 {
		return nil
	}
	_, err = execToOK(exec, "zfs snapshot "+ds+"@"+name, "zfs snapshot "+ds, 120)
	return err
}

func lvmSnap(exec ExecFunc, vg, lv, name string) error {
	snap := snapLVName(lv, name)
	exists, err := exec("lvs --noheadings -o lv_name "+vg+" 2>/dev/null | awk '{print $1}' | grep -qx "+snap, 60)
	if err != nil {
		return err
	}
	if exists.ExitCode != nil && *exists.ExitCode == 0 {
		return nil
	}
	_, err = execToOK(exec, "lvcreate -s -kn -n "+snap+" "+vg+"/"+lv, "lvcreate snapshot "+snap, 120)
	return err
}

// SnapshotList lists plane snapshots across every volume: the union of
// per-volume names, each with its newest creation time and a partial marker
// when a name is missing from some volumes.
func SnapshotList(exec ExecFunc, volumes []PlaneVolume) ([]SnapshotInfo, error) {
	type seen struct {
		created int64
		present int
		used    string
	}
	byName := map[string]*seen{}
	for _, v := range volumes {
		names, err := volumeSnapshots(exec, v)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			s, ok := byName[n.name]
			if !ok {
				s = &seen{}
				byName[n.name] = s
			}
			if n.created > s.created {
				s.created = n.created
			}
			if n.used != "" {
				s.used = n.used
			}
			s.present++
		}
	}
	out := make([]SnapshotInfo, 0, len(byName))
	for name, s := range byName {
		out = append(out, SnapshotInfo{Name: name, Created: s.created, Present: s.present, Volumes: len(volumes), Used: s.used})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name }) // newest first
	return out, nil
}

// volSnap is one snapshot on one volume.
type volSnap struct {
	name    string
	created int64
	used    string // the backend's consumption string ("" when unknown)
}

// lvmTimeLayout is lv_time's DEFAULT format under LC_ALL=C (the ssh login
// shell carries no locale override, and the command forces C anyway — the
// host's own locale can't bend it).
const lvmTimeLayout = "2006-01-02 15:04:05 -0700"

func volumeSnapshots(exec ExecFunc, v PlaneVolume) ([]volSnap, error) {
	switch {
	case v.Zfs != "":
		out, err := exec("zfs list -H -p -o name,creation,used -t snapshot -r "+v.Zfs, 120)
		if err != nil {
			return nil, err
		}
		if out.ExitCode == nil || *out.ExitCode != 0 {
			return nil, fmt.Errorf("zfs list snapshots on %s failed (exit %d)", v.Zfs, exitOf(out))
		}
		var snaps []volSnap
		for _, line := range strings.Split(out.Stdout, "\n") {
			f := strings.Fields(line)
			if len(f) != 3 || !strings.HasPrefix(f[0], v.Zfs+"@"+SnapshotNamePrefix) {
				continue
			}
			created, _ := strconv.ParseInt(f[1], 10, 64)
			snaps = append(snaps, volSnap{
				name:    strings.TrimPrefix(f[0], v.Zfs+"@"),
				created: created,
				used:    humanZfsUsed(f[2]),
			})
		}
		return snaps, nil
	default:
		// Thin snapshots name their origin in the origin column. NO
		// --timeformat: lvm2 2.03.31 rejects it outright (live-verified on a
		// real host) — LC_ALL=C pins lv_time's default format instead, and
		// it parses deterministically.
		out, err := exec("LC_ALL=C lvs --noheadings --separator '|' -o lv_name,origin,lv_time,data_percent "+v.VG, 120)
		if err != nil {
			return nil, err
		}
		if out.ExitCode == nil || *out.ExitCode != 0 {
			return nil, fmt.Errorf("lvs on %s failed (exit %d): %s", v.VG, exitOf(out), strings.TrimSpace(out.Stderr))
		}
		var snaps []volSnap
		for _, line := range strings.Split(out.Stdout, "\n") {
			f := strings.Split(strings.TrimSpace(line), "|")
			if len(f) != 4 || f[1] != v.LV {
				continue
			}
			name := strings.TrimPrefix(f[0], v.LV+"_")
			if !strings.HasPrefix(name, SnapshotNamePrefix) {
				continue
			}
			created := int64(0)
			if t, perr := time.Parse(lvmTimeLayout, strings.TrimSpace(f[2])); perr == nil {
				created = t.Unix()
			}
			snaps = append(snaps, volSnap{name: name, created: created, used: lvmCowLabel(f[3])})
		}
		return snaps, nil
	}
}

// humanZfsUsed renders a zfs -p used byte count as a compact human string.
func humanZfsUsed(raw string) string {
	n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil || n == 0 {
		return ""
	}
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.1fT", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// lvmCowLabel renders a thin snapshot's data_percent as its pool-share
// label ("" when unparseable).
func lvmCowLabel(percent string) string {
	p, err := strconv.ParseFloat(strings.TrimSpace(percent), 64)
	if err != nil || p <= 0 {
		return ""
	}
	return fmt.Sprintf("%.2f%% of pool", p)
}

// exitOf reads the outcome's exit code (0 when absent).
func exitOf(out *client.ExecOutcome) int {
	if out == nil || out.ExitCode == nil {
		return 0
	}
	return *out.ExitCode
}

// volumeHasSnapshot probes whether one volume holds the name (both backends'
// probes are exit-code based — a missing snapshot is exit 1, not a transport
// error).
func volumeHasSnapshot(exec ExecFunc, v PlaneVolume, name string) (bool, error) {
	switch {
	case v.Zfs != "":
		out, err := exec("zfs list -H -o name "+v.Zfs+"@"+name+" >/dev/null 2>&1", 60)
		if err != nil {
			return false, err
		}
		return out.ExitCode != nil && *out.ExitCode == 0, nil
	default:
		out, err := exec("lvs --noheadings -o lv_name "+v.VG+" 2>/dev/null | awk '{print $1}' | grep -qx "+snapLVName(v.LV, name), 60)
		if err != nil {
			return false, err
		}
		return out.ExitCode != nil && *out.ExitCode == 0, nil
	}
}

// SnapshotRemove removes one plane snapshot name from every volume that
// holds it (missing volumes are skipped — rm of a partial is how a crash
// gets cleaned up).
func SnapshotRemove(exec ExecFunc, volumes []PlaneVolume, name string) error {
	if err := ValidateSnapshotName(name); err != nil {
		return err
	}
	for _, v := range volumes {
		has, err := volumeHasSnapshot(exec, v, name)
		if err != nil {
			return err
		}
		if !has {
			continue
		}
		switch {
		case v.Zfs != "":
			if _, err := execToOK(exec, "zfs destroy "+v.Zfs+"@"+name, "zfs destroy "+v.Zfs+"@"+name, 120); err != nil {
				return err
			}
		default:
			if _, err := execToOK(exec, "lvremove -f "+v.VG+"/"+snapLVName(v.LV, name), "lvremove "+snapLVName(v.LV, name), 120); err != nil {
				return err
			}
		}
	}
	return nil
}

// rollbackStep is one volume's rollback as a single shell command — shared
// by SnapshotRollback (which executes it) and the guest-handoff script (which
// carries it verbatim to a detached host-side run), so the two can never
// drift.
type rollbackStep struct {
	Cmd     string
	Step    string
	Timeout uint64
}

// RollbackStepFor renders one volume's rollback as a single shell command
// plus its exec label/timeout — shared by SnapshotRollback (which executes
// it) and the snapshot verb's guest-handoff script (which carries it
// verbatim to a detached host-side run), so the two can never drift. The
// caller must have validated the name and confirmed the snapshot exists on
// the volume.
func RollbackStepFor(v PlaneVolume, name string) rollbackStep {
	if v.Zfs != "" {
		return rollbackStep{Cmd: "zfs rollback -r " + v.Zfs + "@" + name, Step: "zfs rollback " + v.Zfs, Timeout: 300}
	}
	dev := "/dev/" + v.VG + "/" + v.LV
	snapDev := "/dev/" + v.VG + "/" + snapLVName(v.LV, name)
	// ONE command per volume, with a RETRY around activation+dd.
	// Live-verified on the librem world (PVE host): a freshly
	// activated thin snapshot's /dev node can be yanked away again
	// before dd opens it (pvestatd's sweep deactivates plane LVs of
	// STOPPED guests; separate ssh steps widened the window and
	// even a chained one lost twice). The retry rides it out: each
	// attempt re-copies the identical frozen source, so a partial
	// dd is harmlessly re-copied. The fstab-form mount (the ensure
	// stage's pin) never chases a stale /dev symlink; the failure
	// path best-effort-restores the mount — a guest restarting
	// against an unmounted host path would write into the root fs.
		// NOT conv=sparse: on a DEVICE destination a sparse dd skips writing
		// all-zero blocks, so the origin keeps its NEWER bytes there — a
		// silently non-exact rollback (LIVE-TEST LESSON, librem: the exact
		// dd fills the thin pool instead — every written block allocates,
		// zeros included — and a pool sized to the origins' sum went
		// out-of-data-space mid-rollback, ext4 journals aborting to
		// emergency_ro). The rollback therefore needs POOL HEADROOM: the
		// pool must exceed the origin set's full size, and the post-rollback
		// fstrim of each mount returns the zero blocks. Recovery when a pool
		// exhausts mid-rollback: free space (lvremove the redundant
		// snapshots), fstrim the read-only mounts, e2fsck the journaled
		// volumes, remount rw, restart the guests, re-run the build.
		seq := "umount " + v.Source + " 2>/dev/null; mountpoint -q " + v.Source + " && exit 1; " +
			"n=0; until lvchange -ay -K " + snapDev + " && udevadm settle && " +
			"dd if=" + snapDev + " of=" + dev + " bs=4M status=none; do " +
			"n=$((n+1)); [ $n -ge 5 ] && exit 1; sleep 2; done; " +
			"mount " + v.Source
	return rollbackStep{Cmd: seq, Step: "block-copy " + snapLVName(v.LV, name) + " onto " + v.LV + " (" + v.Source + ")", Timeout: 600}
}

// SnapshotRollback rolls every volume back to the named snapshot. The caller
// stops the guests first (their bind-mounts hold the volume busy). Rollback
// is destructive beyond the point: ZFS `-r` destroys newer snapshots (the
// caller's pre-rollback net survives it only as its send-file export), LVM
// merge consumes the snapshot LV — callers must have confirmed. Every volume
// is probed BEFORE any mutation: a name missing anywhere aborts cleanly
// instead of tearing the plane half-way.
//
// The LVM branch block-copies the target snapshot ONTO the origin (dd) —
// NOT `lvconvert --mergethin`: with only the guests stopped, the fstab-
// pinned host mounts keep the origins active (a mergethin against an active
// origin defers silently — exit 0, data unchanged), and worse, LIVE-
// VERIFIED on lvm2 2.03.31: merging one thin snapshot DESTROYS the origin's
// OTHER thin snapshots (the net included — the escape hatch vanished with
// the merge). dd keeps every snapshot: the target survives (it is dd's
// SOURCE), the pre-rollback net survives, and the umounted-but-active
// origin's blocks are simply overwritten. umount first (a live FS under a
// block overwrite = corruption); a failure aborts before any data moved.
func SnapshotRollback(exec ExecFunc, volumes []PlaneVolume, name string) error {
	if err := ValidateSnapshotName(name); err != nil {
		return err
	}
	for _, v := range volumes {
		has, err := volumeHasSnapshot(exec, v, name)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf("%s is missing from %s — refusing a partial rollback (`snapshot --list` marks partials)", name, v.Source)
		}
	}
	for _, v := range volumes {
		st := RollbackStepFor(v, name)
		if _, err := execToOK(exec, st.Cmd, st.Step, st.Timeout); err != nil {
			if v.Zfs == "" {
				_, _ = exec("mount "+v.Source+" 2>/dev/null; true", 120)
				return fmt.Errorf("%w (the plane volume %s may need hand-remounting: mount %s)", err, v.Source, v.Source)
			}
			return err
		}
	}
	return nil
}

// ValidateSnapshotName rejects anything that is not a freehold plane
// snapshot name (`fh-<timestamp>[-label]`).
func ValidateSnapshotName(name string) error {
	if !strings.HasPrefix(name, SnapshotNamePrefix) || strings.Contains(name, "/") || strings.Contains(name, "@") {
		return fmt.Errorf("snapshot name must be %s<timestamp>[-label]", SnapshotNamePrefix)
	}
	return nil
}
