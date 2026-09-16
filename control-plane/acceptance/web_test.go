package acceptance

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"freehold/contract/state"
	"freehold/contract/wire"
	cpconsole "freehold/control-plane/api/console"
)

const agentB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// bootConsole serves the Go console on loopback (the real http.Handler).
func bootConsole(t *testing.T, cpDir string, auth *cpconsole.Auth) (string, *state.StateStore, string) {
	t.Helper()
	store := openStore(t, cpDir)
	secret, pub, err := cpconsole.EnsureConsoleIdentity(cpDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := &cpconsole.Server{
		Store: store, ConsoleSecret: secret, ConsolePubkey: pub,
		Auth: auth, StateDir: cpDir,
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts.URL, store, pub
}

// req does one HTTP call and returns status, body, and response headers.
func req(t *testing.T, method, url, body, cookie, origin string) (int, []byte, http.Header) {
	t.Helper()
	r, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func postJSON(t *testing.T, base, path, body string) (int, []byte) {
	t.Helper()
	s, b, _ := req(t, http.MethodPost, base+path, body, "", "")
	return s, b
}

func getJSON(t *testing.T, base, path, cookie, origin string) (int, map[string]interface{}) {
	t.Helper()
	s, b, _ := req(t, http.MethodGet, base+path, "", cookie, origin)
	var out map[string]interface{}
	_ = json.Unmarshal(b, &out)
	return s, out
}

// login performs the NIP-98 challenge -> sign -> session flow.
func login(t *testing.T, base string, secret []byte) string {
	t.Helper()
	s, b, _ := req(t, http.MethodGet, base+"/api/auth/challenge", "", "", "")
	if s != http.StatusOK {
		t.Fatalf("challenge: %d %s", s, b)
	}
	var ch struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(b, &ch); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Unix()
	tags := [][]string{{"u", base}, {"method", "login"}}
	pub, _, sig, err := wire.SignEvent(secret, wire.KINDHTTPAuth, ts, tags, ch.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]interface{}{
		"nonce": ch.Nonce, "pubkey": pub, "created_at": ts, "tags": tags, "sig": sig,
	})
	s, b, hdr := req(t, http.MethodPost, base+"/api/auth/login", string(body), "", "")
	if s != http.StatusOK {
		t.Fatalf("login: %d %s", s, b)
	}
	for _, c := range hdr.Values("Set-Cookie") {
		if strings.HasPrefix(c, "fh_session=") {
			return strings.SplitN(c, ";", 2)[0]
		}
	}
	t.Fatal("no session cookie")
	return ""
}

func runnerByName(ov map[string]interface{}, name string) map[string]interface{} {
	arr, _ := ov["runners"].([]interface{})
	for _, r := range arr {
		if m, ok := r.(map[string]interface{}); ok && m["name"] == name {
			return m
		}
	}
	return nil
}

func assertNoPlaintext(t *testing.T, v interface{}, secrets ...string) {
	t.Helper()
	dump, _ := json.Marshal(v)
	for _, s := range secrets {
		if strings.Contains(string(dump), s) {
			t.Fatalf("plaintext credential leaked into the API: %s", s)
		}
	}
}

func provisionViaHTTP(t *testing.T, base, name, kind, address, secret, runnerDir string) {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"name": name, "kind": kind, "address": address,
		"secret": secret, "runner_dir": runnerDir,
	})
	s, b := postJSON(t, base, "/api/provision", string(body))
	if s != http.StatusOK {
		t.Fatalf("provision: %d %s", s, b)
	}
}

func TestWorldServesOperatorSeedAfterLogin(t *testing.T) {
	base := t.TempDir()
	cpDir := filepath.Join(base, "cp")
	store := openStore(t, cpDir)
	relayURL := "https://relay.example"
	relayPub := "9797abc9797abc9797abc9797abc9797abc9797abc9797abc9797abc9797abc"
	atURL := "http://10.0.0.5:8089"
	atPub := "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	if err := store.SetRelayURL(&relayURL); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRelayPubkey(&relayPub); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAgentToolsURL(&atURL); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAgentToolsPubkey(&atPub); err != nil {
		t.Fatal(err)
	}

	adminSecret := make([]byte, 32)
	adminSecret[0] = 0x42
	adminPub, err := pubkeyOf(adminSecret)
	if err != nil {
		t.Fatal(err)
	}
	url, _, consolePub := bootConsole(t, cpDir, cpconsole.NewAuth([]string{adminPub}))

	// Anon world fails closed.
	if s, _ := getJSON(t, url, "/api/world", "", ""); s != http.StatusUnauthorized && s != http.StatusForbidden {
		t.Fatalf("anon world status = %d", s)
	}

	cookie := login(t, url, adminSecret)
	s, w := getJSON(t, url, "/api/world", cookie, "")
	if s != http.StatusOK {
		t.Fatalf("world: %d", s)
	}
	if w["relay_url"] != relayURL {
		t.Fatalf("relay_url = %v", w["relay_url"])
	}
	if w["relay_ws_url"] != "wss://relay.example" {
		t.Fatalf("relay_ws_url = %v", w["relay_ws_url"])
	}
	if w["cp_pubkey"] != consolePub {
		t.Fatalf("cp_pubkey = %v", w["cp_pubkey"])
	}
	if w["operator_pubkey"] != adminPub {
		t.Fatalf("operator_pubkey = %v", w["operator_pubkey"])
	}
	if _, ok := w["relay_pubkey"].(string); !ok {
		t.Fatal("relay_pubkey must be a string")
	}
	if w["agent_tools_url"] != atURL {
		t.Fatalf("agent_tools_url = %v", w["agent_tools_url"])
	}
	if w["agent_tools_pubkey"] != atPub {
		t.Fatalf("agent_tools_pubkey = %v", w["agent_tools_pubkey"])
	}
}

func TestProvisionRotateGrantRevokeLifecycle(t *testing.T) {
	base := t.TempDir()
	url, _, consolePub := bootConsole(t, filepath.Join(base, "cp"), nil)
	pkgDir := filepath.Join(base, "runner", "pg")

	provisionViaHTTP(t, url, "pg", "ssh", "10.0.0.5", "sekrit-pg-99", pkgDir)

	_, ov := getJSON(t, url, "/api/overview", "", "")
	assertNoPlaintext(t, ov, "sekrit-pg-99")
	pg := runnerByName(ov, "pg")
	if pg["status"] != "active" {
		t.Fatalf("status = %v", pg["status"])
	}
	sec := pg["secret"].(map[string]interface{})
	if sec["kind"] != "ssh" || sec["address"] != "10.0.0.5" {
		t.Fatalf("secret = %v", sec)
	}
	grants := pg["grants"].([]interface{})
	if len(grants) != 1 || grants[0] != consolePub {
		t.Fatalf("grants = %v", grants)
	}
	pkg := mustLoadPackage(t, pkgDir)
	if len(pkg.Grants) != 1 || pkg.Grants[0] != consolePub {
		t.Fatalf("shipped grants = %v", pkg.Grants)
	}

	// rotate
	if s, b := postJSON(t, url, "/api/rotate", `{"name":"pg","secret":"sekrit-pg-100"}`); s != http.StatusOK {
		t.Fatalf("rotate: %d %s", s, b)
	}
	_, ov2 := getJSON(t, url, "/api/overview", "", "")
	assertNoPlaintext(t, ov2, "sekrit-pg-99", "sekrit-pg-100")
	if _, ok := runnerByName(ov2, "pg")["secret"].(map[string]interface{})["rotated_at"].(float64); !ok {
		t.Fatal("rotate must set rotated_at")
	}

	// grant another agent, then revoke it
	if s, b := postJSON(t, url, "/api/grant", `{"name":"pg","pubkey":"`+agentB+`"}`); s != http.StatusOK {
		t.Fatalf("grant: %d %s", s, b)
	}
	_, ov3 := getJSON(t, url, "/api/overview", "", "")
	if !grantsContain(runnerByName(ov3, "pg"), agentB) {
		t.Fatal("overview must show the new grant")
	}
	if pkg := mustLoadPackage(t, pkgDir); len(pkg.Grants) != 2 {
		t.Fatalf("shipped grants = %v", pkg.Grants)
	}
	if s, _ := postJSON(t, url, "/api/revoke-grant", `{"name":"pg","pubkey":"`+agentB+`"}`); s != http.StatusOK {
		t.Fatal("revoke-grant failed")
	}
	_, ov4 := getJSON(t, url, "/api/overview", "", "")
	if grantsContain(runnerByName(ov4, "pg"), agentB) {
		t.Fatal("revoked grant must be gone")
	}

	// revoke
	if s, _ := postJSON(t, url, "/api/revoke", `{"name":"pg"}`); s != http.StatusOK {
		t.Fatal("revoke failed")
	}
	_, ov5 := getJSON(t, url, "/api/overview", "", "")
	pg5 := runnerByName(ov5, "pg")
	if pg5["status"] != "revoked" {
		t.Fatalf("status = %v", pg5["status"])
	}
	if pg5["grants"] != nil {
		t.Fatalf("revoked runner must report grants == null: %v", pg5["grants"])
	}
	if s, _ := postJSON(t, url, "/api/rotate", `{"name":"pg","secret":"nope"}`); s != http.StatusConflict {
		t.Fatalf("rotate revoked should 409, got %d", s)
	}
}

func TestLiveReadinessProbeViaConsole(t *testing.T) {
	base := t.TempDir()
	url, _, _ := bootConsole(t, filepath.Join(base, "cp"), nil)
	pkgDir := filepath.Join(base, "runner", "pv")
	provisionViaHTTP(t, url, "pv", "vultr", "http://127.0.0.1:1", "vultr-key-7", pkgDir)

	raddr := startRunner(t, pkgDir)
	body, _ := json.Marshal(map[string]interface{}{"name": "pv", "addr": raddr})
	if s, b := postJSON(t, url, "/api/runner-addr", string(body)); s != http.StatusOK {
		t.Fatalf("runner-addr: %d %s", s, b)
	}

	_, ov := getJSON(t, url, "/api/overview", "", "")
	ready := runnerByName(ov, "pv")["readiness"].(map[string]interface{})
	if ready["local"] != "green" {
		t.Fatalf("local must be green: %v", ready)
	}
	svc, _ := ready["pv"].(string)
	if !strings.HasPrefix(svc, "red") && !strings.HasPrefix(svc, "yellow") {
		t.Fatalf("service target should read degraded: %v", ready)
	}
}

func TestUngrantedConsoleCannotReadRunner(t *testing.T) {
	base := t.TempDir()
	url, _, _ := bootConsole(t, filepath.Join(base, "cp"), nil)
	pkgDir := filepath.Join(base, "runner", "locked")
	provisionViaHTTP(t, url, "locked", "b2", "http://127.0.0.1:2", "b2-key-42", pkgDir)

	// Strip the console from the shipped grants (a pre-UI runner).
	pkg := mustLoadPackage(t, pkgDir)
	pkg.Grants = nil
	if err := pkg.WriteToDir(pkgDir); err != nil {
		t.Fatal(err)
	}
	raddr := startRunner(t, pkgDir)
	body, _ := json.Marshal(map[string]interface{}{"name": "locked", "addr": raddr})
	if s, b := postJSON(t, url, "/api/runner-addr", string(body)); s != http.StatusOK {
		t.Fatalf("runner-addr: %d %s", s, b)
	}

	_, ov := getJSON(t, url, "/api/overview", "", "")
	ready := runnerByName(ov, "locked")["readiness"].(map[string]interface{})
	note, _ := ready["note"].(string)
	if !strings.Contains(note, "not granted") {
		t.Fatalf("console must not read an ungranted runner: %v", ov)
	}
}

func TestNonLoopbackOriginRefused(t *testing.T) {
	base := t.TempDir()
	url, _, _ := bootConsole(t, filepath.Join(base, "cp"), nil)
	provisionViaHTTP(t, url, "x", "ssh", "1.2.3.4", "x", filepath.Join(base, "runner", "x"))

	if s, _, _ := req(t, http.MethodGet, url+"/api/overview", "", "", "http://evil.example"); s != http.StatusForbidden {
		t.Fatalf("evil origin read: %d", s)
	}
	if s, _, _ := req(t, http.MethodPost, url+"/api/revoke", `{"name":"x"}`, "", "http://evil.example"); s != http.StatusForbidden {
		t.Fatalf("evil origin write: %d", s)
	}
	for _, origin := range []string{"http://127.0.0.1:9999", "http://localhost", "http://[::1]:8080"} {
		if s, _, _ := req(t, http.MethodGet, url+"/api/overview", "", "", origin); s != http.StatusOK {
			t.Fatalf("loopback origin %s: %d", origin, s)
		}
	}
}

func TestAdminPageEscapesRemoteReadinessText(t *testing.T) {
	base := t.TempDir()
	url, _, _ := bootConsole(t, filepath.Join(base, "cp"), nil)
	s, b, _ := req(t, http.MethodGet, url+"/", "", "", "")
	if s != http.StatusOK {
		t.Fatalf("index: %d", s)
	}
	html := string(b)
	for _, want := range []string{
		"function esc(", "esc(t)", "chip(s)", "esc(r.name)", "esc(r.mcp_addr)",
		"revoked — secrets.json removed (B3)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("admin page missing %q", want)
		}
	}
}

func TestTeardownClearsCPManagedScopeAfterLogin(t *testing.T) {
	base := t.TempDir()
	cpDir := filepath.Join(base, "cp")
	store := openStore(t, cpDir)
	store.InsertRunner("pg", state.RunnerRecord{
		NostrPubkey: "11", EncPubkey: "22", Status: state.RunnerActive,
		PackageDir: filepath.Join(cpDir, "runner/pg"), CreatedAt: 1,
	})
	store.InsertSecret("pg", state.SecretRecord{Runner: "pg", Kind: "ssh", Address: "10.0.0.5", CiphertextHex: "aa", CreatedAt: 1})
	ch := "blog"
	store.InsertAgent("blog", state.AgentRecord{Pubkey: "33", CreatedAt: 1, Channel: &ch})
	store.InsertDNS("relay", state.DnsRecord{IP: "10.0.0.1", Source: "record_lxc relay", CreatedAt: 1})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	adminSecret := make([]byte, 32)
	adminSecret[0] = 0x42
	adminPub, _ := pubkeyOf(adminSecret)
	url, _, _ := bootConsole(t, cpDir, cpconsole.NewAuth([]string{adminPub}))

	if s, _, _ := req(t, http.MethodPost, url+"/api/teardown", "", "", ""); s != http.StatusUnauthorized && s != http.StatusForbidden {
		t.Fatalf("anon teardown status = %d", s)
	}

	cookie := login(t, url, adminSecret)
	s, b, _ := req(t, http.MethodPost, url+"/api/teardown", "", cookie, "")
	if s != http.StatusOK {
		t.Fatalf("teardown: %d %s", s, b)
	}
	var v map[string]interface{}
	_ = json.Unmarshal(b, &v)
	if v["runners_removed"].(float64) != 1 || v["agents_removed"].(float64) != 1 || v["dns_removed"].(float64) != 1 {
		t.Fatalf("teardown counts: %v", v)
	}

	_, ov := getJSON(t, url, "/api/overview", cookie, "")
	if arr, _ := ov["runners"].([]interface{}); len(arr) != 0 {
		t.Fatalf("overview must be empty: %v", ov["runners"])
	}
}

// ---- small helpers ----

func mustLoadPackage(t *testing.T, dir string) *wire.SecretPackage {
	t.Helper()
	pkg, err := wire.Load(dir)
	if err != nil {
		t.Fatalf("load package: %v", err)
	}
	return pkg
}

func grantsContain(r map[string]interface{}, pk string) bool {
	arr, _ := r["grants"].([]interface{})
	for _, g := range arr {
		if g == pk {
			return true
		}
	}
	return false
}
