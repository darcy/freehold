package build

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/config"
	"freehold/contract/crypto"
	cert "freehold/platform/services/certificates/letsencrypt"
	"freehold/platform/provisioning/box"
)

func TestCertCachePathIsBaseScopedAndHostKeyed(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	p := certCachePath("relay.fresh.freehold.technology")
	if strings.Contains(p, "profiles/") {
		t.Fatalf("cache path must live OUTSIDE profiles/ (a profile's uninstall --remove-data wipes it): %s", p)
	}
	if !strings.Contains(p, "relay.fresh.freehold.technology") {
		t.Fatalf("cache path must be keyed by the cert's host: %s", p)
	}
	if filepath.Dir(p) != filepath.Join(config.DefaultStateHome(), "cert-cache") {
		t.Fatalf("unexpected cache dir: %s", p)
	}
}

func TestCertCacheRoundtripThroughBaseIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)

	// the base ops identity the cache seals to (what a box's first login mints)
	enc := make([]byte, 32)
	if _, err := crand.Read(enc); err != nil {
		t.Fatal(err)
	}
	opsDir := filepath.Join(home, "control-plane", "agent-ops")
	if err := os.MkdirAll(opsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	idJSON, err := json.Marshal(map[string]string{"nostr_secret_hex": hex.EncodeToString(enc), "enc_secret_hex": hex.EncodeToString(enc)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opsDir, "identity.json"), idJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := box.LoadIdentity(opsDir); err != nil {
		t.Fatalf("identity the fill/ship relies on must load: %v", err)
	}

	// fill: seal to the base identity's pub; ship: open with its secret
	_, baseSec, pub, err := baseCacheIdent()
	if err != nil {
		t.Fatal(err)
	}
	host := "relay.fresh.freehold.technology"
	path := certCachePath(host)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"host": host, "fullchain": "fc-bytes", "key": "key-bytes"}
	seal := func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) }
	if err := cert.SaveCreds(path, "cert-cache", env, seal, pub, "cert-cache-"+host); err != nil {
		t.Fatal(err)
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	_, got, err := cert.LoadCreds(path, open, baseSec)
	if err != nil {
		t.Fatalf("the ship must open the cache a PREVIOUS profile sealed: %v", err)
	}
	if got["host"] != host || got["fullchain"] != "fc-bytes" || got["key"] != "key-bytes" {
		t.Fatalf("roundtrip mismatch: %v", got)
	}
}
