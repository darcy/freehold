package certcred

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"freehold/contract/crypto"
	cert "freehold/platform/services/certificates/letsencrypt"
)

// TestDNSZone: the zone is everything after the first label; a bare host has
// none.
func TestDNSZone(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"relay.example.com", "example.com"},
		{"cp.example.com", "example.com"},
		{"relay.a.b.example.com", "a.b.example.com"},
		{"localhost", ""},
		{"", ""},
	} {
		if got := DNSZone(tc.host); got != tc.want {
			t.Errorf("DNSZone(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

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

	blob, err := CPSecretBlob("litellm", "litellm", map[string]string{
		"master": "m", "pg": "p", "provider": "k",
	}, seal, pub)
	if err != nil {
		t.Fatalf("CPSecretBlob: %v", err)
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
