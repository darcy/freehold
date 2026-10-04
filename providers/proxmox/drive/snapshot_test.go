package drive

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"freehold/contract/client"
	"freehold/platform/provisioning/planebase"
)

// fakeSnap models the snapshot surface the primitives drive: a zfs dataset
// table (mount path -> dataset name), per-dataset snapshots (ds@snap ->
// creation epoch), an lvm VG of thin LVs (snapshot LVs carry their origin +
// creation), and the mount table resolving sources to devices. It records
// every command so tests assert the exact sequence.
type fakeSnap struct {
	cmds    []string
	failOn  string              // substring of cmd → exit 1
	zfsDS   map[string]string   // mount path -> dataset name
	zfsSn   map[string]int64    // ds@snap -> creation epoch
	lvs     map[string][]string // vg -> plain LV names
	snaps   map[string]int64    // snap LV name -> creation epoch
	origin  map[string]string   // snap LV name -> origin LV name
	findmnt map[string]string   // source path -> device path
}

func newFakeSnap() *fakeSnap {
	return &fakeSnap{
		zfsDS:   map[string]string{},
		zfsSn:   map[string]int64{},
		lvs:     map[string][]string{},
		snaps:   map[string]int64{},
		origin:  map[string]string{},
		findmnt: map[string]string{},
	}
}

func (f *fakeSnap) exec(cmd string, _ uint64) (*client.ExecOutcome, error) {
	f.cmds = append(f.cmds, cmd)
	code := func(c int) *int { return &c }
	if f.failOn != "" && strings.Contains(cmd, f.failOn) {
		return &client.ExecOutcome{ExitCode: code(1)}, nil
	}
	switch {
	case strings.HasPrefix(cmd, "zfs list -H -o name "):
		path := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(cmd, "zfs list -H -o name "), ">/dev/null 2>&1"))
		if ds, ok := f.zfsDS[path]; ok {
			return &client.ExecOutcome{Stdout: ds, ExitCode: code(0)}, nil
		}
		if _, ok := f.zfsSn[path]; ok { // the existence probe asks about ds@snap
			return &client.ExecOutcome{ExitCode: code(0)}, nil
		}
		return &client.ExecOutcome{ExitCode: code(1)}, nil

	case strings.HasPrefix(cmd, "zfs list -H -p -o name,creation,used"):
		ds := strings.TrimSpace(strings.TrimPrefix(cmd, "zfs list -H -p -o name,creation,used -t snapshot -r "))
		var out []string
		for key, created := range f.zfsSn {
			if strings.HasPrefix(key, ds+"@") {
				out = append(out, fmt.Sprintf("%s %d 52428800", key, created))
			}
		}
		return &client.ExecOutcome{Stdout: strings.Join(out, "\n"), ExitCode: code(0)}, nil

	case strings.HasPrefix(cmd, "zfs snapshot "):
		key := strings.TrimSpace(strings.TrimPrefix(cmd, "zfs snapshot "))
		f.zfsSn[key] = 1700000000
		return &client.ExecOutcome{ExitCode: code(0)}, nil

	case strings.HasPrefix(cmd, "zfs destroy "):
		delete(f.zfsSn, strings.TrimSpace(strings.TrimPrefix(cmd, "zfs destroy ")))
		return &client.ExecOutcome{ExitCode: code(0)}, nil

	case strings.HasPrefix(cmd, "zfs rollback -r "):
		return &client.ExecOutcome{ExitCode: code(0)}, nil

	case strings.HasPrefix(cmd, "findmnt -n -o SOURCE "):
		src := strings.TrimSpace(strings.TrimPrefix(cmd, "findmnt -n -o SOURCE "))
		if dev, ok := f.findmnt[src]; ok {
			return &client.ExecOutcome{Stdout: dev, ExitCode: code(0)}, nil
		}
		return &client.ExecOutcome{ExitCode: code(1)}, nil

	case strings.HasPrefix(cmd, "lvs --noheadings -o lv_name"):
		// `lvs --noheadings -o lv_name <vg> | awk '{print $1}' | grep -qx
		// <snap>` — model the grep: exit 0 iff the name is an LV in the VG
		// (plain or snapshot). lvs PADS its columns (live-verified), which
		// is what the awk trim is for — the command-shape test pins it.
		fl := strings.Fields(cmd)
		vg, snap := fl[4], fl[len(fl)-1]
		for _, lv := range f.lvs[vg] {
			if lv == snap {
				return &client.ExecOutcome{ExitCode: code(0)}, nil
			}
		}
		if _, ok := f.snaps[snap]; ok {
			return &client.ExecOutcome{ExitCode: code(0)}, nil
		}
		return &client.ExecOutcome{ExitCode: code(1)}, nil

	case strings.Contains(cmd, "lvs --noheadings --separator '|'"):
		// The real command pins LC_ALL=C (no --timeformat — lvm2 2.03.31
		// rejects it) so lv_time's default format is deterministic; the
		// 4th column is the thin snapshot's data_percent (its pool share).
		var out []string
		for snap, created := range f.snaps {
			out = append(out, fmt.Sprintf("%s|%s|%s|0.42", snap, f.origin[snap], time.Unix(created, 0).UTC().Format("2006-01-02 15:04:05 -0700")))
		}
		return &client.ExecOutcome{Stdout: strings.Join(out, "\n"), ExitCode: code(0)}, nil

	case strings.HasPrefix(cmd, "lvcreate -s -kn -n "):
		fl := strings.Fields(cmd)
		snap, vol := fl[4], fl[5]
		pair := strings.SplitN(vol, "/", 2)
		if f.snaps == nil {
			f.snaps = map[string]int64{}
		}
		f.snaps[snap] = 1700000000
		f.origin[snap] = pair[1]
		return &client.ExecOutcome{ExitCode: code(0)}, nil

	case strings.HasPrefix(cmd, "lvremove -f "):
		pair := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(cmd, "lvremove -f ")), "/", 2)
		delete(f.snaps, pair[1])
		delete(f.origin, pair[1])
		return &client.ExecOutcome{ExitCode: code(0)}, nil

	case strings.HasPrefix(cmd, "lvconvert --mergethin "):
		return &client.ExecOutcome{ExitCode: code(0)}, nil
	}
	return &client.ExecOutcome{ExitCode: code(0)}, nil
}

// zfsWorld wires a ZFS plane: two datasets mounted at host paths.
func (f *fakeSnap) zfsWorld() []PlaneVolume {
	f.zfsDS["/rpool/freehold/dom/relay"] = "rpool/freehold/dom/relay"
	f.zfsDS["/rpool/freehold/dom/cp"] = "rpool/freehold/dom/cp"
	return []PlaneVolume{
		{Source: "/rpool/freehold/dom/relay", Zfs: "rpool/freehold/dom/relay"},
		{Source: "/rpool/freehold/dom/cp", Zfs: "rpool/freehold/dom/cp"},
	}
}

// lvmWorld wires an LVM-thin plane: two thin LVs, mapper-style devices.
func (f *fakeSnap) lvmWorld() []PlaneVolume {
	f.lvs["pve"] = []string{"freehold-dom-relay", "freehold-dom-cp"}
	f.findmnt["/srv/plane/relay"] = "/dev/mapper/pve-freehold--dom--relay"
	f.findmnt["/srv/plane/cp"] = "/dev/mapper/pve-freehold--dom--cp"
	return []PlaneVolume{
		{Source: "/srv/plane/relay", VG: "pve", LV: "freehold-dom-relay"},
		{Source: "/srv/plane/cp", VG: "pve", LV: "freehold-dom-cp"},
	}
}

func TestMapperVolumeUnescapesDashes(t *testing.T) {
	vg, lv, err := mapperVolume("/dev/mapper/pve-freehold--dom--relay")
	if err != nil || vg != "pve" || lv != "freehold-dom-relay" {
		t.Fatalf("mapperVolume: vg=%q lv=%q err=%v", vg, lv, err)
	}
	if vg, lv, _ := mapperVolume("/dev/mapper/vg-lv"); vg != "vg" || lv != "lv" {
		t.Fatalf("plain split: vg=%q lv=%q", vg, lv)
	}
	if _, _, err := mapperVolume("/dev/sda1"); err == nil {
		t.Error("non-mapper device must error")
	}
}

func TestResolvePlaneVolumes(t *testing.T) {
	f := newFakeSnap()
	f.zfsDS["/rpool/freehold/dom/relay"] = "rpool/freehold/dom/relay"
	f.findmnt["/srv/plane/relay"] = "/dev/mapper/pve-freehold--dom--relay"
	got, err := ResolvePlaneVolumes(f.exec, string(planebase.KindZfs), []string{"/rpool/freehold/dom/relay"})
	if err != nil || len(got) != 1 || got[0].Zfs != "rpool/freehold/dom/relay" {
		t.Fatalf("zfs resolve: %+v err=%v", got, err)
	}
	got, err = ResolvePlaneVolumes(f.exec, string(planebase.KindLvmThin), []string{"/srv/plane/relay"})
	if err != nil || len(got) != 1 || got[0].VG != "pve" || got[0].LV != "freehold-dom-relay" {
		t.Fatalf("lvm resolve: %+v err=%v", got, err)
	}
	if _, err := ResolvePlaneVolumes(f.exec, "btrfs", []string{"/x"}); err == nil {
		t.Error("unknown kind must error")
	}
	if _, err := ResolvePlaneVolumes(f.exec, string(planebase.KindZfs), nil); err == nil {
		t.Error("empty sources must error")
	}
}

func TestSnapshotCreateZfsSkipsExisting(t *testing.T) {
	f := newFakeSnap()
	vols := f.zfsWorld()
	f.zfsSn["rpool/freehold/dom/relay@fh-1"] = 1 // relay already has it
	if err := SnapshotCreate(f.exec, vols, "fh-1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, ok := f.zfsSn["rpool/freehold/dom/cp@fh-1"]; !ok {
		t.Error("cp must gain the snapshot")
	}
	for _, cmd := range f.cmds {
		if strings.HasPrefix(cmd, "zfs snapshot rpool/freehold/dom/relay@fh-1") {
			t.Errorf("existing snapshot must not be re-created: %v", f.cmds)
		}
	}
}

func TestSnapshotCreateLvmNamesSnapWithOrigin(t *testing.T) {
	f := newFakeSnap()
	vols := f.lvmWorld()
	if err := SnapshotCreate(f.exec, vols, "fh-1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, ok := f.snaps["freehold-dom-relay_fh-1"]; !ok {
		t.Errorf("relay snapshot missing: %v", f.snaps)
	}
	if _, ok := f.snaps["freehold-dom-cp_fh-1"]; !ok {
		t.Errorf("cp snapshot missing: %v", f.snaps)
	}
	if f.origin["freehold-dom-cp_fh-1"] != "freehold-dom-cp" {
		t.Errorf("origin not recorded: %v", f.origin)
	}
}

func TestSnapshotListUnionMarksPartial(t *testing.T) {
	f := newFakeSnap()
	vols := f.zfsWorld()
	f.zfsSn["rpool/freehold/dom/relay@fh-full"] = 100
	f.zfsSn["rpool/freehold/dom/relay@fh-full2"] = 300
	f.zfsSn["rpool/freehold/dom/cp@fh-full"] = 200
	got, err := SnapshotList(f.exec, vols)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].Name != "fh-full2" {
		t.Fatalf("want fh-full2 first (newest), got %+v", got)
	}
	full := got[1]
	if full.Name != "fh-full" || full.Present != 2 || full.Volumes != 2 || full.Created != 200 {
		t.Fatalf("fh-full wrong: %+v", full)
	}
	// fh-full2 exists on one volume only — a partial.
	if p := got[0]; p.Present != 1 || p.Volumes != 2 {
		t.Fatalf("partial not marked: %+v", p)
	}
}

func TestSnapshotListLvmReadsOriginColumn(t *testing.T) {
	f := newFakeSnap()
	vols := f.lvmWorld()
	f.snaps["freehold-dom-relay_fh-1"] = 1700000000
	f.origin["freehold-dom-relay_fh-1"] = "freehold-dom-relay"
	got, err := SnapshotList(f.exec, vols)
	if err != nil || len(got) != 1 || got[0].Name != "fh-1" || got[0].Present != 1 {
		t.Fatalf("list: %+v err=%v", got, err)
	}
}

func TestSnapshotRemoveBothBackends(t *testing.T) {
	f := newFakeSnap()
	f.zfsSn["rpool/freehold/dom/relay@fh-1"] = 1
	if err := SnapshotRemove(f.exec, f.zfsWorld(), "fh-1"); err != nil {
		t.Fatalf("zfs rm: %v", err)
	}
	if len(f.zfsSn) != 0 {
		t.Errorf("zfs snapshot survived: %v", f.zfsSn)
	}
	f2 := newFakeSnap()
	f2.snaps["freehold-dom-relay_fh-1"] = 1
	f2.origin["freehold-dom-relay_fh-1"] = "freehold-dom-relay"
	if err := SnapshotRemove(f2.exec, f2.lvmWorld(), "fh-1"); err != nil {
		t.Fatalf("lvm rm: %v", err)
	}
	if len(f2.snaps) != 0 {
		t.Errorf("lvm snapshot survived: %v", f2.snaps)
	}
	if err := SnapshotRemove(f.exec, f.zfsWorld(), "not-fh"); err == nil {
		t.Error("non-fh name must be refused")
	}
}

// TestSnapshotRemovePartialSkipsMissing: rm of a partial is the crash-cleanup
// path — a volume missing the name is skipped (never a failed zfs destroy of
// a nonexistent snapshot), and the holding volume still gets cleaned.
func TestSnapshotRemovePartialSkipsMissing(t *testing.T) {
	f := newFakeSnap()
	vols := f.zfsWorld()
	f.zfsSn["rpool/freehold/dom/relay@fh-1"] = 1 // cp never got it (the crash)
	if err := SnapshotRemove(f.exec, vols, "fh-1"); err != nil {
		t.Fatalf("rm of a partial must complete: %v", err)
	}
	if len(f.zfsSn) != 0 {
		t.Errorf("the holding volume's snapshot survived: %v", f.zfsSn)
	}
}

func TestSnapshotRollbackCommands(t *testing.T) {
	f := newFakeSnap()
	vols := f.zfsWorld()
	if err := SnapshotCreate(f.exec, vols, "fh-1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := SnapshotRollback(f.exec, vols, "fh-1"); err != nil {
		t.Fatalf("zfs rollback: %v", err)
	}
	want := "zfs rollback -r rpool/freehold/dom/relay@fh-1"
	if !contains(f.cmds, want) {
		t.Errorf("missing %q in %v", want, f.cmds)
	}
	f2 := newFakeSnap()
	vols2 := f2.lvmWorld()
	if err := SnapshotCreate(f2.exec, vols2, "fh-1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := SnapshotRollback(f2.exec, vols2, "fh-1"); err != nil {
		t.Fatalf("lvm rollback: %v", err)
	}
	// ONE command per volume with a RETRY around activation+dd (live-
	// verified: pvestatd's sweep yanks a freshly-activated snapshot's /dev
	// node before the dd can open it). Assert the chain carries every piece.
	wantSeq := []string{
		"umount /srv/plane/relay",
		"lvchange -ay -K /dev/pve/freehold-dom-relay_fh-1",
		"dd if=/dev/pve/freehold-dom-relay_fh-1 of=/dev/pve/freehold-dom-relay bs=4M status=none",
		"mount /srv/plane/relay",
	}
	found := 0
	for _, c := range f2.cmds {
		if strings.Contains(c, "umount /srv/plane/relay") {
			idx := 0
			for _, want := range wantSeq[1:] {
				if !strings.Contains(c, want) {
					t.Errorf("the per-volume chain must carry %q in order", want)
					return
				}
				if strings.Index(c[idx:], want) >= 0 {
					idx += strings.Index(c[idx:], want)
				}
				found++
			}
		}
	}
	if found != 3 { // 3 pieces per volume × 2 volumes
		t.Errorf("want the chained pieces on both volumes, found %d", found)
	}
}

// TestSnapshotRollbackRefusesPartialBeforeMutating: a name missing from any
// volume aborts BEFORE the first rollback command — no half-rolled plane.
func TestSnapshotRollbackRefusesPartialBeforeMutating(t *testing.T) {
	f := newFakeSnap()
	vols := f.zfsWorld()
	f.zfsSn["rpool/freehold/dom/relay@fh-1"] = 1 // cp missing it
	before := len(f.cmds)
	if err := SnapshotRollback(f.exec, vols, "fh-1"); err == nil {
		t.Fatal("a partial target must be refused")
	}
	for _, cmd := range f.cmds[before:] {
		if strings.HasPrefix(cmd, "zfs rollback") {
			t.Errorf("no volume may be mutated on a partial: %v", f.cmds[before:])
		}
	}
}

func TestSnapshotNameAndLabel(t *testing.T) {
	n := SnapshotName(time.Unix(1700000000, 0), "smoke")
	if !strings.HasPrefix(n, "fh-") || !strings.HasSuffix(n, "-smoke") {
		t.Fatalf("name: %q", n)
	}
	if err := ValidateSnapshotLabel("ok-1"); err != nil {
		t.Fatalf("valid label rejected: %v", err)
	}
	if err := ValidateSnapshotLabel("bad@label"); err == nil {
		t.Error("bad label accepted")
	}
}

// TestProbePinsTheAwkTrim: lvs PADS its columns (live-verified on a real
// host — every row has leading spaces), so a bare `grep -qx` never matches
// and the existence probes silently read not-found (rm became a no-op, the
// rollback refused complete snapshots). The probe command must trim.
func TestProbePinsTheAwkTrim(t *testing.T) {
	var cmds []string
	recording := ExecFunc(func(cmd string, _ uint64) (*client.ExecOutcome, error) {
		cmds = append(cmds, cmd)
		return &client.ExecOutcome{ExitCode: &zero2}, nil
	})
	if err := SnapshotCreate(recording, newFakeSnap().lvmWorld(), "fh-1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	found := false
	for _, c := range cmds {
		if strings.Contains(c, "awk '{print $1}'") && strings.Contains(c, "grep -qx") {
			found = true
		}
	}
	if !found {
		t.Errorf("the lvs existence probe must trim padded columns (awk), got %v", cmds)
	}
}

var zero2 = 0

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
