package oplogin

import (
	"encoding/hex"
	"testing"
)

// TestOperatorLedgerRoundTrip verifies the operator's nsec persists to the
// operator dir (isolated via FREEHOLD_HOME) and that first-run-wins holds.
func TestOperatorLedgerRoundTrip(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())

	secret := [32]byte{}
	secret[0] = 7
	wrote, err := Save(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("first save should write")
	}
	got, err := SecretHex()
	if err != nil {
		t.Fatal(err)
	}
	if want := hex.EncodeToString(secret[:]); got != want {
		t.Fatalf("secret mismatch: want %s got %s", want, got)
	}
	// First-run-wins: a second Save must not clobber.
	other := [32]byte{}
	other[0] = 9
	if wrote, _ = Save(other); wrote {
		t.Fatal("second save should be refused (first-run-wins)")
	}
	if got, _ = SecretHex(); got != hex.EncodeToString(secret[:]) {
		t.Fatal("operator identity was clobbered")
	}
}

// TestNsecToSecret accepts both nsec1 bech32 and bare hex.
func TestNsecToSecret(t *testing.T) {
	hex64 := "b9100ce43b1deac5c5189f48bc6d97b287e122211d5f271488c0bcae2b350ad6"
	s, err := NsecToSecret(hex64)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(s[:]) != hex64 {
		t.Fatal("hex nsec misparsed")
	}
	if _, err := NsecToSecret("not-a-nsec"); err == nil {
		t.Fatal("invalid nsec should error")
	}
}
