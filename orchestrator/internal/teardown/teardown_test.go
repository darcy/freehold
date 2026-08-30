package teardown

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records every interface call; no subprocesses.
type fakeRunner struct {
	execs    []string
	lxcRoles []string
	pools    []string // "vg/pool" entries DestroyPool saw
	failExec string   // substring => Exec returns false
}

func (f *fakeRunner) Exec(cmd string) (bool, string) {
	f.execs = append(f.execs, cmd)
	if f.failExec != "" && strings.Contains(cmd, f.failExec) {
		return false, ""
	}
	return true, "0"
}

func (f *fakeRunner) DestroyOneLxc(role string, vmid *uint32) ([]string, error) {
	f.lxcRoles = append(f.lxcRoles, role)
	if vmid == nil {
		return []string{fmt.Sprintf("%s LXC: never created (no vmid recorded)", role)}, nil
	}
	return []string{fmt.Sprintf("destroyed %s LXC %d", role, *vmid)}, nil
}

func (f *fakeRunner) DestroyDataset(tenant, domain, pool, kind, dataset string) (bool, error) {
	return true, nil
}

func (f *fakeRunner) DestroyPool(vg, pool string) error {
	f.pools = append(f.pools, vg+"/"+pool)
	return nil
}

func testCfg(t *testing.T, data bool) (*Cfg, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	conf := filepath.Join(dir, "config.toml")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte("domain = \"world.test\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Cfg{
		WorldHome:     home,
		ConfigPath:    conf,
		RunNTarget:    "proxmox-box",
		RunnerComment: "freehold-world-1234",
		Domain:        "world.test",
		Pool:          "pve",
		Managed:       []string{"relay", "cp"},
		Vmid: map[string]*uint32{
			"relay": ptr32(100),
			"cp":    ptr32(101),
		},
		ThinPool: "fh-thin",
		Data:     data,
	}, &fakeRunner{}
}

func ptr32(v uint32) *uint32 { return &v }

// The default whole-world teardown is the COMPUTE teardown: it destroys the
// LXCs but keeps the config INTACT — including the recorded LXC coordinates
// (vmid + ip are operator-owned facts: proxy targets + static assignments),
// the world home, and the door key — so rebuild re-boots the SAME world.
func TestWholeWorldKeepsConfigAndDoor(t *testing.T) {
	cfg, r := testCfg(t, false)

	before, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(r, cfg, ScopeWholeWorld, true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(r.pools) != 0 {
		t.Errorf("no --data => no pool removal, got %v", r.pools)
	}
	// Regression (operator report 2026-08-30): compute teardown must NOT
	// strip the LXC coordinates — the config file comes back byte-identical.
	after, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("compute teardown must leave the config byte-identical:\nbefore: %s\nafter: %s", before, after)
	}
	for _, cmd := range r.execs {
		if strings.Contains(cmd, "authorized_keys") {
			t.Errorf("default teardown must NOT touch the door: %q", cmd)
		}
	}
	if _, err := os.Stat(cfg.WorldHome); err != nil {
		t.Error("default teardown must keep the world home")
	}
	if _, err := os.Stat(cfg.ConfigPath); err != nil {
		t.Error("default teardown must keep the config file")
	}
	if !strings.Contains(report, "config KEPT INTACT") || !strings.Contains(report, "door key KEPT") {
		t.Errorf("report must say what was kept:\n%s", report)
	}
	if len(r.lxcRoles) != 2 {
		t.Errorf("both managed LXCs must be destroyed, got %v", r.lxcRoles)
	}
}

// --data on a whole world is the FULL teardown: tenant datasets, the
// freehold-created thin pool, then the door key, the world home, and the
// config (LAST, so the CLI can still show the report).
func TestWholeWorldDataRemovesEverything(t *testing.T) {
	cfg, r := testCfg(t, true)

	report, err := Run(r, cfg, ScopeWholeWorld, true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(r.pools) != 1 || r.pools[0] != "pve/fh-thin" {
		t.Errorf("--data must remove the created pool exactly once, got %v", r.pools)
	}
	var sawSed bool
	for _, cmd := range r.execs {
		if strings.Contains(cmd, "sed -i") && strings.Contains(cmd, "authorized_keys") {
			sawSed = true
		}
	}
	if !sawSed {
		t.Errorf("--data must remove the door key: %v", r.execs)
	}
	if _, err := os.Stat(cfg.WorldHome); !os.IsNotExist(err) {
		t.Error("--data must remove the world home")
	}
	if _, err := os.Stat(cfg.ConfigPath); !os.IsNotExist(err) {
		t.Error("--data must remove the config")
	}
	if !strings.Contains(report, "removed freehold-created thin pool") {
		t.Errorf("report must mention the pool removal:\n%s", report)
	}
	if strings.Contains(report, "config KEPT") {
		t.Errorf("report must not claim the config was kept:\n%s", report)
	}
}

// A whole-world --data on a world whose pool was NOT created by freehold
// (thin_pool absent from the config) must not remove any pool — the guard
// against wiping a reused stock pool.
func TestWholeWorldDataSkipsPoolWhenNotCreated(t *testing.T) {
	cfg, r := testCfg(t, true)
	cfg.ThinPool = ""

	if _, err := Run(r, cfg, ScopeWholeWorld, true); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(r.pools) != 0 {
		t.Errorf("no created pool recorded => no pool removal, got %v", r.pools)
	}
}

// Tenant scopes keep the config + door regardless of --data (they never
// owned them).
func TestTenantScopeKeepsConfig(t *testing.T) {
	for _, scope := range []Scope{ScopeTenantCompute, ScopeTenantData} {
		cfg, r := testCfg(t, true)
		cfg.TenantRole = "relay"
		_, err := Run(r, cfg, scope, true)
		if err != nil {
			t.Fatalf("Run scope=%v: %v", scope, err)
		}
		if len(r.pools) != 0 {
			t.Errorf("tenant scope %v must not remove pools: %v", scope, r.pools)
		}
		if _, err := os.Stat(cfg.ConfigPath); err != nil {
			t.Errorf("tenant scope %v must keep the config", scope)
		}
		if len(r.lxcRoles) != 1 || r.lxcRoles[0] != "relay" {
			t.Errorf("tenant scope %v must destroy only the relay LXC, got %v", scope, r.lxcRoles)
		}
	}
}

func TestRunRequiresConfirm(t *testing.T) {
	cfg, r := testCfg(t, false)
	if _, err := Run(r, cfg, ScopeWholeWorld, false); err == nil {
		t.Error("unconfirmed teardown must abort")
	}
	if len(r.execs) != 0 {
		t.Error("unconfirmed teardown must not run anything")
	}
}

// Real-world paths: WorldHome/ConfigPath may be genuinely absent (a wiped
// box) — the FULL teardown must treat missing as already-done, not fail.
func TestWholeWorldDataToleratesAbsentFiles(t *testing.T) {
	cfg, r := testCfg(t, true)
	cfg.WorldHome = filepath.Join(t.TempDir(), "no-home")
	cfg.ConfigPath = filepath.Join(t.TempDir(), "no-config.toml")

	if _, err := Run(r, cfg, ScopeWholeWorld, true); err != nil {
		t.Fatalf("missing home/config must not fail the full teardown: %v", err)
	}
}

// TestDestroyingAnnouncedLive pins the teardown's streaming contract (the
// TUI's checkbox feed): EVERY destroy is announced the moment it STARTS
// ("destroying relay LXC 100") — the operator never stares at a silent
// screen for the ~30s a pct destroy takes — and the finished line
// ("destroyed relay LXC 100") lands after it, both in the Live stream AND
// the final report.
func TestDestroyingAnnouncedLive(t *testing.T) {
	cfg, r := testCfg(t, false)
	var live []string
	cfg.Live = func(line string) { live = append(live, line) }
	three := uint32(102)
	cfg.Managed = append(cfg.Managed, "k3s")
	cfg.Vmid["k3s"] = &three

	report, err := Run(r, cfg, ScopeWholeWorld, true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, role := range []string{"relay", "cp", "k3s"} {
		if !strings.Contains(report, fmt.Sprintf("destroying %s LXC", role)) {
			t.Errorf("report must announce %s BEFORE the destroy:\n%s", role, report)
		}
		if !strings.Contains(report, fmt.Sprintf("destroyed %s LXC", role)) {
			t.Errorf("report must record the finished %s destroy:\n%s", role, report)
		}
	}
	// the Live stream must see the start line BEFORE the finish line (that
	// ordering is the whole point — the TUI flips pending→running→✓ on it).
	idx := func(needle string) int {
		for i, l := range live {
			if strings.Contains(l, needle) {
				return i
			}
		}
		return -1
	}
	if idx("destroying relay") < 0 || idx("destroyed relay") < 0 || idx("destroying relay") > idx("destroyed relay") {
		t.Errorf("Live must stream 'destroying relay' before 'destroyed relay': %v", live)
	}
}

// A managed LXC with NO recorded vmid was never created: no "destroying"
// announcement (there is nothing to destroy), the noop line still lands.
func TestNoDestroyingLineWithoutVmid(t *testing.T) {
	cfg, r := testCfg(t, false)
	delete(cfg.Vmid, "cp")

	report, err := Run(r, cfg, ScopeWholeWorld, true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(report, "destroying cp") {
		t.Errorf("no vmid => never announce a cp destroy:\n%s", report)
	}
	if !strings.Contains(report, "never created") {
		t.Errorf("report must say the cp LXC was never created:\n%s", report)
	}
}

// pctList models real `pct list` output (VMID STATUS LOCK NAME): each row
// starts at column 0 or with whitespace, the name is the LAST column — the
// shape lxcOccupant's `awk $NF` parses.
type pctHost struct {
	execs []string
	names map[uint32]string // vmid -> guest name
}

func (p *pctHost) execFn(cmd string) (bool, string) {
	p.execs = append(p.execs, cmd)
	switch {
	case strings.Contains(cmd, "pct list") && strings.Contains(cmd, "awk"):
		// occupant lookup: "pct list | grep -E '^\s*N ' | awk ..."
		for vmid, name := range p.names {
			if strings.Contains(cmd, fmt.Sprintf("\\s*%d ", vmid)) {
				return true, name + "\n"
			}
		}
		return true, "\n"
	case strings.Contains(cmd, "pct list") && strings.Contains(cmd, "grep -c"):
		for vmid := range p.names {
			if strings.Contains(cmd, fmt.Sprintf("\\s*%d ", vmid)) {
				return true, "1\n"
			}
		}
		return true, "0\n"
	case strings.HasPrefix(cmd, "pct destroy "):
		var v uint32
		_, _ = fmt.Sscanf(strings.TrimPrefix(cmd, "pct destroy "), "%d", &v)
		delete(p.names, v)
		return true, ""
	case strings.HasPrefix(cmd, "pct status "):
		return true, "status: stopped\n"
	}
	return true, ""
}

// TestExecRunnerRefusesForeignGuestAtRecordedVmid is the vmid-reuse guard:
// the config kept vmid 100 after a compute teardown, PVE handed the freed
// id to the operator's next guest, and a second teardown must REFUSE — no
// pct stop, no pct destroy.
func TestExecRunnerRefusesForeignGuestAtRecordedVmid(t *testing.T) {
	host := &pctHost{names: map[uint32]string{100: "operators-new-ct"}}
	r := &ExecRunner{Domain: "world.test", execFn: host.execFn}
	_, err := r.DestroyOneLxc("relay", ptr32(100))
	if err == nil {
		t.Fatal("destroy must refuse when the recorded vmid holds a foreign guest")
	}
	for _, c := range []string{"operators-new-ct", "world-test-relay"} {
		if !strings.Contains(err.Error(), c) {
			t.Errorf("refusal must name both the occupant %q and the expected %q, got: %v", "operators-new-ct", "world-test-relay", err)
		}
	}
	for _, cmd := range host.execs {
		if strings.HasPrefix(cmd, "pct destroy") || strings.HasPrefix(cmd, "pct stop") {
			t.Errorf("no destroy/stop may run on a foreign occupant, got: %q", cmd)
		}
	}
}

// TestExecRunnerDestroysMatchingGuest: the occupant at the recorded vmid IS
// the freehold guest (same name rebuild creates) — the full destroy path
// runs and reports it.
func TestExecRunnerDestroysMatchingGuest(t *testing.T) {
	host := &pctHost{names: map[uint32]string{100: "world-test-relay"}}
	r := &ExecRunner{Domain: "world.test", execFn: host.execFn}
	log, err := r.DestroyOneLxc("relay", ptr32(100))
	if err != nil {
		t.Fatalf("matching occupant must destroy, got: %v", err)
	}
	if len(log) != 1 || !strings.Contains(log[0], "destroyed relay LXC 100") {
		t.Errorf("log = %v", log)
	}
	var destroyed bool
	for _, cmd := range host.execs {
		if cmd == "pct destroy 100" {
			destroyed = true
		}
	}
	if !destroyed {
		t.Errorf("pct destroy 100 never ran: %v", host.execs)
	}
}

// TestExecRunnerAbsentVmidIsNoop: a free vmid = "already gone", and nothing
// destructive runs.
func TestExecRunnerAbsentVmidIsNoop(t *testing.T) {
	host := &pctHost{names: map[uint32]string{}}
	r := &ExecRunner{Domain: "world.test", execFn: host.execFn}
	log, err := r.DestroyOneLxc("cp", ptr32(101))
	if err != nil {
		t.Fatalf("absent vmid is a no-op, got: %v", err)
	}
	if len(log) != 1 || !strings.Contains(log[0], "already gone") {
		t.Errorf("log = %v", log)
	}
	for _, cmd := range host.execs {
		if strings.HasPrefix(cmd, "pct destroy") || strings.HasPrefix(cmd, "pct stop") {
			t.Errorf("nothing destructive may run on an absent vmid, got: %q", cmd)
		}
	}
}

// TestExecRunnerRefusesWithoutDomain: no domain recorded = no expected name
// to verify against = fail closed (never destroy by vmid alone).
func TestExecRunnerRefusesWithoutDomain(t *testing.T) {
	host := &pctHost{names: map[uint32]string{100: "world-test-relay"}}
	r := &ExecRunner{execFn: host.execFn}
	if _, err := r.DestroyOneLxc("relay", ptr32(100)); err == nil {
		t.Fatal("no domain must refuse — the occupant cannot be verified")
	}
	for _, cmd := range host.execs {
		if strings.HasPrefix(cmd, "pct destroy") {
			t.Errorf("no destroy may run without a domain guard, got: %q", cmd)
		}
	}
}
