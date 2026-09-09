package crypto

import (
	"crypto/ecdh"
	"fmt"
)

// X25519PublicKey derives the raw X25519 public key (32 bytes) from a 32-byte
// secret, matching identity::enc_pubkey_hex (x25519_dalek StaticSecret::from).
func X25519PublicKey(secret []byte) ([]byte, error) {
	if len(secret) != 32 {
		return nil, fmt.Errorf("enc secret must be 32 bytes, got %d", len(secret))
	}
	priv, err := ecdh.X25519().NewPrivateKey(secret)
	if err != nil {
		return nil, fmt.Errorf("invalid x25519 secret: %w", err)
	}
	return priv.PublicKey().Bytes(), nil
}
