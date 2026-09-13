package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProfiles exercises the filesystem profile registry + the Current seam.
func TestProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FREEHOLD_HOME", t.TempDir())

	if got := List(); len(got) != 0 {
		t.Fatalf("empty box has %d profiles, want 0", len(got))
	}
	if Resolve("demo") != nil {
		t.Fatal("no profile => Resolve must return nil")
	}

	// A named profile lives at its own config + scoped state dir.
	name := "demo-prod"
	np := &Profile{Name: name, ConfigPath: NewProfilePath(name), StateDir: NewProfileState(name)}
	if err := (&Config{CPURL: "https://cp.demo.example"}).Save(np.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if p := Resolve(name); p == nil {
		t.Fatal("named profile not listed")
	} else if p.ConfigPath != np.ConfigPath || p.StateDir != np.StateDir {
		t.Fatalf("named profile scoped wrong: %+v", p)
	}
	SetCurrent(np)
	if ConfigPath() != np.ConfigPath || StateDir() != np.StateDir {
		t.Fatal("Current must scope config + state to the named profile")
	}
	if _, err := os.Stat(filepath.Join(np.StateDir, "x")); !os.IsNotExist(err) {
		t.Fatal("named profile state dir must be scoped, not the freehold root")
	}

	// Names: "default" is an ordinary slug now (no reserved/legacy special
	// case); reject blanks and unsafe chars.
	for _, good := range []string{"default", "a", "demo-prod", "tenant_2", "A"} {
		if !ValidProfileName(good) {
			t.Errorf("ValidProfileName(%q) = false, want true", good)
		}
	}
	for _, bad := range []string{"", "has space", "-lead", "_lead", "end-", "cap!"} {
		if ValidProfileName(bad) {
			t.Errorf("ValidProfileName(%q) = true, want false", bad)
		}
	}
}
