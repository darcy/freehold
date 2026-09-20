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
// BuildMigrator: scripts shipped into <StateDir>/migrations/scripts/<epoch>.sh
// run via `bash -euo pipefail` with FREEHOLD_AGENT_TOOLS / REGISTRY /
// CONSOLE_STATE / STATE_DIR env populated. A stub `freehold-agent-tools` asserts
// those env vars (proving cpGuestDirs + path resolution) and exits 0, so both
// migrations run and their markers are written.
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
	// Ship two scripts into the CP's scripts dir (install/update would copy
	// these from the release asset / tree).
	scriptsDir := migrations.ScriptsRoot(filepath.Join(stateDir, "migrations"))
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

	// Both completion markers now exist; a second run has nothing pending.
	root := filepath.Join(stateDir, "migrations")
	for _, n := range []string{"1799900001.sh", "1799900002.sh"} {
		if !migrations.Done(root, n) {
			t.Errorf("marker missing for %s", n)
		}
	}
	if again, err := m(); err != nil || len(again) != 0 {
		t.Fatalf("second run must be a no-op, got %v / %v", again, err)
	}
}
