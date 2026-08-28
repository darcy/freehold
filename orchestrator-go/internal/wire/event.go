package wire

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"freehold/orchestrator-go/internal/crypto"
)

// Nostr event kinds (core/src/nip98.rs).
const (
	KINDHTTPAuth   = 27235
	ChannelCreate  = 9007
	PutUser        = 9000
	RemoveUser     = 9001
	GroupMeta      = 39000
	GroupMembers   = 39002
	MemoryKind     = 30174
	ChannelMessage = 9
)

// canonicalEventBytes serializes the NIP-01 id array [0, pubkey, created_at,
// kind, tags, content] as JSON without whitespace, HTML-escape DISABLED —
// serde_json does not escape <>& or U+2028/U+2029, while Go's default
// encoding/json does. This is the byte-exact canonical form (core/src/nip98.rs).
func canonicalEventBytes(pubkeyHex string, createdAt int64, kind uint32, tags [][]string, content string) ([]byte, error) {
	arr := []interface{}{
		0,
		pubkeyHex,
		createdAt,
		kind,
		tags,
		content,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(arr); err != nil {
		return nil, err
	}
	// Encoder.Encode appends a trailing newline; strip it for byte-exactness.
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

// EventID returns the sha256 of the canonical event serialization (Nostr id).
func EventID(pubkeyHex string, createdAt int64, kind uint32, tags [][]string, content string) ([32]byte, error) {
	canon, err := canonicalEventBytes(pubkeyHex, createdAt, kind, tags, content)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canon), nil
}

// SignEvent signs a Nostr event with the identity's 32-byte secret seed;
// returns (pubkey_hex, id_hex, sig_hex). Matches core::nip98::sign_event.
func SignEvent(secret []byte, kind uint32, createdAt int64, tags [][]string, content string) (string, string, string, error) {
	pubkeyHex, err := crypto.PubkeyFromSecret(secret)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid secret seed: %w", err)
	}
	id, err := EventID(pubkeyHex, createdAt, kind, tags, content)
	if err != nil {
		return "", "", "", err
	}
	sig, err := crypto.SignBIP340(secret, id[:])
	if err != nil {
		return "", "", "", err
	}
	return pubkeyHex, hex.EncodeToString(id[:]), hex.EncodeToString(sig), nil
}

// VerifyEvent verifies a signed event against its own id/signature; returns
// the pubkey hex on success. Matches core::nip98::verify_event.
func VerifyEvent(pubkeyHex string, createdAt int64, kind uint32, tags [][]string, content, sigHex string) (string, error) {
	id, err := EventID(pubkeyHex, createdAt, kind, tags, content)
	if err != nil {
		return "", err
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return "", fmt.Errorf("signature is not hex")
	}
	if len(sig) != 64 {
		return "", fmt.Errorf("signature is not 64 bytes")
	}
	if err := crypto.VerifyBIP340(pubkeyHex, id[:], sig); err != nil {
		return "", fmt.Errorf("bad signature: %w", err)
	}
	return pubkeyHex, nil
}

// Nip98Auth builds the NIP-98 `Authorization: Nostr <b64(event)>` value.
// A random `nonce` tag makes rapid same-second publishes distinct (the
// relay's replay set dedupes by event id). Matches core::nip98::nip98_auth.
func Nip98Auth(secret []byte, method, url string, nowSecs int64) (string, error) {
	nonce, err := randomHexBytes()
	if err != nil {
		return "", err
	}
	tags := [][]string{
		{"u", url},
		{"method", method},
		{"nonce", nonce},
	}
	pubkey, id, sig, err := SignEvent(secret, KINDHTTPAuth, nowSecs, tags, "")
	if err != nil {
		return "", err
	}
	ev := map[string]interface{}{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": nowSecs,
		"kind":       KINDHTTPAuth,
		"tags":       [][]string{{"u", url}, {"method", method}, {"nonce", nonce}},
		"content":    "",
		"sig":        sig,
	}
	var evBuf bytes.Buffer
	enc := json.NewEncoder(&evBuf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev); err != nil {
		return "", err
	}
	evBytes := evBuf.Bytes()
	if len(evBytes) > 0 && evBytes[len(evBytes)-1] == '\n' {
		evBytes = evBytes[:len(evBytes)-1]
	}
	return "Nostr " + base64.StdEncoding.EncodeToString(evBytes), nil
}
