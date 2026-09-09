package wire

import (
	"strings"
	"testing"
)

// TestSealMemoryRejectsOversized proves the write path ERRORS on a value that
// would overflow the NIP-44 16-bit length prefix — never silently writes a
// payload no reader could decrypt (IMPORTANT-5).
func TestSealMemoryRejectsOversized(t *testing.T) {
	var secret [32]byte
	for i := range secret {
		secret[i] = 7
	}
	sealed, err := SealMemory(secret[:], "ok")
	if err != nil || sealed == "" {
		t.Fatalf("small value should seal: err=%v", err)
	}
	big := strings.Repeat("x", nip44MaxPlaintext+1)
	if _, err := SealMemory(secret[:], big); err == nil {
		t.Fatal("expected error for value larger than the NIP-44 max plaintext")
	}
	// A value exactly at the cap seals (no truncation).
	atCap := strings.Repeat("y", nip44MaxPlaintext)
	if _, err := SealMemory(secret[:], atCap); err != nil {
		t.Fatalf("value at the cap should seal: %v", err)
	}
}
