package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectTag(t *testing.T) {
	tags := []string{"v0.5.23", "v0.7.0", "v0.7.1-rc.2", "v0.7.1-rc.10", "v0.6.0", "v0.7.0-4-gabc"}
	if got, ok := SelectTag("stable", tags); !ok || got != "v0.7.0" {
		t.Errorf("stable = %q,%v want v0.7.0", got, ok)
	}
	if _, ok := SelectTag("rc", tags); ok {
		t.Error("the rc channel is gone; it must select nothing")
	}
	if _, ok := SelectTag("stable", []string{"v0.7.1-rc.1"}); ok {
		t.Error("an rc must not satisfy stable")
	}
}

func TestVerifyChecksums(t *testing.T) {
	dir := t.TempDir()
	data := []byte("binary")
	if err := os.WriteFile(filepath.Join(dir, "runner"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"),
		[]byte(hex.EncodeToString(sum[:])+"  runner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksums(dir); err != nil {
		t.Fatalf("valid checksums rejected: %v", err)
	}
	// Corrupt the binary -> mismatch must fail.
	if err := os.WriteFile(filepath.Join(dir, "runner"), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksums(dir); err == nil {
		t.Fatal("tampered binary must fail checksum verification")
	}
}
