package state

import (
	"os"
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

// TestValidOperatorTZ: empty (unset) is valid; real IANA names are valid;
// Go's "Local" special case is refused (no pod resolves it — it would fake a
// setting while reading as UTC); garbage is invalid.
func TestValidOperatorTZ(t *testing.T) {
	for tz, want := range map[string]bool{
		"":                true,
		"America/Chicago": true,
		"Europe/Berlin":   true,
		"UTC":             true,
		"Local":           false,
		"Mars/Olympus":    false,
	} {
		if got := ValidOperatorTZ(tz); got != want {
			t.Errorf("ValidOperatorTZ(%q) = %v, want %v", tz, got, want)
		}
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

// TestOpenAssertsTraverseOnlyStateDir: the CP state root must stay 0711
// (traverse-only) across every Open. Sandboxed capability doors run as an
// unprivileged user and re-open their audit log + identity through this
// ancestor per exec; a 0700 assertion would strip traversal out from under
// a healthy (never-restarted) door — the audit stream would die silently.
// Contents stay private by their own modes.
func TestOpenAssertsTraverseOnlyStateDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatalf("open: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o711 {
		t.Fatalf("state dir mode = %04o, want 0711 (traverse-only)", got)
	}
	// A door's own package dir stays 0700: traversal into the state root
	// must never expose a sibling door's identity.
	pkg := filepath.Join(dir, "runner", "some-door")
	if err := os.MkdirAll(pkg, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatalf("re-open: %v", err)
	}
	pi, err := os.Stat(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got := pi.Mode().Perm(); got != 0o700 {
		t.Fatalf("package dir mode = %04o, want 0700", got)
	}
}
