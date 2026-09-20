package console

import (
	"encoding/hex"
	"path/filepath"
	"testing"

	"freehold/platform/provisioning/box"
)

// TestConsoleEncPubkey: the helper derives the X25519 pubkey from a REAL
// console identity written by box.MintIdentity (the same writer the deployment
// uses), and returns "" when the identity is absent. Using the real writer
// means the field name/format is exercised, not assumed.
func TestConsoleEncPubkey(t *testing.T) {
	dir := t.TempDir()
	s := &Server{StateDir: dir}
	if got := s.consoleEncPubkey(); got != "" {
		t.Fatalf("no identity => empty, got %q", got)
	}
	if err := box.MintIdentity(filepath.Join(dir, "console")); err != nil {
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
