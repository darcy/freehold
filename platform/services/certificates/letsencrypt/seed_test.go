package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoversHost(t *testing.T) {
	cases := []struct {
		names []string
		host  string
		want  bool
	}{
		{[]string{"relay.fresh.example"}, "relay.fresh.example", true},
		{[]string{"*.fresh.example"}, "relay.fresh.example", true},
		{[]string{"*.fresh.example"}, "x.y.fresh.example", false}, // one label deep only
		{[]string{"*.other.example"}, "relay.fresh.example", false},
		{[]string{"fresh.example"}, "relay.fresh.example", false},
		{nil, "relay.fresh.example", false},
		{[]string{"*.fresh.example"}, "fresh.example", false}, // bare zone not covered
	}
	for _, c := range cases {
		if got := CoversHost(c.names, c.host); got != c.want {
			t.Errorf("CoversHost(%v, %q) = %v, want %v", c.names, c.host, got, c.want)
		}
	}
}

// testCert mints a self-signed leaf with the given SANs and notAfter.
func testCert(t *testing.T, dnsNames []string, notAfter time.Time) (fullchain, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "seed-test"},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	fullchain = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return fullchain, keyPEM
}

func writeSeed(t *testing.T, env map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cert-seed-relay.json")
	if err := SaveCreds(path, "cert-cache", env, staticSeal, []byte{1}, "cert-seed-relay"); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadSeedVerdicts(t *testing.T) {
	host := "relay.fresh.example"
	fresh, freshKey := testCert(t, []string{host}, time.Now().Add(60*24*time.Hour))
	stale, staleKey := testCert(t, []string{host}, time.Now().Add(20*24*time.Hour))
	other, otherKey := testCert(t, []string{"relay.other.example"}, time.Now().Add(60*24*time.Hour))
	now := time.Now()
	ok30d := 30 * 24 * time.Hour

	t.Run("valid seed installs", func(t *testing.T) {
		path := writeSeed(t, map[string]string{"host": host, "fullchain": string(fresh), "key": string(freshKey)})
		fc, key, err := LoadSeed(path, staticOpen, nil, host, now, ok30d)
		if err != nil {
			t.Fatalf("valid seed rejected: %v", err)
		}
		if string(fc) != string(fresh) || string(key) != string(freshKey) {
			t.Errorf("seed material mismatch")
		}
	})

	t.Run("wrong host falls through", func(t *testing.T) {
		path := writeSeed(t, map[string]string{"host": "relay2.fresh.example", "fullchain": string(fresh), "key": string(freshKey)})
		if _, _, err := LoadSeed(path, staticOpen, nil, host, now, ok30d); err == nil || !strings.Contains(err.Error(), "relay2") {
			t.Fatalf("wrong-host seed accepted (err=%v)", err)
		}
	})

	t.Run("expired seed falls through", func(t *testing.T) {
		path := writeSeed(t, map[string]string{"host": host, "fullchain": string(stale), "key": string(staleKey)})
		if _, _, err := LoadSeed(path, staticOpen, nil, host, now, ok30d); err == nil {
			t.Fatal("stale seed accepted")
		}
	})

	t.Run("SAN mismatch falls through", func(t *testing.T) {
		// recorded host matches, but the LEAF does not cover it — the guard
		// against a renamed-domain cache installing the wrong cert.
		path := writeSeed(t, map[string]string{"host": host, "fullchain": string(other), "key": string(otherKey)})
		if _, _, err := LoadSeed(path, staticOpen, nil, host, now, ok30d); err == nil || !strings.Contains(err.Error(), "cover") {
			t.Fatalf("SAN-mismatched seed accepted (err=%v)", err)
		}
	})

	t.Run("missing material falls through", func(t *testing.T) {
		path := writeSeed(t, map[string]string{"host": host})
		if _, _, err := LoadSeed(path, staticOpen, nil, host, now, ok30d); err == nil {
			t.Fatal("empty seed accepted")
		}
	})

	t.Run("missing file falls through", func(t *testing.T) {
		if _, _, err := LoadSeed(filepath.Join(t.TempDir(), "absent.json"), staticOpen, nil, host, now, ok30d); err == nil {
			t.Fatal("absent seed accepted")
		}
	})
}
