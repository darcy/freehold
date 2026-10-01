package snapshot

import (
	"strings"
	"testing"
	"time"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/providers/proxmox/drive"
)

func vid(v uint32) *uint32 { return &v }

// TestPlaneSourcesStableOrderAndRoleMap: the recorded mounts enumerate in
// stable role order and each source maps back to its role (the rollback
// stop/start mapping).
func TestPlaneSourcesStableOrderAndRoleMap(t *testing.T) {
	cfg := &config.Config{Plane: config.PlaneSpec{
		Mounts: map[string][]config.PlaneMount{
			"k3s":   {{Source: "/plane/k3s-volumes", GuestPath: "/srv/data/k8s-volumes"}},
			"cp":    {{Source: "/plane/cp", GuestPath: "/srv/data/cp"}},
			"relay": {{Source: "/plane/relay", GuestPath: "/srv/data/relay"}, {Source: "/plane/docker", GuestPath: "/var/lib/docker"}},
		},
	}}
	sources, role := planeSources(cfg)
	// Roles sort cp < k3s < relay; within a role the recorded order holds.
	want := []string{"/plane/cp", "/plane/k3s-volumes", "/plane/relay", "/plane/docker"}
	if len(sources) != 4 {
		t.Fatalf("want 4 sources, got %v", sources)
	}
	for i, w := range want {
		if sources[i] != w {
			t.Fatalf("sources not in stable role order: %v", sources)
		}
	}
	if role["/plane/docker"] != "relay" || role["/plane/cp"] != "cp" || role["/plane/k3s-volumes"] != "k3s" {
		t.Fatalf("role map wrong: %v", role)
	}
}

// TestAffectedGuestsMapsRolesToRecordedVMIDs: only roles that own mounts,
// deduped, sorted; a role with no recorded VMID (gone after teardown) is
// skipped.
func TestAffectedGuestsMapsRolesToRecordedVMIDs(t *testing.T) {
	cfg := &config.Config{
		Lxc: config.LxcSpec{
			Relay: config.LxcGuest{Vmid: vid(103)},
			Cp:    config.LxcGuest{Vmid: vid(101)},
			// k3s has no VMID — torn down.
		},
	}
	guests := affectedGuests(cfg, map[string]string{
		"/plane/relay": "relay",
		"/plane/cp":    "cp",
		"/plane/k3s":   "k3s",
	})
	if len(guests) != 2 || guests[0] != 101 || guests[1] != 103 {
		t.Fatalf("guests wrong: %v", guests)
	}
}

// TestPreRollbackSnapshotAlwaysFresh: the net is ALWAYS taken fresh — the
// pre-rollback LIVE state (un-snapshotted divergence included) is exactly
// what a mistaken rollback destroys, and no older snapshot carries it. On
// ZFS the net is ALSO sent to a file, the copy that survives `rollback -r`;
// LVM volumes get no file (mergethin is surgical; the net LV survives).
func TestPreRollbackSnapshotAlwaysFresh(t *testing.T) {
	var cmds []string
	recording := drive.ExecFunc(func(cmd string, _ uint64) (*client.ExecOutcome, error) {
		cmds = append(cmds, cmd)
		// The existence probes must read NOT-FOUND (a fresh world) — exit 0
		// would make the creates skip.
		code := 0
		if strings.HasPrefix(cmd, "zfs list -H -o name ") || strings.HasPrefix(cmd, "lvs --noheadings -o lv_name") {
			code = 1
		}
		return &client.ExecOutcome{ExitCode: &code}, nil
	})
	// Even with a complete snapshot from today: fresh net + send (the reuse
	// shortcut left the day's divergence unrecoverable).
	today := drive.SnapshotInfo{Name: "fh-today", Created: time.Now().UTC().Unix(), Present: 2, Volumes: 2}
	if err := preRollbackSnapshot(recording, zfsVolumes(), []drive.SnapshotInfo{today}, "fh-today"); err != nil {
		t.Fatalf("today snapshot: %v", err)
	}
	hasCreate, hasSend := false, false
	for _, c := range cmds {
		if strings.HasPrefix(c, "zfs snapshot rpool/freehold/dom/relay@fh-") {
			hasCreate = true
		}
		if strings.HasPrefix(c, "zfs send rpool/freehold/dom/relay@fh-") && strings.Contains(c, "> /srv/nobackup/") {
			hasSend = true
		}
	}
	if !hasCreate || !hasSend {
		t.Errorf("want a fresh net create + a send-file, got %v", cmds)
	}

	// With nothing at all: the same shape.
	cmds = nil
	if err := preRollbackSnapshot(recording, zfsVolumes(), nil, "fh-target"); err != nil {
		t.Fatalf("pre-rollback: %v", err)
	}
	sends := 0
	for _, cmd := range cmds {
		if strings.HasPrefix(cmd, "zfs send rpool/freehold/dom/relay@") && strings.Contains(cmd, "> /srv/nobackup/") {
			sends++
		}
	}
	if sends != 1 {
		t.Errorf("exactly one zfs send expected (ZFS volumes only), got %d in %v", sends, cmds)
	}
}

// zfsVolumes is one ZFS volume + one LVM volume — the mixed plane.
func zfsVolumes() []drive.PlaneVolume {
	return []drive.PlaneVolume{
		{Source: "/plane/relay", Zfs: "rpool/freehold/dom/relay"},
		{Source: "/plane/cp", VG: "pve", LV: "freehold-dom-cp"},
	}
}

// TestPreRollbackSendFailureAborts: a failed zfs send must ABORT (the hatch
// must not silently not exist) — the error names the recovery.
func TestPreRollbackSendFailureAborts(t *testing.T) {
	one := 0
	failing := drive.ExecFunc(func(cmd string, _ uint64) (*client.ExecOutcome, error) {
		one++
		c := 0
		if strings.HasPrefix(cmd, "zfs send ") {
			c = 1 // ENOSPC, /srv/nobackup absent, …
		}
		return &client.ExecOutcome{ExitCode: &c}, nil
	})
	err := preRollbackSnapshot(failing, zfsVolumes(), nil, "fh-target")
	if err == nil || !strings.Contains(err.Error(), "ABORTED") {
		t.Fatalf("a failed send must abort the rollback: %v", err)
	}
}

// TestStopGuestClassifiesAffirmatively: the status probe runs FIRST — a
// probe failure aborts (unknown state never rolls back), a confirmed
// not-running guest skips, a confirmed running guest stops (exit 0), and a
// failed pct stop returns an error.
func TestStopGuestClassifiesAffirmatively(t *testing.T) {
	var cmds []string
	okStop := drive.ExecFunc(func(cmd string, _ uint64) (*client.ExecOutcome, error) {
		cmds = append(cmds, cmd)
		code := 0
		out := ""
		if strings.HasPrefix(cmd, "pct status") {
			out = "status: running"
		}
		return &client.ExecOutcome{Stdout: out, ExitCode: &code}, nil
	})
	wasRunning, err := stopGuest(okStop, 101)
	if err != nil || !wasRunning {
		t.Fatalf("running guest must stop cleanly: stopped=%v err=%v", wasRunning, err)
	}
	if len(cmds) != 2 {
		t.Errorf("status + stop expected, got %v", cmds)
	}

	// Not running: confirmed by status, no stop command at all.
	cmds = nil
	stopped := drive.ExecFunc(func(cmd string, _ uint64) (*client.ExecOutcome, error) {
		cmds = append(cmds, cmd)
		code := 0
		out := ""
		if strings.HasPrefix(cmd, "pct status") {
			out = "status: stopped"
		}
		return &client.ExecOutcome{Stdout: out, ExitCode: &code}, nil
	})
	off, err := stopGuest(stopped, 101)
	if err != nil || off {
		t.Errorf("a stopped guest must be (false, nil): %v %v", off, err)
	}
	if len(cmds) != 1 {
		t.Errorf("no stop may run on a stopped guest: %v", cmds)
	}

	// A FAILED status probe = guest gone (the pct contract) — skip. But an
	// UNPARSEABLE status (exit 0, no status line) aborts: unknown state
	// never rolls back.
	probeFail := drive.ExecFunc(func(cmd string, _ uint64) (*client.ExecOutcome, error) {
		code := 512
		return &client.ExecOutcome{ExitCode: &code}, nil
	})
	if off, err := stopGuest(probeFail, 101); err != nil || off {
		t.Errorf("a failed status probe means the guest is gone — skip: %v %v", off, err)
	}
	unparseable := drive.ExecFunc(func(cmd string, _ uint64) (*client.ExecOutcome, error) {
		code := 0
		return &client.ExecOutcome{ExitCode: &code}, nil
	})
	if _, err := stopGuest(unparseable, 101); err == nil {
		t.Fatal("an unparseable status must abort, not print ✓")
	}

	// A failed pct stop returns an error — never a ✓.
	failStop := drive.ExecFunc(func(cmd string, _ uint64) (*client.ExecOutcome, error) {
		code := 0
		out := ""
		if strings.HasPrefix(cmd, "pct status") {
			out = "status: running"
		} else if strings.HasPrefix(cmd, "pct stop") {
			code = 5
		}
		return &client.ExecOutcome{Stdout: out, ExitCode: &code}, nil
	})
	if stopped, err := stopGuest(failStop, 101); err == nil || stopped {
		t.Fatal("a failed pct stop must return (false, err)")
	}
}

// TestStartGuestsCountsFailures: a failed pct start is counted and announced,
// never a ✓.
func TestStartGuestsCountsFailures(t *testing.T) {
	code := 0
	counting := drive.ExecFunc(func(cmd string, _ uint64) (*client.ExecOutcome, error) {
		code++
		c := code % 2 // first call fails, second succeeds
		return &client.ExecOutcome{ExitCode: &c}, nil
	})
	if n := startGuests(counting, []uint32{1, 2}); n != 1 {
		t.Errorf("want 1 failed start, got %d", n)
	}
}

var zero = 0

// TestHandoffScript pins the guest-handoff rollback's script: it waits for
// the CP guest's stop, carries the drive-rendered rollback commands, starts
// exactly the stopped guests back, and revives the CP through the staged
// script.
func TestHandoffScript(t *testing.T) {
	volumes := []drive.PlaneVolume{
		{Source: "/srv/data/cp", Zfs: "pve/cp"},
		{Source: "/srv/data/relay", VG: "pve", LV: "relay"},
	}
	s := handoffScript(volumes, "fh-123-pre", []uint32{104}, 103)
	for _, want := range []string{
		"for i in $(seq 1 120); do pct status 103 | grep -q 'status: running' || break",
		"zfs rollback -r pve/cp@fh-123-pre",
		"mount /srv/data/relay 2>/dev/null; true", // the LVM failure-path restore
		"lvchange -ay -K /dev/pve/relay_fh-123-pre",
		"pct start 104",
		"pct start 103",
		"pct exec 103 -- sh -c '/srv/data/cp/bin/revive-cp.sh'",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("handoff script missing %q\n---\n%s", want, s)
		}
	}
	if !strings.Contains(s, "ROLLBACK FAILED at /srv/data/relay") {
		t.Fatalf("handoff script should report the failing volume\n---\n%s", s)
	}
}
