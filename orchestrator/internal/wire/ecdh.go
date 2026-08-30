package wire

import (
	"crypto/rand"
	"math/big"

	"freehold/orchestrator/internal/crypto"
	"github.com/btcsuite/btcd/btcec/v2"
)

// cryptoRandRead is the CSPRNG backing randRead.
func cryptoRandRead(b []byte) (int, error) { return rand.Read(b) }

// ecdhEvenParitySharedX computes X(secret * P) where P = secret*G — the
// secp256k1 ECDH shared point's X coordinate, reproducing rust-nostr's
// generate_shared_key for the self-encryption case (sender == receiver).
//
// rust-nostr computes ecdh::shared_secret_point(normalized_pubkey, secret)
// and takes the first 32 bytes (X). Normalizing the pubkey to even parity
// only flips Y's sign; negating a point negates only its Y, leaving X of the
// product unchanged — so multiplying by the pubkey directly yields the same
// X coordinate.
func ecdhEvenParitySharedX(secret []byte) ([]byte, error) {
	prv, err := crypto.PrivKey(secret)
	if err != nil {
		return nil, err
	}
	pub := prv.PubKey()
	// Extract X,Y from the uncompressed serialization
	// ([0x04 || X(32) || Y(32)]).
	ser := pub.SerializeUncompressed()
	x := new(big.Int).SetBytes(ser[1:33])
	y := new(big.Int).SetBytes(ser[33:65])

	curve := btcec.S256()
	rx, _ := curve.ScalarMult(x, y, secret)
	out := make([]byte, 32)
	rx.FillBytes(out)
	return out, nil
}
