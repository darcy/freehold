package crypto

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Sealed box wire format — reproduce core/src/crypto.rs byte-exactly.
//
//	[0]       version byte (currently 1)
//	[1..33]   ephemeral X25519 public key
//	[33..45]  12-byte nonce
//	[45..]    ChaCha20-Poly1305 ciphertext (tag appended)
const (
	sealedFormatVersion = 1
	sealedKeyLen        = 32
	ephPubLen           = 32
	nonceLen            = 12
	sealedSalt          = "freehold-sealed-box-v1"
)

var errNonContributory = errors.New("non-contributory DH: zero shared secret")

// deriveKey: AEAD_key = HKDF-SHA256(salt=sealedSalt, ikm=DH(eph_sk, recip_pk),
// info=eph_pk ‖ recip_pk). Rejects non-contributory DH (zero shared secret).
func deriveKey(ephPub, recipPub, shared []byte) ([]byte, error) {
	zero := true
	for _, b := range shared {
		if b != 0 {
			zero = false
			break
		}
	}
	if zero {
		return nil, errNonContributory
	}
	info := make([]byte, ephPubLen*2)
	copy(info[:ephPubLen], ephPub)
	copy(info[ephPubLen:], recipPub)

	key := make([]byte, sealedKeyLen)
	r := hkdf.New(sha256.New, shared, []byte(sealedSalt), info)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("kdf: %w", err)
	}
	return key, nil
}

// Seal encrypts plaintext to recipientPub (X25519) bound to aad (the secret
// NAME). Returns the versioned blob. Reproduces core::crypto::seal.
func Seal(recipientPub, aad, plaintext []byte) ([]byte, error) {
	if len(recipientPub) != ephPubLen {
		return nil, fmt.Errorf("recipient pubkey must be %d bytes, got %d", ephPubLen, len(recipientPub))
	}
	// Fresh ephemeral key per seal.
	ephPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("x25519 keygen: %w", err)
	}
	recipPubKey, err := ecdh.X25519().NewPublicKey(recipientPub)
	if err != nil {
		return nil, fmt.Errorf("invalid recipient pubkey: %w", err)
	}
	shared, err := ephPriv.ECDH(recipPubKey)
	if err != nil {
		return nil, err
	}
	ephPub := ephPriv.PublicKey().Bytes()
	key, err := deriveKey(ephPub, recipientPub, shared)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("chacha20poly1305: %w", err)
	}
	ct := aead.Seal(nil, nonce, plaintext, aad)

	out := make([]byte, 0, 1+ephPubLen+nonceLen+len(ct))
	out = append(out, sealedFormatVersion)
	out = append(out, ephPub...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// Open decrypts a sealed blob with recipientSecret (X25519 static secret)
// bound to aad (the secret NAME). Reproduces core::crypto::open.
func Open(recipientSecret, aad, blob []byte) ([]byte, error) {
	header := 1 + ephPubLen + nonceLen
	if len(blob) < header {
		return nil, fmt.Errorf("sealed blob too short: %d bytes", len(blob))
	}
	if blob[0] != sealedFormatVersion {
		return nil, fmt.Errorf("unsupported sealed-box version %d", blob[0])
	}
	ephPub := blob[1 : 1+ephPubLen]
	nonce := blob[1+ephPubLen : header]

	if len(recipientSecret) != sealedKeyLen {
		return nil, fmt.Errorf("recipient secret must be %d bytes, got %d", sealedKeyLen, len(recipientSecret))
	}
	privKey, err := ecdh.X25519().NewPrivateKey(recipientSecret)
	if err != nil {
		return nil, fmt.Errorf("invalid recipient secret: %w", err)
	}
	recipPub := privKey.PublicKey().Bytes()
	ephPubKey, err := ecdh.X25519().NewPublicKey(ephPub)
	if err != nil {
		return nil, fmt.Errorf("invalid ephemeral pubkey: %w", err)
	}
	shared, err := privKey.ECDH(ephPubKey)
	if err != nil {
		return nil, err
	}
	key, err := deriveKey(ephPub, recipPub, shared)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("chacha20poly1305: %w", err)
	}
	pt, err := aead.Open(nil, nonce, blob[header:], aad)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %w", err)
	}
	return pt, nil
}
