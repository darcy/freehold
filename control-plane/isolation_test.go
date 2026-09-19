package controlplane_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// controlPlaneRoot walks up from the test's working dir to the module root.
func controlPlaneRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(b), "module freehold/control-plane") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the freehold/control-plane module root")
		}
		dir = parent
	}
}

// TestControlPlaneDoesNotImportCLI is one half of the local/server split guard:
// the server must never link the local operator CLI. The reciprocal guard lives
// in the freehold-cli module (freehold-cli/isolation_test.go).
func TestControlPlaneDoesNotImportCLI(t *testing.T) {
	root := controlPlaneRoot(t)
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		p := strings.TrimSpace(line)
		if p == "freehold/freehold-cli" || strings.HasPrefix(p, "freehold/freehold-cli/") {
			t.Errorf("control-plane imports %s — the server must not link the local CLI", p)
		}
	}
}
