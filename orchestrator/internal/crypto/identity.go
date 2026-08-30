// Package crypto reproduces the freehold-core identity + signing surface
// (core/src/identity.rs, core/src/audit.rs) byte-exactly in Go.
//
//   - Nostr identity: secp256k1 keypair with an x-only pubkey, BIP-340 Schnorr
//     signatures (deterministic, no aux rand — matching the Rust audit signer).
//   - bech32 nsec/npub decode (BIP-173), reproducing the orchestrator's
//     nsec_to_secret (accepts nsec1... or bare 64-hex, no 5->8 repack).
//   - ed25519 runner SSH keypair generation (OpenSSH public-key line format).
package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// secp256k1 group order N. Rust's SecretKey::from_slice rejects secrets not
// in [1, n-1]; btcec's SetByteSlice silently REDUCES a too-large scalar
// modulo N (which would derive the wrong pubkey vs Rust), so we validate the
// raw 32-byte scalar ourselves before use.
var secpOrderN = mustHex("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141")

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// validateScalar rejects a 32-byte secret not in [1, n-1], matching
// secp256k1::SecretKey::from_slice semantics.
func validateScalar(secret []byte) error {
	if len(secret) != 32 {
		return fmt.Errorf("secret must be 32 bytes, got %d", len(secret))
	}
	zero := true
	for _, b := range secret {
		if b != 0 {
			zero = false
			break
		}
	}
	if zero {
		return fmt.Errorf("invalid secp256k1 secret key (scalar must be in [1, n-1])")
	}
	if isGE(secret, secpOrderN) {
		return fmt.Errorf("invalid secp256k1 secret key (scalar must be in [1, n-1])")
	}
	return nil
}

// isGE reports whether a >= b for equal-length big-endian byte slices.
func isGE(a, b []byte) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

// PrivKey validates the scalar and returns the btcec private key, or an error.
func PrivKey(secret []byte) (*btcec.PrivateKey, error) {
	if err := validateScalar(secret); err != nil {
		return nil, err
	}
	prv, _ := btcec.PrivKeyFromBytes(secret)
	return prv, nil
}

// PubkeyFromSecret derives the Nostr x-only public key (hex) from a 32-byte
// secret seed, matching core/src/identity.rs::nostr_pubkey_hex.
func PubkeyFromSecret(secret []byte) (string, error) {
	prv, err := PrivKey(secret)
	if err != nil {
		return "", err
	}
	comp := prv.PubKey().SerializeCompressed()
	// x-only pubkey = the 32-byte x coordinate (drop the 0x02/0x03 prefix).
	return hex.EncodeToString(comp[1:]), nil
}

// VerifyBIP340 verifies a BIP-340 Schnorr signature over msgDigest by an
// x-only pubkey (64-hex). Matches core/src/audit.rs::verify_event.
func VerifyBIP340(pubkeyHex string, msgDigest []byte, sig []byte) error {
	pkBytes, err := hex.DecodeString(pubkeyHex)
	if err != nil {
		return fmt.Errorf("invalid pubkey hex: %w", err)
	}
	if len(pkBytes) != 32 {
		return fmt.Errorf("x-only pubkey must be 32 bytes, got %d", len(pkBytes))
	}
	xonlyPub, err := schnorr.ParsePubKey(pkBytes)
	if err != nil {
		return fmt.Errorf("invalid x-only pubkey: %w", err)
	}
	sigStruct, err := schnorr.ParseSignature(sig)
	if err != nil {
		return fmt.Errorf("invalid signature: %w", err)
	}
	if !sigStruct.Verify(msgDigest, xonlyPub) {
		return fmt.Errorf("schnorr signature verification failed")
	}
	return nil
}

// GenerateED25519SSHKeypair generates an ed25519 SSH keypair, reproducing
// core/src/identity.rs::generate_ssh_keypair. Returns the private
// (openssh-key-v1 PEM) and the public (OpenSSH authorized_keys line).
func GenerateED25519SSHKeypair(comment string) (privatePEM []byte, pubLine string, err error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, "", fmt.Errorf("generate ed25519 seed: %w", err)
	}
	return encodeED25519OpenSSH(seed, comment)
}
