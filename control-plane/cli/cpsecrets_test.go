package cli

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"freehold/contract/crypto"
	cert "freehold/platform/services/certificates/letsencrypt"
)

func TestCpSecretBlobRoundTrip(t *testing.T) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	pub, err := crypto.X25519PublicKey(secret[:])
	if err != nil {
		t.Fatal(err)
	}
	seal := func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) }
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }

	blob, err := cpSecretBlob("litellm", "litellm", map[string]string{
		"master": "m", "pg": "p", "provider": "k",
	}, seal, pub)
	if err != nil {
		t.Fatalf("cpSecretBlob: %v", err)
	}
	path := filepath.Join(t.TempDir(), "litellm.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	provider, env, err := cert.LoadCreds(path, open, secret[:])
	if err != nil {
		t.Fatalf("LoadCreds: %v", err)
	}
	if provider != "litellm" || env["master"] != "m" || env["pg"] != "p" || env["provider"] != "k" {
		t.Fatalf("round-trip mismatch: provider=%q env=%v", provider, env)
	}
}
