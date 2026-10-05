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

// TestBuildHostFlagDefaultsEmpty: a NON-EMPTY --host default (the example
// value) rode every build's fromAnswers as a real answer and overwrote the
// recorded host on any box whose host differs — the world config then pointed
// at the example machine and `freehold update` SSHed the wrong host (seen
// live). The flag must default empty; the merge preserves the recorded host.
func TestBuildHostFlagDefaultsEmpty(t *testing.T) {
	f := buildCmd.Flags().Lookup("host")
	if f == nil {
		t.Fatal("build has no --host flag")
	}
	if f.DefValue != "" {
		t.Errorf("--host defaults to %q — every build persists it over the recorded host", f.DefValue)
	}
}
