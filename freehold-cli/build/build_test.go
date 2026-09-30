package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/config"
	"freehold/contract/crypto"
	cert "freehold/platform/services/certificates/letsencrypt"
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

	// baseCacheIdent MINTS the base ops identity on first use (nothing else
	// creates it — login/install both pin a profile first)
	_, baseSec, pub, err := baseCacheIdent()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "control-plane", "agent-ops", "identity.json")); err != nil {
		t.Fatalf("baseCacheIdent must mint the base ops identity: %v", err)
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
