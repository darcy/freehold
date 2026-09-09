package cert

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Sealer encrypts a plaintext blob to a recipient (AAD-bound). Open decrypts.
// The engine supplies these backed by freehold's own ops identity, so the DNS
// provider token is sealed-to-self at rest and freehold can reopen it to run
// lego in-process — never plaintext on disk or in config.
type Sealer func(recipientPub, aad, plaintext []byte) ([]byte, error)
type Opener func(recipientSecret, aad, blob []byte) ([]byte, error)

// dnsCred is the on-disk record for one DNS provider credential.
type dnsCred struct {
	Provider string `json:"provider"`
	Sealed   string `json:"sealed"` // hex ciphertext of the env map JSON
	AAD      string `json:"aad"`    // the bound secret name (defence for tamper)
}

// SaveCreds seals the provider's env map (JSON) to recipientPub and writes the
// record to path. aad binds the blob to the credential name.
func SaveCreds(path string, provider string, env map[string]string, seal Sealer, recipientPub []byte, aad string) error {
	plain, err := json.Marshal(env)
	if err != nil {
		return err
	}
	blob, err := seal(recipientPub, []byte(aad), plain)
	if err != nil {
		return err
	}
	rec := dnsCred{Provider: provider, Sealed: hex.EncodeToString(blob), AAD: aad}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// LoadCreds reads and reopens the record, returning provider + env map.
func LoadCreds(path string, open Opener, recipientSecret []byte) (string, map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	var rec dnsCred
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", nil, fmt.Errorf("dns cred parse: %w", err)
	}
	blob, err := hex.DecodeString(rec.Sealed)
	if err != nil {
		return "", nil, fmt.Errorf("dns cred blob: %w", err)
	}
	plain, err := open(recipientSecret, []byte(rec.AAD), blob)
	if err != nil {
		return "", nil, fmt.Errorf("dns cred unseal: %w", err)
	}
	var env map[string]string
	if err := json.Unmarshal(plain, &env); err != nil {
		return "", nil, fmt.Errorf("dns cred env: %w", err)
	}
	if rec.Provider == "" {
		return "", nil, fmt.Errorf("dns cred record has no provider")
	}
	return rec.Provider, env, nil
}

// CredExists reports whether a credential record is already on disk.
func CredExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
