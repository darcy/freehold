package box

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"freehold/contract/crypto"
	"freehold/contract/wire"
)

// Identity is a runner/agent identity file layout (identity.json: nostr +
// encryption secrets, 0600). Shared by the install engine and the box helpers
// that must reopen a sealed package (door re-derivation, cert identity).
type Identity struct {
	NostrSecretHex string `json:"nostr_secret_hex"`
	EncSecretHex   string `json:"enc_secret_hex"`
}

// hexDecode decodes a hex string.
func hexDecode(s string) ([]byte, error) { return hex.DecodeString(s) }

// pubkeyFromSecret derives the Nostr x-only pubkey from a raw secret.
func pubkeyFromSecret(secret []byte) (string, error) { return crypto.PubkeyFromSecret(secret) }

// LoadIdentity reads identity.json in dir.
func LoadIdentity(dir string) (*Identity, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// NostrPubkeyHex derives the Nostr pubkey from the identity's secret.
func (id *Identity) NostrPubkeyHex() (string, error) {
	secret, err := hex.DecodeString(id.NostrSecretHex)
	if err != nil {
		return "", err
	}
	return pubkeyFromSecret(secret)
}

// LoadPubkey derives the Nostr pubkey from an identity dir's identity.json.
func LoadPubkey(dir string) (string, error) {
	id, err := LoadIdentity(dir)
	if err != nil {
		return "", err
	}
	return id.NostrPubkeyHex()
}

// MintIdentity creates an agent identity dir (nostr + enc secrets) if missing,
// writing identity.json 0600.
func MintIdentity(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	nostrSecret := make([]byte, 32)
	encSecret := make([]byte, 32)
	if _, err := rand.Read(nostrSecret); err != nil {
		return err
	}
	if _, err := rand.Read(encSecret); err != nil {
		return err
	}
	doc := map[string]string{
		"nostr_secret_hex": hex.EncodeToString(nostrSecret),
		"enc_secret_hex":   hex.EncodeToString(encSecret),
	}
	return wire.WriteJSON0600(filepath.Join(dir, "identity.json"), doc)
}

// EnsureIdentity mints an identity dir if missing; a present one is kept.
func EnsureIdentity(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err == nil {
		return nil
	}
	return MintIdentity(dir)
}
