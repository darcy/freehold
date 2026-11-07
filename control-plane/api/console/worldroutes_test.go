package console

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/api/cpbuild"
)

// loginWorldSession drives the challenge/login dance against s and returns the
// session cookie (the same flow TestWorldBuildGating exercises).
func loginWorldSession(t *testing.T, s *Server, sec []byte) string {
	t.Helper()
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
	var cookie string
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie")
	}
	return cookie
}

// The operator world routes are session-scoped (like /api/world-build) and
// refuse loudly when their engine/coords are absent.
func TestWorldRoutesGating(t *testing.T) {
	sec := adminSecret()
	adminPK, _ := crypto.PubkeyFromSecret(sec)
	s, _ := testServer(t, NewAuth([]string{adminPK}, ""))
	post := func(path string, body []byte) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		return rec.Code
	}
	// Without a session -> unauthorized.
	for _, path := range []string{"/api/world-exec", "/api/world-migrate", "/api/world-door"} {
		if code := post(path, nil); code != 401 {
			t.Fatalf("%s without a session must be 401, got %d", path, code)
		}
	}
	cookie := loginWorldSession(t, s, sec)
	authed := func(path string, body []byte) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		return rec.Code
	}
	// With a session but no build engine bound -> 503 (run bootstrap).
	if code := authed("/api/world-exec", []byte(`{"cmd":"df -h"}`)); code != 503 {
		t.Fatalf("world-exec with no builder must be 503, got %d", code)
	}
	if code := authed("/api/world-door", []byte(`{"action":"authorize","pubkey":"k"}`)); code != 503 {
		t.Fatalf("world-door with no builder must be 503, got %d", code)
	}
	// world-migrate with no agent-tools coords recorded -> 503.
	if code := authed("/api/world-migrate", nil); code != 503 {
		t.Fatalf("world-migrate with no coords must be 503, got %d", code)
	}
}

// TestWorldMigrateProxy proves the console proxies world_migrate into the
// agent-tools serve, signed as the console identity — the roster-free
// migration trigger whose execution must stay in the serve's process.
func TestWorldMigrateProxy(t *testing.T) {
	sec := adminSecret()
	adminPK, _ := crypto.PubkeyFromSecret(sec)
	s, store := testServer(t, NewAuth([]string{adminPK}, ""))
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x33
	consolePK, err := crypto.PubkeyFromSecret(consoleSec)
	if err != nil {
		t.Fatal(err)
	}
	s.ConsoleSecret = consoleSec
	s.ConsolePubkey = consolePK

	// A stand-in agent-tools serve: it verifies the console's signed call and
	// answers world_migrate with a result envelope.
	audSec := make([]byte, 32)
	audSec[0] = 0x21
	audPK, err := crypto.PubkeyFromSecret(audSec)
	if err != nil {
		t.Fatal(err)
	}
	sawCaller := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		raw := string(body)
		ts64, _ := strconv.ParseInt(r.Header.Get(agenttools.TSHeader), 10, 64)
		if _, verr := agenttools.VerifyRequest([]string{consolePK}, audPK,
			r.Header.Get(agenttools.PubkeyHeader), r.Header.Get(agenttools.SigHeader),
			strconv.FormatInt(ts64, 10), raw); verr != nil {
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"` + verr.Error() + `"}}`))
			return
		}
		sawCaller = r.Header.Get(agenttools.PubkeyHeader)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"001-ok"}]}}`))
	}))
	defer srv.Close()

	if err := store.SetAgentToolsURL(&srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAgentToolsPubkey(&audPK); err != nil {
		t.Fatal(err)
	}

	cookie := loginWorldSession(t, s, sec)
	r := httptest.NewRequest(http.MethodPost, "/api/world-migrate", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("world-migrate via proxy: %d %s", rec.Code, rec.Body.String())
	}
	if sawCaller != consolePK {
		t.Fatalf("proxy must sign as the console identity, signed as %q", sawCaller)
	}
	var v struct {
		Report string `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Report != "001-ok" {
		t.Fatalf("world-migrate report: %q err=%v", rec.Body.String(), err)
	}
}

// TestApplyBuildCoordsReseatsAndPreservesRuntime: the box's shipped coords
// re-seat the build executor (an explicit pangolin clear included) while the
// console's own state dir + agent-tools bindings + serve-flag relay overrides
// stay (they belong to this process, not the box).
func TestApplyBuildCoordsReseatsAndPreservesRuntime(t *testing.T) {
	base := cpbuild.NewSpec(cpbuild.Coords{
		Name: "librem", RelayHost: "relay.example", RelayIP: "192.168.30.10",
		RelayURL: "http://relay.example:3000", RelayAuthURL: "https://relay.example",
		CpHost: "cp.example", CpIP: "192.168.30.11", ProxyIP: "192.168.30.8",
		Pangolin: &config.PangolinCoords{Org: "freehold", Site: "librem", Domain: "librem.example.test", DNSIP: "203.0.113.9"},
	}, make([]byte, 32), "aud")
	base.StateDir = "/srv/data/cp/control-plane"
	base.AgentIdentityDir = "/srv/data/cp/agent-tools"
	apps := &agenttools.AppsStore{}
	base.Apps = apps
	s := &Server{StateDir: "/srv/data/cp/control-plane", Builder: base}

	body := `{"coords":{"name":"librem","relay_host":"relay.example","cp_host":"cp.example","proxy_ip":"192.168.30.9","pangolin":null}}`
	r := httptest.NewRequest(http.MethodPost, "/api/world-build", strings.NewReader(body))
	if err := s.applyBuildCoords(r); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if base.ProxyIP != "192.168.30.9" {
		t.Fatalf("coords not applied: proxy %q", base.ProxyIP)
	}
	if base.Pangolin != nil {
		t.Fatalf("an explicit pangolin clear must stick: %+v", base.Pangolin)
	}
	if base.StateDir != "/srv/data/cp/control-plane" || base.AgentIdentityDir != "/srv/data/cp/agent-tools" || base.Apps != apps {
		t.Fatal("the console's runtime bindings must survive the apply")
	}
	if base.RelayURL != "http://relay.example:3000" || base.RelayAuthURL != "https://relay.example" {
		t.Fatalf("the serve-flag relay overrides must survive: %+v", base)
	}
}
