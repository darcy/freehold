package wire

import (
	"crypto/rand"
	"encoding/hex"
)

// randomHexBytes returns a random hex string of n bytes, mirroring
// core::nip98::random_hex.
func randomHexBytes() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
