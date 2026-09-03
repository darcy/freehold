package cert

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
)

// writeSelfSigned writes a fullchain PEM whose leaf NotAfter is at `exp`.
func writeSelfSigned(t *testing.T, dir string, exp time.Time) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "*.freehold-test"},
		DNSNames:     []string{"*.freehold-test.darcydev.net"},
		NotBefore:    exp.Add(-24 * time.Hour),
		NotAfter:     exp,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	fc := filepath.Join(dir, "fullchain.pem")
	if err := os.WriteFile(fc, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return fc
}

func TestReuseIfValid(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	fc := writeSelfSigned(t, dir, now.Add(70*24*time.Hour)) // ~70d left

	got, ok := ReuseIfValid(fc, now, 30*24*time.Hour) // min 30d
	if !ok {
		t.Fatalf("valid cert (70d left, min 30d) should be reused")
	}
	if got.Before(now.Add(69 * 24 * time.Hour)) {
		t.Errorf("expiry parsed too early: %s", got)
	}

	// expiring: min 45d but only 70-30? Use a cert with < min left.
	fc2 := writeSelfSigned(t, dir, now.Add(10*24*time.Hour))
	if _, ok := ReuseIfValid(fc2, now, 30*24*time.Hour); ok {
		t.Errorf("cert with 10d left must NOT be reused when min is 30d")
	}

	// missing file -> never reuse
	if _, ok := ReuseIfValid(filepath.Join(dir, "nope.pem"), now, 0); ok {
		t.Errorf("missing file must not be reused")
	}
}

func TestProvidersAndEnvGeneratedFromLego(t *testing.T) {
	ps := Providers()
	if ps == nil {
		t.Fatal("providers must not be nil")
	}
	// sanity: some real lego providers are present, not a trivial curated list.
	for _, want := range []string{"route53", "cloudflare", "digitalocean", "duckdns"} {
		if !IsProvider(want) {
			t.Errorf("lego provider %q missing from generated registry", want)
		}
	}
	// env names derived from lego constants.
	if got := ProviderEnvNames("cloudflare"); !contains(got, "CLOUDFLARE_DNS_API_TOKEN") {
		t.Errorf("cloudflare env missing token: %v", got)
	}
	if got := ProviderEnvNames("digitalocean"); !contains(got, "DO_AUTH_TOKEN") {
		t.Errorf("digitalocean env missing token: %v", got)
	}
	if got := ProviderEnvNames("route53"); !contains(got, "AWS_ACCESS_KEY_ID") {
		t.Errorf("route53 env missing access key: %v", got)
	}
	if IsProvider("does-not-exist") {
		t.Errorf("bogus provider must be rejected")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
