package cpbuild

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"freehold/control-plane/api/agenttools"
	"freehold/platform/migrations"
)

// TestImportConsoleAgentsAdditiveOnly proves the 002 migration logic folds the
// console state.json agents into the registry ADDITIVELY: an existing registry
// row for the same name keeps its current pubkey (the registry is
// authoritative) and only absent names are imported. The migration SCRIPT drives
// this same Go function (registry import-console); this test pins the logic.
func TestImportConsoleAgentsAdditiveOnly(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	consoleDir := filepath.Join(dir, "console-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(consoleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The registry already holds a CURRENT pubkey for "cpa".
	regPath := filepath.Join(stateDir, "registry.json")
	reg, err := agenttools.OpenRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RegisterAgent("cpa", "CURRENT_PK", "cpa"); err != nil {
		t.Fatal(err)
	}
	// The console's stale state.json carries a DIFFERENT (stale) pubkey for the
	// same name + a new agent that must be imported.
	console := map[string]interface{}{
		"agents": map[string]interface{}{
			"cpa": map[string]interface{}{"pubkey": "STALE_PK", "created_at": 1},
			"bob": map[string]interface{}{"pubkey": "BOB_PK", "created_at": 2},
		},
	}
	raw, _ := json.Marshal(console)
	if err := os.WriteFile(filepath.Join(consoleDir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := agenttools.ImportConsoleAgents(reg, consoleDir); err != nil {
		t.Fatalf("import: %v", err)
	}

	rows, err := agenttools.OpenRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rows.Agents()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, r := range got {
		byName[r.Name] = r.Pubkey
	}
	// cpa keeps the CURRENT registry pubkey (not clobbered by the stale one).
	if got := byName["cpa"]; got != "CURRENT_PK" {
		t.Fatalf("existing registry row clobbered by console state: cpa pubkey = %q", got)
	}
	// bob (absent from the registry) is imported.
	if got := byName["bob"]; got != "BOB_PK" {
		t.Fatalf("absent console agent not imported: bob pubkey = %q", got)
	}
}

// TestBuildMigratorRunsScriptsToConvergence exercises the PRODUCTION wiring of
// BuildMigrator: scripts shipped into <consoleStateDir>/migrations/scripts/<epoch>.sh
// run via `bash -euo pipefail` with FREEHOLD_AGENT_TOOLS / REGISTRY /
// CONSOLE_STATE / STATE_DIR env populated. A stub `freehold-agent-tools` asserts
// those env vars (proving cpGuestDirs + path resolution) and exits 0, so both
// migrations run, their markers are written, and a repeat run is a no-op.
func TestBuildMigratorRunsScriptsToConvergence(t *testing.T) {
	dir := t.TempDir()
	// cpGuestDirs derives binDir from filepath.Dir(StateDir), so the stub under
	// test must sit at <dir>/bin/freehold-agent-tools.
	stateDir := filepath.Join(dir, "state")
	consoleDir := filepath.Join(dir, "console-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(consoleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(binDir, "freehold-agent-tools")
	stubSrc := `#!/bin/sh
set -eu
[ -n "${FREEHOLD_AGENT_TOOLS:-}" ] || { echo "no FREEHOLD_AGENT_TOOLS"; exit 1; }
[ -n "${REGISTRY:-}" ] || { echo "no REGISTRY"; exit 1; }
[ -n "${CONSOLE_STATE:-}" ] || { echo "no CONSOLE_STATE"; exit 1; }
[ -n "${STATE_DIR:-}" ] || { echo "no STATE_DIR"; exit 1; }
exit 0
`
	if err := os.WriteFile(stub, []byte(stubSrc), 0o755); err != nil {
		t.Fatal(err)
	}
	// Ship two scripts into the CONSOLE's scripts dir — where ShipMigrations
	// puts them (install/update copy these from the release asset / tree).
	root := filepath.Join(consoleDir, "migrations")
	scriptsDir := migrations.ScriptsRoot(root)
	if err := os.MkdirAll(scriptsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"1799900001.sh", "1799900002.sh"} {
		if err := os.WriteFile(filepath.Join(scriptsDir, n), []byte("echo hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	spec := &Spec{StateDir: stateDir}
	m := BuildMigrator(spec, consoleDir)
	results, err := m()
	if err != nil {
		t.Fatalf("BuildMigrator: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected both migrations to run, got %d: %+v", len(results), results)
	}
	for _, r := range results {
		if !r.OK {
			t.Errorf("migration %q failed: %s", r.Name, r.Err)
		}
	}

	// Both completion markers now exist in that same root; a second run has
	// nothing pending.
	for _, n := range []string{"1799900001.sh", "1799900002.sh"} {
		if !migrations.Done(root, n) {
			t.Errorf("marker missing for %s", n)
		}
	}
	if again, err := m(); err != nil || len(again) != 0 {
		t.Fatalf("second run must be a no-op, got %v / %v", again, err)
	}
}

// TestBuildMigratorReadsConsoleScriptsRoot pins the PRODUCTION layout, where the
// migrator's scripts root and its registry/STATE_DIR base are DIFFERENT dirs:
// install/update ship scripts to the CONSOLE's state dir (`ShipMigrations` uses
// `DeployCpSpec.StateDir` = `<cpRoot>/control-plane`), and the console's
// `/api/world` pending count reads markers from that same dir; but the serve
// context that calls BuildMigrator passes its OWN `--state-dir` (the sibling
// `<cpRoot>/agent-tools`, where registry.json + facts.json live) as
// `spec.StateDir`. Reading the scripts from `spec.StateDir` sees an empty queue
// and returns a silent no-op — the bug that left the librem world permanently at
// "2 pending migrations". Scripts therefore come from `consoleStateDir`, while
// REGISTRY / STATE_DIR stay on `spec.StateDir` (the authoritative store).
func TestBuildMigratorReadsConsoleScriptsRoot(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent-tools")
	consoleDir := filepath.Join(dir, "control-plane")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(consoleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// cpGuestDirs derives binDir from filepath.Dir(StateDir), so the stub the
	// scripts resolve via FREEHOLD_AGENT_TOOLS must sit at <dir>/bin.
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(binDir, "freehold-agent-tools")
	stubSrc := `#!/bin/sh
set -eu
[ -n "${FREEHOLD_AGENT_TOOLS:-}" ] || { echo "no FREEHOLD_AGENT_TOOLS"; exit 1; }
[ -n "${REGISTRY:-}" ] || { echo "no REGISTRY"; exit 1; }
[ -n "${CONSOLE_STATE:-}" ] || { echo "no CONSOLE_STATE"; exit 1; }
[ -n "${STATE_DIR:-}" ] || { echo "no STATE_DIR"; exit 1; }
exit 0
`
	if err := os.WriteFile(stub, []byte(stubSrc), 0o755); err != nil {
		t.Fatal(err)
	}
	// The authoritative registry lives with the agent-tools state, where the
	// server itself opens it — REGISTRY must point there, not at the console dir.
	if err := os.WriteFile(filepath.Join(agentDir, "registry.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Ship the two registry scripts ONLY into the console root — nothing under
	// agentDir/migrations exists, so a migrator reading spec.StateDir finds zero.
	root := filepath.Join(consoleDir, "migrations")
	scriptsDir := migrations.ScriptsRoot(root)
	if err := os.MkdirAll(scriptsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	names := []string{"1799900000.sh", "1799910000.sh"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(scriptsDir, n), []byte("echo hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	spec := &Spec{StateDir: agentDir}
	m := BuildMigrator(spec, consoleDir)
	results, err := m()
	if err != nil {
		t.Fatalf("BuildMigrator: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected both migrations to run, got %d: %+v", len(results), results)
	}
	for _, r := range results {
		if !r.OK {
			t.Errorf("migration %q failed: %s", r.Name, r.Err)
		}
	}

	// The runner and the console's reporter now share one root: markers exist
	// where /api/world reads them, so its pending count drops to 0.
	for _, n := range names {
		if !migrations.Done(root, n) {
			t.Errorf("marker missing for %s at %s", n, filepath.Join(root, n))
		}
	}
	if n, err := migrations.PendingCount(root); err != nil {
		t.Fatalf("PendingCount: %v", err)
	} else if n != 0 {
		t.Errorf("console reporter still sees %d pending after a converged run", n)
	}
}

// TestBuildMigratorExportsChannelEnv pins the env the channel migration script
// needs: the agent-tools binary path plus the relay coords + CPA name a kind-9002
// channel edit signs with. A stub `freehold-agent-tools` asserts every var is set
// and non-empty, so a dropped/renamed env fails the run.
func TestBuildMigratorExportsChannelEnv(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "agent-tools")
	consoleDir := filepath.Join(dir, "control-plane")
	binDir := filepath.Join(dir, "bin")
	for _, d := range []string{stateDir, consoleDir, binDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stub := filepath.Join(binDir, "freehold-agent-tools")
	stubSrc := `#!/bin/sh
set -eu
for v in FREEHOLD_AGENT_TOOLS REGISTRY CONSOLE_STATE STATE_DIR \
         FREEHOLD_RELAY_URL FREEHOLD_RELAY_AUTH_URL FREEHOLD_CPA_NAME; do
  eval "val=\${$v:-}"
  [ -n "$val" ] || { echo "env $v is empty"; exit 1; }
done
exit 0
`
	if err := os.WriteFile(stub, []byte(stubSrc), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(consoleDir, "migrations")
	scriptsDir := migrations.ScriptsRoot(root)
	if err := os.MkdirAll(scriptsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The script actually invokes the stub, so its env assertions execute.
	if err := os.WriteFile(filepath.Join(scriptsDir, "1799920000.sh"), []byte(`"$FREEHOLD_AGENT_TOOLS" channel edit`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := &Spec{StateDir: stateDir, RelayURL: "https://relay.example", CpaName: "freehold"}
	results, err := BuildMigrator(spec, consoleDir)()
	if err != nil {
		t.Fatalf("BuildMigrator: %v", err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("expected the channel migration to run OK, got %+v", results)
	}
}
