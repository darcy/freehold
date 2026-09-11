package console

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freehold/contract/crypto"
	"freehold/contract/state"
	"freehold/contract/wire"
)

// signLoginEvent signs a NIP-98 kind-27235 login event (content = nonce).
func signLoginEvent(t *testing.T, secret []byte, nonce string, createdAt int64) (pubkey, sig string, tags [][]string) {
	t.Helper()
	tags = [][]string{}
	pk, _, sig, err := wire.SignEvent(secret, wire.KINDHTTPAuth, createdAt, tags, nonce)
	if err != nil {
		t.Fatal(err)
	}
	return pk, sig, tags
}

func adminSecret() []byte {
	s := make([]byte, 32)
	s[0] = 0x42
	return s
}

func testServer(t *testing.T, auth *Auth) (*Server, *state.StateStore) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// auth stays nil when passed nil (the loopback posture); non-nil only when
	// an admin whitelist is configured.
	s := &Server{Store: store, Auth: auth}
	return s, store
}

func TestAuthChallengeSessionPortal(t *testing.T) {
	a := NewAuth([]string{})
	nonce, err := a.IssueChallenge()
	if err != nil || nonce == "" {
		t.Fatalf("challenge: %v %q", err, nonce)
	}
	if !a.ConsumeChallenge(nonce) {
		t.Fatal("fresh challenge must consume")
	}
	if a.ConsumeChallenge(nonce) {
		t.Fatal("reused challenge must fail")
	}
	tok, err := a.IssueSession("pk1")
	if err != nil || tok == "" {
		t.Fatal(err)
	}
	if pk, ok := a.SessionIdentity(tok); !ok || pk != "pk1" {
		t.Fatalf("session identity: %v %v", pk, ok)
	}
	if _, ok := a.SessionIdentity("nope"); ok {
		t.Fatal("unknown session must fail")
	}
	pt, err := a.IssuePortal("pk1")
	if err != nil {
		t.Fatal(err)
	}
	if pk, ok := a.ConsumePortal(pt); !ok || pk != "pk1" {
		t.Fatalf("portal consume: %v %v", pk, ok)
	}
	if _, ok := a.ConsumePortal(pt); ok {
		t.Fatal("single-use portal must not consume twice")
	}
}

func TestCheckOrigin(t *testing.T) {
	req := func(origin string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	if err := checkOrigin(req(""), nil); err != nil {
		t.Fatal("missing origin must pass")
	}
	if err := checkOrigin(req("http://127.0.0.1:8080"), nil); err != nil {
		t.Fatal("loopback origin must pass")
	}
	if err := checkOrigin(req("http://localhost"), nil); err != nil {
		t.Fatal("localhost origin must pass")
	}
	if err := checkOrigin(req("http://evil.example"), nil); err == nil {
		t.Fatal("non-loopback origin must be refused")
	}
	pub := "https://cp.freehold.example"
	if err := checkOrigin(req("https://cp.freehold.example"), &pub); err != nil {
		t.Fatal("configured public origin must pass")
	}
	if err := checkOrigin(req("https://cp.freehold.example.evil.io"), &pub); err == nil {
		t.Fatal("a host that merely shares a prefix with the public origin must be refused")
	}
}

func TestLoginRoundtrip(t *testing.T) {
	sec := adminSecret()
	adminPK, _ := crypto.PubkeyFromSecret(sec)
	s, _ := testServer(t, NewAuth([]string{adminPK}))

	// 1. challenge.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/challenge", nil))
	if rec.Code != 200 {
		t.Fatalf("challenge: %d %s", rec.Code, rec.Body.String())
	}
	var chal struct {
		Nonce string `json:"nonce"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &chal)

	// 2. login with a valid NIP-98 event over the nonce.
	pk, sig, tags := signLoginEvent(t, sec, chal.Nonce, time.Now().Unix())
	body, _ := json.Marshal(map[string]interface{}{
		"nonce": chal.Nonce, "pubkey": pk, "created_at": time.Now().Unix(),
		"tags": tags, "sig": sig,
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
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Fatalf("cookie flags wrong: HttpOnly=%v SameSite=%v", c.HttpOnly, c.SameSite)
			}
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie issued")
	}

	// 3. a sessioned request hits /api/world (auth configured -> operator pubkey).
	r := httptest.NewRequest(http.MethodGet, "/api/world", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("world with session: %d %s", rec.Code, rec.Body.String())
	}
	var world struct {
		OperatorPubkey string `json:"operator_pubkey"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &world)
	if world.OperatorPubkey != pk {
		t.Fatalf("world operator pubkey = %q, want %q", world.OperatorPubkey, pk)
	}
}

// The public relay_url served on /api/world must be the DOMAIN edge
// (https://<relay_host>), NOT the internal LAN IP a box would try to adopt and
// fail to reach remotely. Console-internal dials keep the LAN relay_url.
func TestWorldServesPublicRelayURL(t *testing.T) {
	sec := adminSecret()
	adminPK, _ := crypto.PubkeyFromSecret(sec)
	s, store := testServer(t, NewAuth([]string{adminPK}))
	relayHost := "relay.librem.freehold.technology"
	relayLan := "http://192.168.30.249:3000"
	if err := store.SetRelayHost(&relayHost); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRelayURL(&relayLan); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	// login dance -> session cookie.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/challenge", nil))
	if rec.Code != 200 {
		t.Fatalf("challenge: %d", rec.Code)
	}
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
		RelayURL   string `json:"relay_url"`
		RelayWSURL string `json:"relay_ws_url"`
		RelayHost  string `json:"relay_host"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &world)
	if world.RelayURL != "https://"+relayHost {
		t.Fatalf("served relay_url = %q, want https://%s (the public domain, not the LAN dial)", world.RelayURL, relayHost)
	}
	if world.RelayWSURL != "wss://"+relayHost {
		t.Fatalf("served relay_ws_url = %q, want wss://%s", world.RelayWSURL, relayHost)
	}
	if world.RelayHost != relayHost {
		t.Fatalf("relay_host = %q, want %q", world.RelayHost, relayHost)
	}
}

func TestLoginRejects(t *testing.T) {
	sec := adminSecret()
	adminPK, _ := crypto.PubkeyFromSecret(sec)
	s, _ := testServer(t, NewAuth([]string{adminPK}))
	// A stale signature timestamp is rejected.
	nonce := "not-consumed"
	pk, sig, tags := signLoginEvent(t, sec, nonce, time.Now().Unix()-200)
	body, _ := json.Marshal(map[string]interface{}{
		"nonce": nonce, "pubkey": pk, "created_at": time.Now().Unix() - 200, "tags": tags, "sig": sig,
	})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale login must 401, got %d %s", rec.Code, rec.Body.String())
	}

	// A non-admin pubkey is forbidden.
	other := make([]byte, 32)
	other[0] = 0x99
	otherPK, _ := crypto.PubkeyFromSecret(other)
	rec2 := httptest.NewRecorder()
	_ = rec2
	// Use the challenge flow so the nonce is valid.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/challenge", nil))
	var chal struct {
		Nonce string `json:"nonce"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &chal)
	nonce = chal.Nonce
	pk, sig, tags = signLoginEvent(t, other, nonce, time.Now().Unix())
	body, _ = json.Marshal(map[string]interface{}{
		"nonce": nonce, "pubkey": otherPK, "created_at": time.Now().Unix(), "tags": tags, "sig": sig,
	})
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin login must 403, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestPortalSingleUse(t *testing.T) {
	sec := adminSecret()
	adminPK, _ := crypto.PubkeyFromSecret(sec)
	s, _ := testServer(t, NewAuth([]string{adminPK}))

	// Login to get a session.
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
	session := ""
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c.Value
		}
	}

	// Mint a portal token with the session.
	r := httptest.NewRequest(http.MethodPost, "/api/auth/portal", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	var portal struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &portal)
	if portal.Token == "" {
		t.Fatalf("no portal token: %s", rec.Body.String())
	}

	// Land once -> a fresh session cookie + 302.
	r2 := httptest.NewRequest(http.MethodGet, "/api/auth/portal/"+portal.Token, nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, r2)
	if rec.Code != http.StatusFound {
		t.Fatalf("portal land: %d", rec.Code)
	}
	foundCookie := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			foundCookie = true
		}
	}
	if !foundCookie {
		t.Fatal("portal land must issue a fresh session")
	}

	// Second use -> consumed (404).
	r3 := httptest.NewRequest(http.MethodGet, "/api/auth/portal/"+portal.Token, nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, r3)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reused portal must 404, got %d", rec.Code)
	}
}

func TestWorldRequiresAuthWhenConfigured(t *testing.T) {
	s, _ := testServer(t, nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/world", nil))
	// Auth NOT configured -> the portal/world routes 404.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("world with no auth configured must 404, got %d", rec.Code)
	}
}

func TestOverviewListsRunners(t *testing.T) {
	sec := adminSecret()
	adminPK, _ := crypto.PubkeyFromSecret(sec)
	s, store := testServer(t, NewAuth([]string{adminPK}))
	cs := make([]byte, 32)
	cs[0] = 0x11
	cpk, _ := crypto.PubkeyFromSecret(cs)
	s.ConsolePubkey = cpk
	store.InsertRunner("box", state.RunnerRecord{NostrPubkey: strings.Repeat("a", 64), EncPubkey: strings.Repeat("b", 64), Status: state.RunnerActive, PackageDir: t.TempDir(), CreatedAt: 1})
	_ = store.Save()

	// Login + session.
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
	session := ""
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c.Value
		}
	}

	r := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body.String())
	}
	var ov struct {
		ConsolePubkey string `json:"console_pubkey"`
		Runners       []struct {
			Name string `json:"name"`
		} `json:"runners"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ov)
	if len(ov.Runners) != 1 || ov.Runners[0].Name != "box" {
		t.Fatalf("overview runners: %+v", ov.Runners)
	}
	if ov.ConsolePubkey == "" {
		t.Fatal("overview must carry the console pubkey")
	}
}

func TestLoopbackBindGuard(t *testing.T) {
	if err := ValidateLoopbackBind("127.0.0.1:8080"); err != nil {
		t.Fatalf("loopback bind must pass: %v", err)
	}
	if err := ValidateLoopbackBind("0.0.0.0:8080"); err == nil {
		t.Fatal("non-loopback bind must be refused without auth")
	}
}

var _ = hex.EncodeToString
