package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/config"
)

// TestSelectProfileCreatesAndRefusesDuplicate covers the fail-closed profile
// gate: a bad name is rejected, a new name pins the process to
// profiles/<name>/, and an already-registered name is refused.
func TestSelectProfileCreatesAndRefusesDuplicate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	config.SetCurrent(nil)

	if err := selectProfile("bad name"); err == nil {
		t.Error("invalid profile name must be rejected")
	}
	if err := selectProfile("demo"); err != nil {
		t.Fatalf("new profile must be accepted: %v", err)
	}
	p := config.Current()
	if p == nil || p.Name != "demo" || p.ConfigPath != config.NewProfilePath("demo") || p.StateDir != config.NewProfileState("demo") {
		t.Fatalf("profile not pinned: %+v", p)
	}

	// Registering the config file makes the name an existing profile.
	if err := os.MkdirAll(filepath.Dir(p.ConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigPath, []byte("relay_url = 'https://relay.example'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config.SetCurrent(nil)
	if err := selectProfile("demo"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate profile must refuse, got %v", err)
	}
}

// TestStateRootForDerivesProfileRoot proves a self-staged child recovers the
// profile state root from the agent-dir it is handed, and falls back to the
// default root for a non-conventional dir.
func TestStateRootForDerivesProfileRoot(t *testing.T) {
	root := filepath.Join("/srv", "data", "profiles", "librem2")
	if got := stateRootFor(filepath.Join(root, "control-plane", "agent-ops")); got != root {
		t.Errorf("stateRootFor = %q, want %q", got, root)
	}
	t.Setenv("FREEHOLD_HOME", "/tmp/fh-state-root")
	if got := stateRootFor("/custom/identity"); got != "/tmp/fh-state-root" {
		t.Errorf("non-conventional agent-dir must fall back to the default root, got %q", got)
	}
}
