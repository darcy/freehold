package crypto

import (
	"bytes"
	"testing"
)

// TestSSHPrivateKeyFromSeedRoundTrip: the transient-access private half must
// derive the SAME public line as SSHPublicKeyFromSeed for the same seed +
// comment (DOOR_SPEC: the box reaches the host with the key whose pubkey it
// presents).
func TestSSHPrivateKeyFromSeedRoundTrip(t *testing.T) {
	seed := bytes.Repeat([]byte{0x42}, 32)
	const comment = "freehold-door-testbox"

	pub, err := SSHPublicKeyFromSeed(seed, comment)
	if err != nil {
		t.Fatal(err)
	}
	pem, err := SSHPrivateKeyPEMFromSeed(seed, comment)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := ExtractED25519PublicKeyLine(pem)
	if err != nil {
		t.Fatal(err)
	}
	if derived != pub {
		t.Errorf("private-derived pubkey %q != public-from-seed %q", derived, pub)
	}
	// Deterministic: the same seed yields the same public line.
	pub2, _ := SSHPublicKeyFromSeed(seed, comment)
	if pub2 != pub {
		t.Errorf("derivation is not deterministic")
	}
	// A different seed must not collide.
	other, _ := SSHPublicKeyFromSeed(bytes.Repeat([]byte{0x43}, 32), comment)
	if other == pub {
		t.Errorf("different seeds produced the same key")
	}
}

func TestSSHPrivateKeyFromSeedRejectsBadSeed(t *testing.T) {
	if _, err := SSHPrivateKeyPEMFromSeed(make([]byte, 31), "x"); err == nil {
		t.Error("a non-32-byte seed must error")
	}
}
