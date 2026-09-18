package provisioning

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// providerTokens are the substrate command/string families that must never
// appear in platform/. "provider" itself is deliberately excluded — the DNS
// providers would false-positive.
var providerTokens = []string{"pct ", "pvesm", "vzdump", "qm ", "/etc/pve", "zfs ", "zpool ", "lvs "}

// platformRoot walks up from the test's working dir to the module root.
func platformRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(b), "module freehold/platform") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the freehold/platform module root")
		}
		dir = parent
	}
}

// TestPlatformDoesNotImportProviders is the boundary guard: the platform
// module must stay provider-independent, so no package under it may import
// freehold/providers.
func TestPlatformDoesNotImportProviders(t *testing.T) {
	root := platformRoot(t)
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "freehold/providers" ||
			strings.HasPrefix(strings.TrimSpace(line), "freehold/providers/") {
			t.Errorf("platform imports %s — providers must be injected, not imported", line)
		}
	}
}

// TestPlatformHasNoProviderStrings greps non-test platform sources for
// substrate command tokens (comments included).
func TestPlatformHasNoProviderStrings(t *testing.T) {
	root := platformRoot(t)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, tok := range providerTokens {
			if strings.Contains(string(b), tok) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s contains provider token %q", rel, tok)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
