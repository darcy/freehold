package wire

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"golang.org/x/crypto/chacha20"
	"golang.org/x/crypto/hkdf"
)

// NIP-44 v2 self-encryption — reproduce rust-nostr 0.38 nip44::V2
// byte-for-byte (core/src/memory.rs). This is the one primitive with a full
// external spec; the harness oracle is the truth that catches byte drift.

const (
	nip44Version         = 2
	nip44NonceSize       = 32
	nip44HmacSize        = 32
	nip44MessageKeysSize = 76
)

var errNip44 = errors.New("nip44 v2")

// ConversationKey is the NIP-44 v2 conversation key (HKDF-SHA256 extract with
// salt "nip44-v2" over the ECDH shared secret).
type ConversationKey [32]byte

// DeriveConversationKey derives the conversation key from the agent's own
// secp256k1 keypair (sender == receiver — self-encryption). The pubkey is
// normalized to EVEN parity and the shared key is the X coordinate of the
// ECDH point. Reproduces rust-nostr's ConversationKey::derive +
// util::generate_shared_key.
func DeriveConversationKey(secret []byte) (*ConversationKey, error) {
	shared, err := ecdhSharedSecret(secret)
	if err != nil {
		return nil, err
	}
	// hkdf::extract(b"nip44-v2", shared) — HKDF-Extract with salt as the
	// prk-in... rust-nostr's hkdf::extract uses salt as HKDF salt.
	prk := hmac.New(sha256.New, []byte("nip44-v2"))
	prk.Write(shared)
	key := &ConversationKey{}
	copy(key[:], prk.Sum(nil))
	return key, nil
}

// ecdhSharedSecret computes the ECDH shared secret point's X coordinate
// (first 32 bytes) between secret and its OWN pubkey normalized to even
// parity — matching rust-nostr's generate_shared_key for self-encryption.
// For sender == receiver we need the public key of the same secret.
func ecdhSharedSecret(secret []byte) ([]byte, error) {
	if len(secret) != 32 {
		return nil, fmt.Errorf("memory key error: secret must be 32 bytes, got %d", len(secret))
	}
	// ecdh::shared_secret_point(normalized_pubkey, secret_key): the ECDH
	// shared point = secret_scalar * normalized(P). Its X coordinate is the
	// shared key. With the pubkey normalized to even parity, the point's
	// X coordinate is the same regardless of the other factor's sign used to
	// build the normalized pubkey; but the scalar used in multiplication is
	// still `secret` — so it's secret*P. For the self case, P = G*secret
	// normalized to even-Y; X(secret*(G*secret)) = X(secret^2 * G).
	//
	// Rust computes shared_secret_point where pubkey is a NormalizedPublicKey
	// (even-Y G*secret) and the multiplier is the secret scalar. Go stdlib
	// elliptic curves can compute secret*(secret*G) directly on the base
	// point. We build the even-parity pubkey and do scalar mult of secret over
	// it and take X.
	return ecdhEvenParitySharedX(secret)
}

// SealMemory encrypts value for the agent's own key (self NIP-44 v2), returns
// base64. Reproduces core::memory::seal_memory.
func SealMemory(secret []byte, value string) (string, error) {
	ck, err := DeriveConversationKey(secret)
	if err != nil {
		return "", err
	}
	payload, err := encryptToBytes(ck, []byte(value))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(payload), nil
}

// OpenMemory decrypts a stored NIP-44 v2 payload (base64) with the agent's own
// key. Wrong key or tamper -> error (fail closed).
func OpenMemory(secret []byte, sealed string) (string, error) {
	ck, err := DeriveConversationKey(secret)
	if err != nil {
		return "", err
	}
	payload, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("memory decrypt: bad base64: %w", err)
	}
	plain, err := decryptToBytes(ck, payload)
	if err != nil {
		return "", fmt.Errorf("memory decrypt: %w", err)
	}
	return string(plain), nil
}

// getMessageKeys derives the per-message keys (76 bytes) from the nonce.
func getMessageKeys(ck *ConversationKey, nonce []byte) ([]byte, error) {
	expanded := make([]byte, nip44MessageKeysSize)
	r := hkdf.Expand(sha256.New, ck[:], nonce)
	if _, err := r.Read(expanded); err != nil {
		return nil, fmt.Errorf("nip44: %w", err)
	}
	return expanded, nil
}

// nip44MaxPlaintext is the largest plaintext NIP-44 v2 can represent: the
// 2-byte length prefix caps at 65535, and the spec caps at 65536-128. A larger
// value would be silently truncated by the u16 prefix and produce a payload no
// reader can ever decrypt — error at WRITE instead (IMPORTANT-5).
const nip44MaxPlaintext = 65536 - 128

func encryptToBytes(ck *ConversationKey, plaintext []byte) ([]byte, error) {
	if len(plaintext) < 1 {
		return nil, fmt.Errorf("%w: message empty", errNip44)
	}
	if len(plaintext) > nip44MaxPlaintext {
		return nil, fmt.Errorf("%w: message too long (%d bytes, max %d)", errNip44, len(plaintext), nip44MaxPlaintext)
	}
	// Random 32-byte nonce.
	nonce := make([]byte, nip44NonceSize)
	randRead(nonce)
	keys, err := getMessageKeys(ck, nonce)
	if err != nil {
		return nil, err
	}

	// Pad.
	buffer := pad(plaintext)

	// ChaCha20 stream cipher: keys[0:32] = encryption, keys[32:44] = nonce.
	cipher, _ := chacha20.NewUnauthenticatedCipher(keys[0:32], keys[32:44])
	cipher.XORKeyStream(buffer, buffer)

	// HMAC-SHA256 over nonce || buffer with keys[44:76] = auth.
	mac := hmac.New(sha256.New, keys[44:76])
	mac.Write(nonce)
	mac.Write(buffer)
	hmacBytes := mac.Sum(nil)

	// Payload: 0x02 version || nonce(32) || buffer || hmac(32).
	payload := make([]byte, 0, 1+nip44NonceSize+len(buffer)+nip44HmacSize)
	payload = append(payload, nip44Version)
	payload = append(payload, nonce...)
	payload = append(payload, buffer...)
	payload = append(payload, hmacBytes...)
	return payload, nil
}

func decryptToBytes(ck *ConversationKey, payload []byte) ([]byte, error) {
	if len(payload) < 1+32+0+32 {
		return nil, errNip44
	}
	if payload[0] != nip44Version {
		return nil, fmt.Errorf("%w: unsupported version %d", errNip44, payload[0])
	}
	nonce := payload[1:33]
	if len(payload) < 65 {
		return nil, fmt.Errorf("%w: payload too short", errNip44)
	}
	buffer := payload[33 : len(payload)-32]
	mac := payload[len(payload)-32:]

	keys, err := getMessageKeys(ck, nonce)
	if err != nil {
		return nil, err
	}

	// Verify HMAC.
	calc := hmac.New(sha256.New, keys[44:76])
	calc.Write(nonce)
	calc.Write(buffer)
	if !hmac.Equal(calc.Sum(nil), mac) {
		return nil, fmt.Errorf("%w: invalid hmac", errNip44)
	}

	// Decrypt.
	cipher, _ := chacha20.NewUnauthenticatedCipher(keys[0:32], keys[32:44])
	decrypted := make([]byte, len(buffer))
	cipher.XORKeyStream(decrypted, buffer)

	// Unpad.
	if len(decrypted) < 2 {
		return nil, fmt.Errorf("%w: invalid padding", errNip44)
	}
	unpaddedLen := int(binary.BigEndian.Uint16(decrypted[0:2]))
	if len(decrypted) < 2+unpaddedLen {
		return nil, fmt.Errorf("%w: invalid padding", errNip44)
	}
	unpadded := decrypted[2 : 2+unpaddedLen]
	if len(unpadded) == 0 {
		return nil, fmt.Errorf("%w: message empty", errNip44)
	}
	if len(unpadded) != unpaddedLen {
		return nil, fmt.Errorf("%w: invalid padding", errNip44)
	}
	if len(decrypted) != 2+calcPadding(unpaddedLen) {
		return nil, fmt.Errorf("%w: invalid padding", errNip44)
	}
	return unpadded, nil
}

func pad(unpadded []byte) []byte {
	length := len(unpadded)
	take := calcPadding(length) - length
	// 2-byte length prefix.
	padded := make([]byte, 0, 2+length+take)
	var lenBytes [2]byte
	binary.BigEndian.PutUint16(lenBytes[:], uint16(length))
	padded = append(padded, lenBytes[:]...)
	padded = append(padded, unpadded...)
	for i := 0; i < take; i++ {
		padded = append(padded, 0)
	}
	return padded
}

func calcPadding(length int) int {
	if length <= 32 {
		return 32
	}
	nextpower := 1 << (log2RoundDown(length-1) + 1)
	chunk := 32
	if nextpower > 256 {
		chunk = nextpower / 8
	}
	return chunk * (((length - 1) / chunk) + 1)
}

func log2RoundDown(x int) uint32 {
	if x <= 0 {
		return 0
	}
	return uint32(math.Floor(math.Log2(float64(x))))
}

// randRead fills b with CSPRNG bytes (os.Read math/rand fallback).
func randRead(b []byte) {
	var i int
	for i < len(b) {
		n, err := cryptoRandRead(b[i:])
		if err != nil {
			panic(fmt.Sprintf("nip44: crypto/rand failed: %v", err))
		}
		i += n
	}
}
