package cert

import (
	"path/filepath"
	"testing"
)

// staticSeal/Open reproduce the AEAD wire with a fixed secret for the store test.
func staticSeal(pub, aad, plain []byte) ([]byte, error)   { return plain, nil }
func staticOpen(secret, aad, blob []byte) ([]byte, error) { return blob, nil }

func TestCredSourcelessRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dns-provider.json")
	env := map[string]string{"AWS_ACCESS_KEY_ID": "ak", "AWS_SECRET_ACCESS_KEY": "sk"}
	if err := SaveCreds(path, "route53", env, staticSeal, []byte{1, 2, 3}, "cert-dns"); err != nil {
		t.Fatal(err)
	}
	prov, got, err := LoadCreds(path, staticOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prov != "route53" {
		t.Errorf("provider = %q, want route53", prov)
	}
	if got["AWS_ACCESS_KEY_ID"] != "ak" || got["AWS_SECRET_ACCESS_KEY"] != "sk" {
		t.Errorf("env roundtrip = %+v", got)
	}
	if !CredExists(path) {
		t.Errorf("CredExists should be true after write")
	}
	if CredExists(filepath.Join(t.TempDir(), "nope")) {
		t.Errorf("CredExists false-positive")
	}
}

func TestCredLoadMissing(t *testing.T) {
	if _, _, err := LoadCreds(filepath.Join(t.TempDir(), "absent"), staticOpen, nil); err == nil {
		t.Fatal("expected error loading a missing cred record")
	}
}
