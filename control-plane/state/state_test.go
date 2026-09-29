package state

import (
	"path/filepath"
	"testing"
)

// TestSettingsRoundTrip: SetSettings persists, Open + LoadReadOnly read it
// back, and a state file written WITHOUT the settings group reads as zero
// values (nil Settings) — old worlds unmarshal cleanly, no migration.
func TestSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.SetSettings(&Settings{OperatorTZ: "America/Chicago"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if s := reopened.Settings(); s == nil || s.OperatorTZ != "America/Chicago" {
		t.Fatalf("reopened settings = %+v, want operator_tz America/Chicago", s)
	}

	ro, err := LoadReadOnly(dir)
	if err != nil {
		t.Fatalf("load read-only: %v", err)
	}
	if ro.Settings == nil || ro.Settings.OperatorTZ != "America/Chicago" {
		t.Fatalf("read-only settings = %+v, want operator_tz America/Chicago", ro.Settings)
	}
	if _, err := LoadReadOnly(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("LoadReadOnly on a missing dir must error")
	}
}

// TestSettingsNilReadsZero: a pre-settings state file (no settings key) must
// read as nil/zero — the pods-run-UTC default — never an error.
func TestSettingsNilReadsZero(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	ro, err := LoadReadOnly(dir)
	if err != nil {
		t.Fatalf("load read-only: %v", err)
	}
	if ro.Settings != nil && ro.Settings.OperatorTZ != "" {
		t.Fatalf("pre-settings state read %+v, want nil/empty", ro.Settings)
	}
}
