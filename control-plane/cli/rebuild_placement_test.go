package cli

import (
	"bytes"
	"strings"
	"testing"
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
func placementEngine(resolveOut, input string, flags rebuildFlags) *rebuildEngine {
	e := &rebuildEngine{
		f:    flags,
		bins: rebuildBins{Self: "self"},
		out:  &bytes.Buffer{},
		in:   strings.NewReader(input),
	}
	e.runBin = func(bin string, args []string) (bool, string) {
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
	e := placementEngine(resolveLvmDetected, "", rebuildFlags{thinPool: "data"})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.pool != "pve" || got.thinPool != "data" || got.created {
		t.Errorf("flag naming the DETECTED pool = adopt, got %+v", got)
	}
}

func TestStagePlacementFlagCarve(t *testing.T) {
	e := placementEngine(resolveLvmDetected, "", rebuildFlags{thinPool: "fh-new", poolSizeGB: 30})
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
	e := placementEngine(resolveLvmNone, "", rebuildFlags{})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "freehold-thin" || !got.created {
		t.Errorf("no pool + no flag => carve freehold-thin, got %+v", got)
	}
}

func TestStagePlacementYesReusesDetected(t *testing.T) {
	e := placementEngine(resolveLvmDetected, "", rebuildFlags{yes: true})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "data" || got.created {
		t.Errorf("--yes => reuse detected, got %+v", got)
	}
}

func TestStagePlacementInteractiveReuse(t *testing.T) {
	e := placementEngine(resolveLvmDetected, "r\n", rebuildFlags{})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "data" || got.created {
		t.Errorf("interactive r => reuse, got %+v", got)
	}
}

func TestStagePlacementInteractiveBlankReuse(t *testing.T) {
	e := placementEngine(resolveLvmDetected, "\n", rebuildFlags{})
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
	e := placementEngine(resolveLvmDetected, "fh-new\n30\n", rebuildFlags{poolSizeGB: 40})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "fh-new" || !got.created {
		t.Errorf("named pool answer => carve, got %+v", got)
	}
	if e.f.poolSizeGB != 30 {
		t.Errorf("carve size must come from the second prompt, got %d", e.f.poolSizeGB)
	}
}

func TestStagePlacementInteractiveCarveBlankSizeKeepsDefault(t *testing.T) {
	e := placementEngine(resolveLvmDetected, "fh-new\n\n", rebuildFlags{poolSizeGB: 40})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if !got.created || e.f.poolSizeGB != 40 {
		t.Errorf("blank size => default 40 kept, got %+v / %d", got, e.f.poolSizeGB)
	}
}

func TestStagePlacementInteractiveTypeDetectedNameAdopts(t *testing.T) {
	// Typing the detected pool's name verbatim is a no-op carve request:
	// it must ADOPT (created=false), not re-carve beside it.
	e := placementEngine(resolveLvmDetected, "data\n", rebuildFlags{})
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
	e := placementEngine(resolveLvmTwoPools, "", rebuildFlags{thinPool: "other"})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "other" || got.created {
		t.Errorf("a named EXISTING second pool must be adopted, never claimed created: %+v", got)
	}
}

func TestStagePlacementFlagCarvesUnknownNameInTwoPoolVG(t *testing.T) {
	e := placementEngine(resolveLvmTwoPools, "", rebuildFlags{thinPool: "fh-new", poolSizeGB: 30})
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
	e := placementEngine(resolveLvmTwoPools, "", rebuildFlags{yes: true})
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
	e := placementEngine(resolveLvmTwoPools, "other\n", rebuildFlags{})
	got, err := e.stagePlacement()
	if err != nil {
		t.Fatal(err)
	}
	if got.thinPool != "other" || got.created {
		t.Errorf("typing an existing pool's name => adopt, got %+v", got)
	}
}

func TestStagePlacementResolveFailure(t *testing.T) {
	e := placementEngine("", "", rebuildFlags{})
	e.runBin = func(string, []string) (bool, string) { return false, "boom" }
	if _, err := e.stagePlacement(); err == nil || !strings.Contains(err.Error(), "storage resolution failed") {
		t.Errorf("resolve failure must abort, got %v", err)
	}
}

func TestStagePlacementZfsSkipsGate(t *testing.T) {
	// ZFS resolve emits no STORAGE-THINPOOL line: the gate must NOT carve
	// (datasets carve themselves) and must NOT claim a created pool.
	e := placementEngine("STORAGE-POOL: rpool\n", "", rebuildFlags{yes: true})
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
	e := placementEngine("STORAGE-POOL: rpool\n", "", rebuildFlags{thinPool: "x"})
	if _, err := e.stagePlacement(); err == nil || !strings.Contains(err.Error(), "LVM-thin") {
		t.Errorf("--thin-pool on ZFS must be an actionable error, got %v", err)
	}
}

// ---- stageLocalLvmRepoint: storage.cfg re-point, not pvesm -----------------

// repointEngine builds a stageLocalLvmRepoint-ready engine over a fake
// storage.cfg whose local-lvm block starts at `data`.
func repointEngine(initial string) (*rebuildEngine, *string) {
	cfg := initial
	e := &rebuildEngine{bins: rebuildBins{Self: "self"}, out: &bytes.Buffer{}}
	e.runBin = func(bin string, args []string) (bool, string) {
		if len(args) < 2 || args[0] != "exec" {
			return false, "unexpected call: " + strings.Join(args, " ")
		}
		script := args[len(args)-1]
		switch {
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
