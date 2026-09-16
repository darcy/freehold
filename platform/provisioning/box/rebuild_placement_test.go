package box

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"freehold/platform/provisioning/planebase"
)

// ---- storage-thinpool contract-line parsing ----------------------------------

func TestParseStorageThinPools(t *testing.T) {
	if pools, lvm := parseStorageThinPools("noise\nSTORAGE-THINPOOL: data\nmore\n"); !lvm || len(pools) != 1 || pools[0] != "data" {
		t.Errorf("single pool = %v,%v, want [data],true", pools, lvm)
	}
	// The membership list is the adopt-or-carve probe: multi-pool VGs.
	if pools, lvm := parseStorageThinPools("STORAGE-THINPOOL: data,other\n"); !lvm || len(pools) != 2 || pools[0] != "data" || pools[1] != "other" {
		t.Errorf("two pools = %v,%v, want [data other],true", pools, lvm)
	}
	// "-" is resolve's way of saying the VG has no thin pool yet.
	if pools, lvm := parseStorageThinPools("STORAGE-THINPOOL: -\n"); !lvm || len(pools) != 0 {
		t.Errorf("dash must mean no pool: %v,%v, want [],true", pools, lvm)
	}
	// absent line = not an LVM-thin backend (ZFS): the gate is skipped.
	if pools, lvm := parseStorageThinPools("STORAGE-POOL: rpool\n"); lvm || len(pools) != 0 {
		t.Errorf("absent thinpool line = %v,%v, want [],false", pools, lvm)
	}
}

func TestParseGB(t *testing.T) {
	if got, err := parseGB("", 40, "size"); err != nil || got != 40 {
		t.Errorf("blank => default: got %d, %v", got, err)
	}
	if got, err := parseGB("  30  ", 40, "size"); err != nil || got != 30 {
		t.Errorf("30 => 30: got %d, %v", got, err)
	}
	if _, err := parseGB("0", 40, "size"); err == nil {
		t.Error("0 must be rejected")
	}
	if _, err := parseGB("not-a-number", 40, "size"); err == nil {
		t.Error("garbage must be rejected")
	}
	if _, err := parseGB("-5", 40, "size"); err == nil {
		t.Error("negative must be rejected")
	}
}

// ---- stagePlacement: the plane-placement gate --------------------------------
//
// stagePlacement runs `storage resolve` (through the runBin seam) and decides
// where the tenant LVs land: reuse the VG's detected thin pool or carve a
// dedicated named one. The --thin-pool flag answers headless; --yes takes the
// detected pool; otherwise the operator is prompted.

// placementEngine builds a stagePlacement-ready engine: the runBin seam
// answers the `storage resolve` call with the given contract lines; in/out
// are the operator's terminal.
func placementEngine(resolveOut, input string, flags Flags) *Engine {
	e := &Engine{
		F:    flags,
		Bins: Bins{Self: "self"},
		Out:  &bytes.Buffer{},
		In:   strings.NewReader(input),
	}
	e.RunBin = func(bin string, args []string) (bool, string) {
		if len(args) >= 2 && args[0] == "storage" && args[1] == "resolve" {
			return true, resolveOut
		}
		return false, "unexpected call: " + strings.Join(args, " ")
	}
	return e
}

const resolveLvmDetected = "STORAGE-POOL: pve\nSTORAGE-THINPOOL: data\n"
const resolveLvmNone = "STORAGE-POOL: pve\nSTORAGE-THINPOOL: -\n"

func TestStagePlacementFlagReuse(t *testing.T) {
	e := placementEngine(resolveLvmDetected, "", Flags{ThinPool: "data"})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.pool != "pve" || got.thinPool != "data" || got.created {
		t.Errorf("flag naming the DETECTED pool = adopt, got %+v", got)
	}
}

func TestStagePlacementFlagCarve(t *testing.T) {
	e := placementEngine(resolveLvmDetected, "", Flags{ThinPool: "fh-new", PoolSizeGB: 30})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "fh-new" || !got.created {
		t.Errorf("flag naming a NEW pool = carve, got %+v", got)
	}
}

func TestStagePlacementNoFlagNoPoolCarvesDefault(t *testing.T) {
	// The wiped-box case: the VG has no thin pool, no flag — carve the
	// default pool name (never reuse "-").
	e := placementEngine(resolveLvmNone, "", Flags{})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "freehold-thin" || !got.created {
		t.Errorf("no pool + no flag => carve freehold-thin, got %+v", got)
	}
}

func TestStagePlacementYesReusesDetected(t *testing.T) {
	e := placementEngine(resolveLvmDetected, "", Flags{Yes: true})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "data" || got.created {
		t.Errorf("--yes => reuse detected, got %+v", got)
	}
}

func TestStagePlacementInteractiveReuse(t *testing.T) {
	e := placementEngine(resolveLvmDetected, "r\n", Flags{})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "data" || got.created {
		t.Errorf("interactive r => reuse, got %+v", got)
	}
}

func TestStagePlacementInteractiveBlankReuse(t *testing.T) {
	e := placementEngine(resolveLvmDetected, "\n", Flags{})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "data" || got.created {
		t.Errorf("interactive blank => reuse, got %+v", got)
	}
}

func TestStagePlacementInteractiveCarveAtSize(t *testing.T) {
	// A name answer takes a SECOND prompt: the carve size. "30" must land
	// in poolSizeGB (the earlier bug parsed the NAME as a GB).
	e := placementEngine(resolveLvmDetected, "fh-new\n30\n", Flags{PoolSizeGB: 40})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "fh-new" || !got.created {
		t.Errorf("named pool answer => carve, got %+v", got)
	}
	if e.F.PoolSizeGB != 30 {
		t.Errorf("carve size must come from the second prompt, got %d", e.F.PoolSizeGB)
	}
}

func TestStagePlacementInteractiveCarveBlankSizeKeepsDefault(t *testing.T) {
	e := placementEngine(resolveLvmDetected, "fh-new\n\n", Flags{PoolSizeGB: 40})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if !got.created || e.F.PoolSizeGB != 40 {
		t.Errorf("blank size => default 40 kept, got %+v / %d", got, e.F.PoolSizeGB)
	}
}

func TestStagePlacementInteractiveTypeDetectedNameAdopts(t *testing.T) {
	// Typing the detected pool's name verbatim is a no-op carve request:
	// it must ADOPT (created=false), not re-carve beside it.
	e := placementEngine(resolveLvmDetected, "data\n", Flags{})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "data" || got.created {
		t.Errorf("typing the detected name => adopt, got %+v", got)
	}
}

// ---- the two-pool VG regression (round-4 IMPORTANT #3) ---------------------
//
// `created` must come from a MEMBERSHIP probe (is the named pool already in
// the VG?), never from a name-vs-FIRST-pool comparison: in a two-pool VG the
// flag can name the SECOND pool, which a first-pool compare misreports as
// created=true — recording an operator-owned pool that teardown --data would
// then destroy.
const resolveLvmTwoPools = "STORAGE-POOL: pve\nSTORAGE-THINPOOL: data,other\n"

func TestStagePlacementFlagAdoptsSecondPoolInTwoPoolVG(t *testing.T) {
	e := placementEngine(resolveLvmTwoPools, "", Flags{ThinPool: "other"})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "other" || got.created {
		t.Errorf("a named EXISTING second pool must be adopted, never claimed created: %+v", got)
	}
}

func TestStagePlacementFlagCarvesUnknownNameInTwoPoolVG(t *testing.T) {
	e := placementEngine(resolveLvmTwoPools, "", Flags{ThinPool: "fh-new", PoolSizeGB: 30})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "fh-new" || !got.created {
		t.Errorf("a name matching NO existing pool must carve: %+v", got)
	}
}

func TestStagePlacementYesStillReusesFirstPool(t *testing.T) {
	// The --yes default path is untouched by the membership change: it
	// reuses the FIRST detected pool and never claims it created.
	e := placementEngine(resolveLvmTwoPools, "", Flags{Yes: true})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "data" || got.created {
		t.Errorf("--yes => reuse the first pool, got %+v", got)
	}
}

func TestStagePlacementInteractiveTypeSecondPoolNameAdopts(t *testing.T) {
	// Typing an EXISTING pool's name (even the non-first one) must adopt,
	// not ask for a carve size.
	e := placementEngine(resolveLvmTwoPools, "other\n", Flags{})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "other" || got.created {
		t.Errorf("typing an existing pool's name => adopt, got %+v", got)
	}
}

func TestStagePlacementResolveFailure(t *testing.T) {
	e := placementEngine("", "", Flags{})
	e.RunBin = func(string, []string) (bool, string) { return false, "boom" }
	if _, err := e.stagePlacement(); err == nil || !strings.Contains(err.Error(), "storage resolution failed") {
		t.Errorf("resolve failure must abort, got %v", err)
	}
}

func TestStagePlacementZfsSkipsGate(t *testing.T) {
	// ZFS resolve emits no STORAGE-THINPOOL line: the gate must NOT carve
	// (datasets carve themselves) and must NOT claim a created pool.
	e := placementEngine("STORAGE-POOL: rpool\n", "", Flags{Yes: true})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.created || got.thinPool != "" {
		t.Errorf("ZFS (--yes) must pass through with no pool, got %+v", got)
	}
	if got.pool != "rpool" {
		t.Errorf("ZFS pool = %q, want rpool", got.pool)
	}
}

func TestStagePlacementZfsRejectsThinPoolFlag(t *testing.T) {
	e := placementEngine("STORAGE-POOL: rpool\n", "", Flags{ThinPool: "x"})
	if _, err := e.stagePlacement(); err == nil || !strings.Contains(err.Error(), "LVM-thin") {
		t.Errorf("--thin-pool on ZFS must be an actionable error, got %v", err)
	}
}

// ---- stageLocalLvmRepoint: storage.cfg re-point, not pvesm -----------------

// repointEngine builds a stageLocalLvmRepoint-ready engine over a fake
// storage.cfg whose local-lvm block starts at `data`.
func repointEngine(initial string) (*Engine, *string) {
	return repointEngineRiders(initial, "")
}

// repointEngineRiders additionally models the LVs riding local-lvm's current
// pool (the rider guard's probe): non-empty => the re-point must be skipped.
func repointEngineRiders(initial, riders string) (*Engine, *string) {
	cfg := initial
	e := &Engine{Bins: Bins{Self: "self"}, Out: &bytes.Buffer{}}
	e.RunBin = func(bin string, args []string) (bool, string) {
		if len(args) < 2 || args[0] != "exec" {
			return false, "unexpected call: " + strings.Join(args, " ")
		}
		script := args[len(args)-1]
		switch {
		case strings.Contains(script, "pool_lv,lv_name"):
			// The rider guard: LVs riding local-lvm's current pool.
			return true, riders
		case strings.Contains(script, "grep -A2"):
			for _, l := range strings.Split(cfg, "\n") {
				t := strings.TrimSpace(l)
				if strings.HasPrefix(t, "thinpool ") {
					return true, strings.TrimPrefix(t, "thinpool ") + "\n"
				}
			}
			return true, "\n"
		case strings.Contains(script, "awk -v tp="):
			var out []string
			inb := false
			for _, l := range strings.Split(cfg, "\n") {
				if strings.HasPrefix(l, "lvmthin: local-lvm") {
					inb = true
					out = append(out, l)
					continue
				}
				if strings.TrimSpace(l) == "" {
					inb = false
				}
				if inb && strings.HasPrefix(strings.TrimSpace(l), "thinpool ") {
					out = append(out, "\tthinpool freehold-thin")
					continue
				}
				out = append(out, l)
			}
			cfg = strings.Join(out, "\n")
			return true, ""
		}
		return false, "unexpected script: " + script
	}
	return e, &cfg
}

func TestStageLocalLvmRepointCarves(t *testing.T) {
	e, cfg := repointEngine("dir: local\n\tpath /var/lib/vz\n\nlvmthin: local-lvm\n\tthinpool data\n\tvgname pve\n")
	err := e.stageLocalLvmRepoint(&placement{pool: "pve", thinPool: "freehold-thin", created: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*cfg, "thinpool freehold-thin") || strings.Contains(*cfg, "thinpool data") {
		t.Errorf("the local-lvm block must now point at freehold-thin:\n%s", *cfg)
	}
	if !strings.Contains(*cfg, "dir: local") || !strings.Contains(*cfg, "vgname pve") {
		t.Errorf("the edit must be SCOPED — other lines untouched:\n%s", *cfg)
	}
}

func TestStageLocalLvmRepointAlreadyCorrect(t *testing.T) {
	e, cfg := repointEngine("lvmthin: local-lvm\n\tthinpool freehold-thin\n\tvgname pve\n")
	if err := e.stageLocalLvmRepoint(&placement{pool: "pve", thinPool: "freehold-thin", created: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*cfg, "thinpool freehold-thin") {
		t.Errorf("an already-correct pointer must be skipped untouched:\n%s", *cfg)
	}
}

func TestStageLocalLvmRepointNoBlock(t *testing.T) {
	e, _ := repointEngine("dir: local\n\tpath /var/lib/vz\n")
	err := e.stageLocalLvmRepoint(&placement{pool: "pve", thinPool: "freehold-thin", created: true})
	if err == nil || !strings.Contains(err.Error(), "no `lvmthin: local-lvm`") {
		t.Errorf("missing local-lvm block must be a hard error, got %v", err)
	}
}

func TestStageLocalLvmRepointSkipsWhenCurrentPoolHasRiders(t *testing.T) {
	// local-lvm points at `data`, which still holds live guest disks: the
	// re-point to freehold's carved pool must be SKIPPED, not performed.
	e, cfg := repointEngineRiders("lvmthin: local-lvm\n\tthinpool data\n\tvgname pve\n", "vm-100-disk-0\nvm-116-disk-0\n")
	if err := e.stageLocalLvmRepoint(&placement{pool: "pve", thinPool: "freehold-thin", created: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*cfg, "thinpool data") || strings.Contains(*cfg, "freehold-thin") {
		t.Errorf("local-lvm with live riders must be left untouched:\n%s", *cfg)
	}
}

// ---- inventory-driven selection ---------------------------------------------
//
// stagePlacement consumes the JSON inventory line `storage resolve` emits and
// drives a plain-language choice: a single safe backend is a one-keystroke
// confirm; several usable backends must be chosen explicitly (--yes refuses);
// blocked devices are never selectable; freehold's own data offers
// reconnect-or-erase.

// invEngine builds a stagePlacement-ready engine whose resolve emits the given
// inventory.
func invEngine(inv planebase.Inventory, input string, flags Flags) *Engine {
	e := placementEngine("", input, flags)
	resolve := planebase.EncodeInventory(inv)
	e.RunBin = func(bin string, args []string) (bool, string) {
		switch {
		case len(args) >= 2 && args[0] == "storage" && args[1] == "resolve":
			return true, resolve
		case len(args) >= 2 && args[0] == "storage" && args[1] == "destroy":
			return true, "STORAGE-DESTROYED: true\n"
		}
		return false, "unexpected call: " + strings.Join(args, " ")
	}
	return e
}

func TestSelectPlacementSingleSafeZpoolShortcut(t *testing.T) {
	inv := planebase.Inventory{Zpools: []planebase.ZpoolInfo{{Name: "rpool", FreeGB: 1700, Health: "ONLINE"}}}
	e := invEngine(inv, "y\n", Flags{RelayDomain: "t.d"})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.pool != "rpool" || got.thinPool != "" {
		t.Errorf("single safe zpool => rpool, got %+v", got)
	}
}

func TestSelectPlacementMenusSeveralBackends(t *testing.T) {
	inv := planebase.Inventory{
		Zpools: []planebase.ZpoolInfo{{Name: "rpool", FreeGB: 1700}},
		VGs:    VGInfoForTest("pve", "data", 2),
	}
	// recommended is the freehold-less safe zpool (option 1); pick the VG,
	// reuse its only pool, and accept the share (it holds guest LVs).
	e := invEngine(inv, "2\nr\nshare\n", Flags{RelayDomain: "t.d"})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.pool != "pve" || got.thinPool != "data" || got.created {
		t.Errorf("choose VG pve reuse data, got %+v", got)
	}
}

// VGInfoForTest builds a VG with one pool holding `riders` guest LVs.
func VGInfoForTest(name, pool string, riders int) []planebase.VGInfo {
	var rl []string
	for i := 0; i < riders; i++ {
		rl = append(rl, fmt.Sprintf("vm-%d-disk-0", 100+i))
	}
	return []planebase.VGInfo{{Name: name, FreeGB: 100, Pools: []planebase.PoolInfo{{Name: pool, DataPercent: 37, Riders: rl}}}}
}

func TestSelectPlacementYesRefusesMultiple(t *testing.T) {
	inv := planebase.Inventory{
		Zpools: []planebase.ZpoolInfo{{Name: "rpool", FreeGB: 1700}},
		VGs:    VGInfoForTest("pve", "data", 2),
	}
	e := invEngine(inv, "", Flags{RelayDomain: "t.d", Yes: true})
	if _, err := e.stagePlacement(); err == nil || !strings.Contains(err.Error(), "--plane-pool") {
		t.Errorf("--yes with several backends must refuse and name --plane-pool, got %v", err)
	}
}

func TestSelectPlacementPlanePoolPicksNonFirst(t *testing.T) {
	inv := planebase.Inventory{
		VGs: append(
			VGInfoForTest("pve", "data", 2),
			planebase.VGInfo{Name: "pve-fast", FreeGB: 95, Pools: []planebase.PoolInfo{{Name: "data", DataPercent: 11}}},
		),
	}
	// PlanePool selects pve-fast; the pool prompt reuses its only pool.
	e := invEngine(inv, "r\n", Flags{RelayDomain: "t.d", PlanePool: "pve-fast"})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.pool != "pve-fast" || got.thinPool != "data" {
		t.Errorf("--plane-pool pve-fast, got %+v", got)
	}
}

func TestSelectPlacementBlockedDevicesOnlyFails(t *testing.T) {
	inv := planebase.Inventory{Devices: []planebase.DeviceInfo{{Path: "/dev/sda", SizeGB: 1800, Clean: false, Content: "it already has data on it (zfs_member)"}}}
	e := invEngine(inv, "", Flags{RelayDomain: "t.d"})
	_, err := e.stagePlacement()
	if err == nil || !strings.Contains(err.Error(), "no usable storage") {
		t.Errorf("blocked-only inventory must fail actionably, got %v", err)
	}
}

func TestSelectPlacementSharedPoolNeedsShare(t *testing.T) {
	inv := planebase.Inventory{VGs: VGInfoForTest("pve", "data", 3)}
	// choosePool asks reuse/carve first (r), THEN the share confirmation.
	e := invEngine(inv, "r\nnope\n", Flags{RelayDomain: "t.d"})
	if _, err := e.stagePlacement(); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("a shared pool without the share word must stop, got %v", err)
	}
	// With the share word it proceeds.
	e2 := invEngine(inv, "r\nshare\n", Flags{RelayDomain: "t.d"})
	got, err := e2.stagePlacement()
	if err != nil || got.pool != "pve" {
		t.Errorf("share => reuse pve, got %+v %v", got, err)
	}
}

func TestSelectPlacementReconnectsMatchingFreeholdDomain(t *testing.T) {
	inv := planebase.Inventory{VGs: []planebase.VGInfo{{
		Name: "pve", FreeGB: 100,
		Pools: []planebase.PoolInfo{{Name: "freehold-thin", Freehold: planebase.Provenance{Freehold: true, Domains: []string{"t-d"}, Volumes: 4}}},
	}}}
	e := invEngine(inv, "k\nr\n", Flags{RelayDomain: "t.d"})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.pool != "pve" || got.thinPool != "freehold-thin" {
		t.Errorf("reconnect to freehold plane, got %+v", got)
	}
}

func TestSelectPlacementFreeholdDomainMismatchFails(t *testing.T) {
	inv := planebase.Inventory{VGs: []planebase.VGInfo{{
		Name: "pve", FreeGB: 100,
		Pools: []planebase.PoolInfo{{Name: "freehold-thin", Freehold: planebase.Provenance{Freehold: true, Domains: []string{"old-domain"}, Volumes: 4}}},
	}}}
	e := invEngine(inv, "k\n", Flags{RelayDomain: "new.domain"})
	if _, err := e.stagePlacement(); err == nil || !strings.Contains(err.Error(), "old-domain") {
		t.Errorf("domain mismatch on keep must explain, got %v", err)
	}
}

func TestSelectPlacementCleanDeviceOnlyDefers(t *testing.T) {
	inv := planebase.Inventory{Devices: []planebase.DeviceInfo{{Path: "/dev/sdb", SizeGB: 1800, Clean: true}}}
	e := invEngine(inv, "", Flags{RelayDomain: "t.d"})
	_, err := e.stagePlacement()
	if err == nil || !strings.Contains(err.Error(), "later phase") {
		t.Errorf("a clean-disk-only host must defer with a clear message, got %v", err)
	}
}

func TestSelectPlacementYesReconnectPicksFreeholdPool(t *testing.T) {
	// The freehold pool is NOT first: a headless reconnect must pick it, not
	// pools[0], or it would orphan the previous plane.
	inv := planebase.Inventory{VGs: []planebase.VGInfo{{Name: "pve", FreeGB: 100, Pools: []planebase.PoolInfo{
		{Name: "data", DataPercent: 10},
		{Name: "freehold-thin", Freehold: planebase.Provenance{Freehold: true, Domains: []string{"t-d"}, Volumes: 4}},
	}}}}
	e := invEngine(inv, "", Flags{RelayDomain: "t.d", Yes: true})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "freehold-thin" {
		t.Errorf("--yes reconnect must pick the freehold pool, got %+v", got)
	}
}

func TestSelectPlacementAmbiguousFreeholdPoolsRequireName(t *testing.T) {
	inv := planebase.Inventory{VGs: []planebase.VGInfo{{Name: "pve", FreeGB: 100, Pools: []planebase.PoolInfo{
		{Name: "fh-a", Freehold: planebase.Provenance{Freehold: true, Domains: []string{"t-d"}, Volumes: 2}},
		{Name: "fh-b", Freehold: planebase.Provenance{Freehold: true, Domains: []string{"t-d"}, Volumes: 2}},
	}}}}
	// --yes must refuse rather than default to an empty pool name.
	e := invEngine(inv, "", Flags{RelayDomain: "t.d", Yes: true})
	if _, err := e.stagePlacement(); err == nil || !strings.Contains(err.Error(), "--thin-pool") {
		t.Errorf("--yes with two freehold pools must refuse, got %v", err)
	}
	// Interactive: the operator names the pool; k keeps the data.
	e2 := invEngine(inv, "k\nfh-b\n", Flags{RelayDomain: "t.d"})
	got, err := e2.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "fh-b" {
		t.Errorf("ambiguous freehold pools must accept the typed pool, got %+v", got)
	}
}

func TestSelectPlacementErasesFreeholdData(t *testing.T) {
	inv := planebase.Inventory{VGs: []planebase.VGInfo{{
		Name: "pve", FreeGB: 100,
		Pools: []planebase.PoolInfo{{Name: "freehold-thin", Freehold: planebase.Provenance{Freehold: true, Domains: []string{"t-d"}, Volumes: 4}}},
	}}}
	destroyed := 0
	e := invEngine(inv, "e\nr\n", Flags{RelayDomain: "t.d"})
	base := e.RunBin
	e.RunBin = func(bin string, args []string) (bool, string) {
		if len(args) >= 2 && args[0] == "storage" && args[1] == "destroy" {
			destroyed++
		}
		return base(bin, args)
	}
	if _, err := e.stagePlacement(); err != nil {
		t.Fatal(err)
	}
	if destroyed != 3 {
		t.Errorf("erase must destroy all three tenants, got %d", destroyed)
	}
}
