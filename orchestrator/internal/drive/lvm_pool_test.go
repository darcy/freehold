package drive

import (
	"strings"
	"testing"
)

// ---- placement gate: named thin pool adopt-or-carve ------------------------

func TestEnsureLvmLvNamedThinPoolCarvesWhenAbsent(t *testing.T) {
	f := newFakeRunner()
	c := f.serve(t)
	err := EnsureLvmLv(c, "box", "pve", "freehold-t-d-cp", "/freehold/t-d/cp", 10, 30, "my-pool")
	if err != nil {
		t.Fatalf("EnsureLvmLv: %v", err)
	}
	if i := indexOfContaining(f.cmds, "lvcreate -L 30G -T pve/my-pool"); i < 0 {
		t.Errorf("named pool absent => carve my-pool at the operator size: %v", f.cmds)
	}
	if i := indexOfContaining(f.cmds, "-T pve/my-pool -n freehold-t-d-cp"); i < 0 {
		t.Errorf("the LV must land in the named pool: %v", f.cmds)
	}
}

func TestEnsureLvmLvNamedThinPoolAdoptsExisting(t *testing.T) {
	f := newFakeRunner()
	f.lvs["data_tdata"] = true
	f.lvs["data_tmeta"] = true
	c := f.serve(t)
	err := EnsureLvmLv(c, "box", "pve", "freehold-t-d-cp", "/freehold/t-d/cp", 10, 40, "data")
	if err != nil {
		t.Fatalf("EnsureLvmLv: %v", err)
	}
	if i := indexOfContaining(f.cmds, "lvcreate -L"); i >= 0 {
		t.Errorf("named pool present => adopt, never carve: %v", f.cmds)
	}
	if i := indexOfContaining(f.cmds, "-T pve/data -n freehold-t-d-cp"); i < 0 {
		t.Errorf("the LV must land in the adopted pool: %v", f.cmds)
	}
}

func TestEnsureLvmLvNamedNeverCarvesSecondPool(t *testing.T) {
	// The accident the gate prevents: naming a NEW pool while one already
	// exists must carve the NAMED one, not reuse the detected one.
	f := newFakeRunner()
	f.lvs["data_tdata"] = true
	f.lvs["data_tmeta"] = true
	c := f.serve(t)
	err := EnsureLvmLv(c, "box", "pve", "freehold-t-d-cp", "/freehold/t-d/cp", 10, 40, "fh-new")
	if err != nil {
		t.Fatalf("EnsureLvmLv: %v", err)
	}
	if i := indexOfContaining(f.cmds, "lvcreate -L 40G -T pve/fh-new"); i < 0 {
		t.Errorf("named new pool must be carved by name: %v", f.cmds)
	}
	if i := indexOfContaining(f.cmds, "-T pve/data -n"); i >= 0 {
		t.Errorf("must NOT reuse the detected pool when a name is given: %v", f.cmds)
	}
}

// ---- RemoveThinPool (the teardown --data pool half) -------------------------

// seedPool puts a full thin pool (its own LV + the _tdata/_tmeta pair) into
// the fake VG, mirroring what `lvcreate -L ... -T` leaves behind.
func seedPool(f *fakeRunner, vg, name string) {
	f.lvs[name] = true
	f.lvs[name+"_tdata"] = true
	f.lvs[name+"_tmeta"] = true
}

func TestRemoveThinPoolRemovesEmptyPool(t *testing.T) {
	f := newFakeRunner()
	seedPool(f, "pve", "fh")
	c := f.serve(t)
	if err := RemoveThinPool(c, "box", "pve", "fh"); err != nil {
		t.Fatalf("RemoveThinPool: %v", err)
	}
	if i := indexOfContaining(f.cmds, "lvremove -f pve/fh"); i < 0 {
		t.Errorf("pool removal must run lvremove on the pool: %v", f.cmds)
	}
	if f.lvs["fh"] || f.lvs["fh_tdata"] || f.lvs["fh_tmeta"] {
		t.Errorf("pool trio must be gone: %v", f.lvs)
	}
	if i := indexOfContaining(f.cmds, "pvesm set"); i >= 0 {
		t.Errorf("no storage.cfg pointer => no re-point: %v", f.cmds)
	}
}

func TestRemoveThinPoolRefusesWhileLVRides(t *testing.T) {
	f := newFakeRunner()
	seedPool(f, "pve", "fh")
	// a tenant thin volume still riding the doomed pool
	f.lvs["freehold-t-d-cp"] = true
	f.thinOf["freehold-t-d-cp"] = "fh"
	c := f.serve(t)
	err := RemoveThinPool(c, "box", "pve", "fh")
	if err == nil || !strings.Contains(err.Error(), "still holds") {
		t.Fatalf("must refuse while an LV rides the pool, got %v", err)
	}
	if i := indexOfContaining(f.cmds, "lvremove -f pve/fh"); i >= 0 {
		t.Errorf("refused removal must not run: %v", f.cmds)
	}
}

func TestRemoveThinPoolSurvivorNotARider(t *testing.T) {
	// The rider count must be per-POOL (the pool_lv column), not a global
	// LV count: a SURVIVING pool's own LVs must not block removing the
	// doomed one, or the re-point branch below would be unreachable.
	f := newFakeRunner()
	seedPool(f, "pve", "fh")
	seedPool(f, "pve", "other")
	f.lvs["other-guest-lv"] = true // plain PVE guest volume on the survivor
	c := f.serve(t)
	if err := RemoveThinPool(c, "box", "pve", "fh"); err != nil {
		t.Fatalf("survivor's LVs must not count as riders: %v", err)
	}
	if i := indexOfContaining(f.cmds, "lvremove -f pve/fh"); i < 0 {
		t.Errorf("the doomed pool must still be removed: %v", f.cmds)
	}
}

func TestRemoveThinPoolAbsentIsNoop(t *testing.T) {
	f := newFakeRunner()
	c := f.serve(t)
	if err := RemoveThinPool(c, "box", "pve", "gone"); err != nil {
		t.Fatalf("absent pool = no-op, got %v", err)
	}
	if i := indexOfContaining(f.cmds, "lvremove"); i >= 0 {
		t.Errorf("absent pool must not lvremove: %v", f.cmds)
	}
}

func TestRemoveThinPoolRepointsLocalLvmToSurvivor(t *testing.T) {
	f := newFakeRunner()
	seedPool(f, "pve", "fh")
	seedPool(f, "pve", "other")
	f.storageCfgThinpool = "pve/fh"
	c := f.serve(t)
	if err := RemoveThinPool(c, "box", "pve", "fh"); err != nil {
		t.Fatalf("RemoveThinPool: %v", err)
	}
	// The re-point must land on the SURVIVOR, before the removal, and never
	// on the doomed pool.
	if i := indexOfContaining(f.cmds, "pvesm set local-lvm --thinpool pve/other"); i < 0 {
		t.Errorf("must re-point local-lvm to the surviving pool: %v", f.cmds)
	}
	if i := indexOfContaining(f.cmds, "pvesm set local-lvm --thinpool pve/fh"); i >= 0 {
		t.Errorf("must never re-point local-lvm onto the doomed pool: %v", f.cmds)
	}
	if rep, rem := indexOfContaining(f.cmds, "pvesm set"), indexOfContaining(f.cmds, "lvremove -f pve/fh"); rem >= 0 && rep > rem {
		t.Errorf("re-point must precede the removal: %v", f.cmds)
	}
	if f.storageCfgThinpool != "pve/other" {
		t.Errorf("storage.cfg must end on the survivor, got %q", f.storageCfgThinpool)
	}
}

func TestRemoveThinPoolNoSurvivorLeavesLocalLvm(t *testing.T) {
	// The stock case: local-lvm points at the ONLY pool freehold carved.
	// Removing it leaves no successor — leave storage.cfg alone; the next
	// rebuild re-points it once the new pool is carved.
	f := newFakeRunner()
	seedPool(f, "pve", "fh")
	f.storageCfgThinpool = "pve/fh"
	c := f.serve(t)
	if err := RemoveThinPool(c, "box", "pve", "fh"); err != nil {
		t.Fatalf("RemoveThinPool: %v", err)
	}
	if i := indexOfContaining(f.cmds, "pvesm set"); i >= 0 {
		t.Errorf("no survivor => no re-point (rebuild re-points on next carve): %v", f.cmds)
	}
	if i := indexOfContaining(f.cmds, "lvremove -f pve/fh"); i < 0 {
		t.Errorf("the pool must still be removed: %v", f.cmds)
	}
}
