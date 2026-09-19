package freeholdcli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func cliRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(b), "module freehold/freehold-cli") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the freehold/freehold-cli module root")
		}
		dir = parent
	}
}

// TestCLIDoesNotImportControlPlane is the other half of the local/server split
// guard: the local operator CLI must never link the server. The reciprocal
// guard lives in control-plane (control-plane/isolation_test.go).
func TestCLIDoesNotImportControlPlane(t *testing.T) {
	root := cliRoot(t)
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		p := strings.TrimSpace(line)
		if p == "freehold/control-plane" || strings.HasPrefix(p, "freehold/control-plane/") {
			t.Errorf("freehold-cli imports %s — the local CLI must drive the server, never link it", p)
		}
	}
}
