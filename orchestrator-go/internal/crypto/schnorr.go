package crypto

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/btcsuite/btcd/btcec/v2"
)

// BIP-340 Schnorr signing with all-zero auxiliary randomness ("no aux rand"),
// byte-matching Rust secp256k1 0.30's `sign_schnorr_no_aux_rand` (which passes
// NULL aux to libsecp256k1's nonce_function_bip340, i.e. aux = 0x00^32).
//
// btcec's default schnorr.Sign uses an RFC6979 nonce, which is NOT the same
// signature the Rust core produces — so this own implementation is required
// for byte-exactness.

// taggedHash computes SHA256(sha256(tag) || sha256(tag) || data...), the
// BIP-340 tagged hash construction.
func taggedHash(tag string, data ...[]byte) []byte {
	tagHash := sha256.Sum256([]byte(tag))
	h := sha256.New()
	h.Write(tagHash[:])
	h.Write(tagHash[:])
	for _, d := range data {
		h.Write(d)
	}
	return h.Sum(nil)
}

func bigIntFromBytes(b []byte) *big.Int {
	return new(big.Int).SetBytes(b)
}

func isOdd(v *big.Int) bool {
	return v.Bit(0) == 1
}

// pointFromScalar computes G*d and returns (x, y).
func pointFromScalar(curve *btcec.KoblitzCurve, d *big.Int) (*big.Int, *big.Int) {
	x, y := curve.ScalarBaseMult(d.Bytes())
	return x, y
}

// pointTimes computes P*d and returns (x, y).
func pointTimes(curve *btcec.KoblitzCurve, px, py, d *big.Int) (*big.Int, *big.Int) {
	x, y := curve.ScalarMult(px, py, d.Bytes())
	return x, y
}

// SignBIP340NoAuxRand signs a 32-byte message with a deterministic BIP-340
// signature using all-zero auxiliary randomness — the exact signature the
// Rust core produces via sign_schnorr_no_aux_rand. Returns the 64-byte sig.
func SignBIP340NoAuxRand(secret []byte, msgDigest []byte) ([]byte, error) {
	if len(msgDigest) != 32 {
		return nil, fmt.Errorf("BIP-340 message must be 32 bytes, got %d", len(msgDigest))
	}
	if err := validateScalar(secret); err != nil {
		return nil, err
	}
	curve := btcec.S256()
	n := curve.N

	// d = secret (already validated in [1, n-1]).
	d := bigIntFromBytes(secret)

	// P = d*G; x-only pubkey x, parity = y odd.
	px, py := pointFromScalar(curve, d)
	// Negate d if P.y is odd.
	dNeg := new(big.Int).Sub(n, d)
	if isOdd(py) {
		d = dNeg
	}

	// masked_key = key XOR TaggedHash("BIP0340/aux", 0x00^32). For no_aux_rand
	// the aux is all zeros.
	auxMask := taggedHash("BIP0340/aux", make([]byte, 32))
	masked := make([]byte, 32)
	for i := 0; i < 32; i++ {
		masked[i] = secret[i] ^ auxMask[i]
	}

	// x32 (x-only pubkey bytes).
	x32 := px.Bytes()
	if len(x32) < 32 {
		x32 = append(make([]byte, 32-len(x32)), x32...)
	}

	// k' = int(TaggedHash("BIP0340/nonce", masked || x(P) || msg)) mod n.
	nonce := bigIntFromBytes(taggedHash("BIP0340/nonce", masked, x32, msgDigest))
	nonce.Mod(nonce, n)
	if nonce.Sign() == 0 {
		return nil, fmt.Errorf("schnorr: zero nonce")
	}

	// R = k*G; negate k if R.y is odd.
	rx, ry := pointFromScalar(curve, nonce)
	nonceNeg := new(big.Int).Sub(n, nonce)
	if isOdd(ry) {
		nonce = nonceNeg
	}

	// e = int(TaggedHash("BIP0340/challenge", x(R) || x(P) || msg)) mod n.
	rx32 := rx.Bytes()
	if len(rx32) < 32 {
		rx32 = append(make([]byte, 32-len(rx32)), rx32...)
	}
	challenge := bigIntFromBytes(taggedHash("BIP0340/challenge", rx32, x32, msgDigest))
	challenge.Mod(challenge, n)

	// s = (k' + e * d') mod n.
	ed := new(big.Int).Mul(challenge, d)
	s := new(big.Int).Add(nonce, ed)
	s.Mod(s, n)

	// sig = x(R) || s (each 32 bytes).
	sig := make([]byte, 0, 64)
	sig = append(sig, rx32...)
	sBytes := s.Bytes()
	if len(sBytes) < 32 {
		sBytes = append(make([]byte, 32-len(sBytes)), sBytes...)
	}
	sig = append(sig, sBytes...)
	return sig, nil
}

// SignBIP340 defaults to the no-aux-rand form to match the Rust core (the
// audit signer and all relay event signers use sign_schnorr_no_aux_rand).
func SignBIP340(secret []byte, msgDigest []byte) ([]byte, error) {
	return SignBIP340NoAuxRand(secret, msgDigest)
}

var _ = hex.EncodeToString
