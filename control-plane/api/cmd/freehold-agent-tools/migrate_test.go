package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/control-plane/api/agenttools"
)

// TestMigrateImportConsoleAgentsAdditiveOnly proves migration 002 folds the
// console state.json agents into the registry ADDITIVELY: an existing registry
// row for the same name keeps its current pubkey (the registry is
// authoritative) and only absent names are imported.
func TestMigrateImportConsoleAgentsAdditiveOnly(t *testing.T) {
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

	spec := &deploySpec{stateDir: stateDir}
	m := buildMigrator(spec, consoleDir)
	if _, err := m(); err != nil {
		t.Fatalf("migrate: %v", err)
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
	// The ledger records the migration as done (verify passed).
	ledger, err := os.ReadFile(filepath.Join(stateDir, "migrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ledger), `"done"`) {
		t.Fatalf("migration not recorded done: %s", ledger)
	}
}