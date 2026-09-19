package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/config"
)

// writeProfile registers a named profile with the given config body.
func writeProfile(t *testing.T, name, body string) {
	t.Helper()
	p := config.NewProfilePath(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSelectProfileCreatesAndReAdopts covers the life-cycle-aware profile
// selection: a bad name is rejected, a new name pins profiles/<name>/, and an
// already-registered name RE-ADOPTS it (the gate, not selectProfile, refuses a
// live CP).
func TestSelectProfileCreatesAndReAdopts(t *testing.T) {
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

	writeProfile(t, "demo", "relay_url = 'https://relay.example'\n")
	config.SetCurrent(nil)
	if err := selectProfile("demo"); err != nil {
		t.Fatalf("existing profile must re-adopt, got %v", err)
	}
	if p := config.Current(); p == nil || p.Name != "demo" {
		t.Fatalf("re-adopt must pin the existing profile, got %+v", p)
	}
}

// TestGateInstallMatrix is the PR1 gate: no profile mints; an existing profile
// whose recorded CP answers /healthz FAILS (a live world means build/teardown/
// uninstall/login); an existing profile whose CP is absent re-adopts.
func TestGateInstallMatrix(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	config.SetCurrent(nil)

	if a, err := gateInstall("demo"); err != nil || a != lifecycleMint {
		t.Fatalf("no profile must mint: a=%v err=%v", a, err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	writeProfile(t, "demo", "cp_url = '"+srv.URL+"'\nrelay_url = 'https://relay.example'\n")
	if _, err := gateInstall("demo"); err == nil || !strings.Contains(err.Error(), "live control plane") {
		t.Fatalf("a live CP must fail the gate, got %v", err)
	}

	// CP absent: no recorded URL is not-live, and a dead URL is not-live.
	writeProfile(t, "demo", "relay_url = 'https://relay.example'\n")
	if a, err := gateInstall("demo"); err != nil || a != lifecycleReAdopt {
		t.Fatalf("a profile with no live CP must re-adopt: a=%v err=%v", a, err)
	}
	writeProfile(t, "demo", "cp_url = 'http://127.0.0.1:1'\n")
	if a, err := gateInstall("demo"); err != nil || a != lifecycleReAdopt {
		t.Fatalf("a dead CP must re-adopt: a=%v err=%v", a, err)
	}
}
