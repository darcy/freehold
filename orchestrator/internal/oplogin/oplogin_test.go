package oplogin

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/orchestrator/internal/config"
	"freehold/orchestrator/internal/crypto"
)

// TestOperatorLedgerRoundTrip verifies the operator's nsec persists to the
// operator dir (isolated via FREEHOLD_HOME) and that first-run-wins holds.
func TestOperatorLedgerRoundTrip(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())

	secret := [32]byte{}
	secret[0] = 7
	wrote, err := Save(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("first save should write")
	}
	got, err := SecretHex()
	if err != nil {
		t.Fatal(err)
	}
	if want := hex.EncodeToString(secret[:]); got != want {
		t.Fatalf("secret mismatch: want %s got %s", want, got)
	}
	// First-run-wins: a second Save must not clobber.
	other := [32]byte{}
	other[0] = 9
	if wrote, _ = Save(other); wrote {
		t.Fatal("second save should be refused (first-run-wins)")
	}
	if got, _ = SecretHex(); got != hex.EncodeToString(secret[:]) {
		t.Fatal("operator identity was clobbered")
	}
}

// TestNsecToSecret accepts both nsec1 bech32 and bare hex.
func TestNsecToSecret(t *testing.T) {
	hex64 := "b9100ce43b1deac5c5189f48bc6d97b287e122211d5f271488c0bcae2b350ad6"
	s, err := NsecToSecret(hex64)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(s[:]) != hex64 {
		t.Fatal("hex nsec misparsed")
	}
	if _, err := NsecToSecret("not-a-nsec"); err == nil {
		t.Fatal("invalid nsec should error")
	}
}

// TestInteractiveSeedsWorldProfile drives `freehold login` end to end against a
// mock CP: challenge -> login (Set-Cookie) -> world summary; the local desire
// profile is seeded and the operator identity is persisted, all root-free.
func TestInteractiveSeedsWorldProfile(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // config.DefaultPath() under here

	const nsecHex = "b9100ce43b1deac5c5189f48bc6d97b287e122211d5f271488c0bcae2b350ad6"
	wantOpPK, err := crypto.PubkeyFromSecret(mustHex(t, nsecHex))
	if err != nil {
		t.Fatal(err)
	}
	const cpPubkey = "00aa11bb22cc33dd44ee55ff6677889900112233445566778899aabbccddeeff"

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/challenge":
			writeJSON(w, map[string]string{"nonce": "deadbeef"})
		case "/api/auth/login":
			w.Header().Set("Set-Cookie", "fh_session=tok123; Path=/; HttpOnly")
			writeJSON(w, map[string]string{"ok": "true"})
		case "/api/world":
			writeJSON(w, map[string]string{
				"relay_url":       "https://relay.example",
				"relay_ws_url":    "wss://relay.example",
				"relay_pubkey":    "11111",
				"cp_url":          srv.URL,
				"cp_pubkey":       cpPubkey,
				"operator_pubkey": wantOpPK,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Pre-write the CP address+pubkey; relay/operator fields are absent (a
	// fresh-box recovery seed must fill them from login, not from a lost box).
	pre := &config.Config{}
	pre.CPURL = srv.URL
	pre.CpPubkey = cpPubkey
	if err := pre.Save(config.DefaultPath()); err != nil {
		t.Fatal(err)
	}

	// Feed the nsec over piped stdin (no terminal, no l / c).
	pipeR, pipeW, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = pipeR
	go func() { pipeW.WriteString(nsecHex + "\n"); pipeW.Close() }()
	defer func() { os.Stdin = oldStdin }()

	if err := Interactive(); err != nil {
		t.Fatalf("login failed: %v", err)
	}

	got, err := config.Load(config.DefaultPath())
	if err != nil || got == nil {
		t.Fatalf("seeded config: %v", err)
	}
	if got.CPURL != srv.URL {
		t.Errorf("cp_url not seeded: %s", got.CPURL)
	}
	if got.RelayURL != "https://relay.example" {
		t.Errorf("relay_url not seeded: %s", got.RelayURL)
	}
	if got.RelayWsURL != "wss://relay.example" {
		t.Errorf("relay_ws_url not seeded: %s", got.RelayWsURL)
	}
	if got.OperatorPubkey != wantOpPK {
		t.Errorf("operator_pubkey not seeded from nsec: %s", got.OperatorPubkey)
	}
	if got.CpPubkey != cpPubkey {
		t.Errorf("cp_pubkey not seeded: %s", got.CpPubkey)
	}
	// Identity ledger persisted.
	if gotSec, err := SecretHex(); err != nil || gotSec != nsecHex {
		t.Errorf("operator identity not persisted: %q err=%v", gotSec, err)
	}
}

// TestLogoutClearsOnlyLocalLedger verifies logout drops the box's operator
// identity while touching nothing else.
func TestLogoutClearsOnlyLocalLedger(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	secret := [32]byte{}
	secret[0] = 5
	if _, err := Save(secret); err != nil {
		t.Fatal(err)
	}
	if err := Logout(); err != nil {
		t.Fatal(err)
	}
	if _, err := SecretHex(); err == nil {
		t.Fatal("operator identity should be gone after logout")
	}
	if _, err := os.Stat(filepath.Join(Dir(), identityFile)); !os.IsNotExist(err) {
		t.Fatal("identity file should not exist after logout")
	}
	// Logout is idempotent (missing file is fine).
	if err := Logout(); err != nil {
		t.Fatalf("repeat logout should be a no-op, got: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}
