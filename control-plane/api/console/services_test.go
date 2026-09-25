package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freehold/contract/crypto"
	"freehold/control-plane/state"
)

// TestWorldServiceRows registers live local probes for each service kind and
// verifies worldServiceRows reports the co-located health (k3s = any HTTP
// answer, litellm = 2xx, caddy = any answer). The probes use real httptest
// servers so no service URL is ever guessed blind.
func TestWorldServiceRows(t *testing.T) {
	ln := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ln.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // k3s's normal "no client cert" answer
	}))
	defer tls.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	snap := state.ControlPlaneState{Services: map[string]state.WorldService{
		"k3s":     {Kind: "k3s", URL: tls.URL},    // any answer incl 401 = up
		"litellm": {Kind: "litellm", URL: ln.URL}, // 200 = up
		"caddy":   {Kind: "caddy", URL: ln.URL},   // any answer = up
	}}
	rows := worldServiceRows(snap)
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	for _, r := range rows {
		if !r.Up {
			t.Errorf("service %s should be up, got detail=%q", r.Name, r.Detail)
		}
	}

	// A litellm 5xx must read as DOWN (gateway health requires 2xx).
	snap.Services["litellm"] = state.WorldService{Kind: "litellm", URL: bad.URL}
	for _, r := range worldServiceRows(snap) {
		if r.Name == "litellm" && r.Up {
			t.Error("litellm 500 should report down")
		}
	}
}

// TestWorldRelayRowProbesLanDial: the /api/world relay row must probe the relay
// the way the CP guest dials it — the LAN dial by HOSTNAME (the /etc/hosts pin,
// re-read from the live DHCP lease every converge) — never the public https
// origin, which on the CP guest resolves through that same pin to the raw relay
// LXC where only buzz :3000 listens (TLS lives at the proxy edge). The listener
// here is plain HTTP: the LAN dial answers, an https dial to it cannot — an
// up=true row proves the dial went LAN-form.
func TestWorldRelayRowProbesLanDial(t *testing.T) {
	buzz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // any answer = up (buzz 404s on /)
	}))
	defer buzz.Close()
	host := strings.TrimPrefix(buzz.URL, "http://") // 127.0.0.1:<port>

	sec := adminSecret()
	adminPK, _ := crypto.PubkeyFromSecret(sec)
	s, store := testServer(t, NewAuth([]string{adminPK}))
	rh, ru := host, "http://"+host
	if err := store.SetRelayHost(&rh); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRelayURL(&ru); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	// login dance -> session cookie (the auth_test.go pattern).
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/challenge", nil))
	var chal struct {
		Nonce string `json:"nonce"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &chal)
	pk, sig, tags := signLoginEvent(t, sec, chal.Nonce, time.Now().Unix())
	body, _ := json.Marshal(map[string]interface{}{
		"nonce": chal.Nonce, "pubkey": pk, "created_at": time.Now().Unix(), "tags": tags, "sig": sig,
	})
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	var cookie string
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie")
	}

	r := httptest.NewRequest(http.MethodGet, "/api/world", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("world: %d %s", rec.Code, rec.Body.String())
	}
	var world struct {
		Services []WorldServiceJSON `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &world); err != nil {
		t.Fatal(err)
	}
	for _, svc := range world.Services {
		if svc.Kind == "relay" && !svc.Up {
			t.Fatalf("relay row should be up via the LAN dial (detail %q), got up=false", svc.Detail)
		}
	}
}
