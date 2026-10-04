package box

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVerbSSHKeyGenerateOnce pins the cp-verb key's lifecycle: first call
// generates a 0600 private PEM beside the profile's config.toml; the second
// call reuses it (same public line — the authorize on the host stays
// idempotent because the key never rotates behind the deploy's back).
func TestVerbSSHKeyGenerateOnce(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "librem", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}

	priv, pub, err := VerbSSHKey(cfgPath, "librem")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if priv != filepath.Join(dir, "librem", "cp-verb-key") {
		t.Fatalf("priv path = %s", priv)
	}
	if !pubLineOk(pub) {
		t.Fatalf("public line malformed: %q", pub)
	}
	info, err := os.Stat(priv)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %v, want 0600", info.Mode().Perm())
	}

	priv2, pub2, err := VerbSSHKey(cfgPath, "librem")
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if priv2 != priv || pub2 != pub {
		t.Fatalf("reuse drifted: %s/%s vs %s/%s", priv2, pub2, priv, pub)
	}
}

func pubLineOk(line string) bool {
	return len(line) > 8 && line[:4] == "ssh-"
}
