package cert

import (
	"path/filepath"
	"testing"

	"github.com/go-acme/lego/v4/certcrypto"
)

// TestResumeStateRoundTrip locks the persisted-order shape: what Begin persists
// must be loadable again by TryLoad (the resume path), with the same kid/order
// identity and the account key reopened. Seal/Open are identity mocks here — the
// real path seals to the ops identity; the JSON+parse+expiry logic is what this
// guards.
func TestResumeStateRoundTrip(t *testing.T) {
	key, err := certcrypto.GeneratePrivateKey(certcrypto.RSA2048)
	if err != nil {
		t.Fatal(err)
	}
	r := &Resume{
		Domain: "relay.librem.freehold.technology",
		Path:   filepath.Join(t.TempDir(), "pending.json"),
		Seal:   func(_, _, plain []byte) ([]byte, error) { return plain, nil },
		Open:   func(_, _, blob []byte) ([]byte, error) { return blob, nil },
	}
	po := &pendingOrder{
		acctKey:  key,
		kid:      "https://acme.example/acct/1",
		orderURL: "https://acme.example/order/1",
	}
	if err := r.persist(po, "_acme-challenge.relay.librem.freehold.technology"); err != nil {
		t.Fatalf("persist: %v", err)
	}
	got, ok, err := r.TryLoad()
	if err != nil || !ok {
		t.Fatalf("TryLoad: ok=%v err=%v", ok, err)
	}
	if got.orderURL != po.orderURL || got.kid != po.kid {
		t.Errorf("resumed identity mismatch: order=%q kid=%q", got.orderURL, got.kid)
	}
	if got.acctKey == nil {
		t.Error("resumed account key not reopened")
	}

	// TryLoad returns (nil,false) for a missing state file.
	absent := &Resume{Path: filepath.Join(t.TempDir(), "nope.json")}
	if _, ok, _ := absent.TryLoad(); ok {
		t.Error("expected no resumed order for a missing state file")
	}
}
