package console

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestConsoleEncPubkey: the helper derives the X25519 pubkey from the console
// identity's enc secret and returns "" when the identity is absent.
func TestConsoleEncPubkey(t *testing.T) {
	dir := t.TempDir()
	s := &Server{StateDir: dir}
	if got := s.consoleEncPubkey(); got != "" {
		t.Fatalf("no identity => empty, got %q", got)
	}
	if err := os.MkdirAll(filepath.Join(dir, "console"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	doc := `{"enc_secret_hex":"` + hex.EncodeToString(secret) + `"}`
	if err := os.WriteFile(filepath.Join(dir, "console", "identity.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	got := s.consoleEncPubkey()
	if len(got) != 64 {
		t.Fatalf("consoleEncPubkey = %q, want 64-hex", got)
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("consoleEncPubkey not hex: %v", err)
	}
}
