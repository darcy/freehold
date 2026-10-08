package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/control-plane/state"
)

// ---- fixture ----

type memberFixture struct {
	s           *Server
	opSec       []byte
	opPK        string
	memberSec   []byte
	memberPK    string
	relaySec    []byte
	relayPK     string
	membersFile string
	relay       *httptest.Server
}

// newMemberFixture builds a console with the member tier enabled, the relay
// coords pointed at a fake /query serving ONE kind-13534 listing that names
// the fixture's own member, and a public origin for cookie/next handling.
func newMemberFixture(t *testing.T) *memberFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &memberFixture{
		opSec:     secret32(1),
		memberSec: secret32(2),
		relaySec:  secret32(3),
	}
	f.opPK = pkOf(t, f.opSec)
	f.memberPK = pkOf(t, f.memberSec)
	f.relayPK = pkOf(t, f.relaySec)
	f.relay = fakeRelayQuery(t, []map[string]interface{}{membershipListEvent(t, f.relaySec, 100, f.memberPK)})
	if err := store.SetRelayURL(&f.relay.URL); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRelayPubkey(&f.relayPK); err != nil {
		t.Fatal(err)
	}
	f.membersFile = filepath.Join(dir, "members.json")
	pub := "https://cp.example.com"
	f.s = &Server{
		Store:         store,
		ConsoleSecret: secret32(4),
		Auth:          NewAuth([]string{f.opPK}, filepath.Join(dir, "sessions.json")),
		Members:       NewMembers(f.membersFile),
		PublicOrigin:  &pub,
	}
	return f
}

func secret32(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func pkOf(t *testing.T, sec []byte) string {
	t.Helper()
	p, err := crypto.PubkeyFromSecret(sec)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeRelayQuery answers /query with a fixed event set — the client-side
// author check + signature verification in IsCommunityMemberAuth make the
// fake's filter-blindness harmless.
func fakeRelayQuery(t *testing.T, events []map[string]interface{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(events)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// membershipListEvent is the relay's NIP-43 list, RELAY-SIGNED, members
// riding `member` tags — the shape the pinned buzz relay serves.
func membershipListEvent(t *testing.T, relaySec []byte, ts int64, members ...string) map[string]interface{} {
	t.Helper()
	tags := [][]string{}
	for _, m := range members {
		tags = append(tags, []string{"member", m, "member"})
	}
	return signEventMap(t, relaySec, wire.KINDNip43Membership, ts, tags, "")
}

func signEventMap(t *testing.T, sec []byte, kind uint32, ts int64, tags [][]string, content string) map[string]interface{} {
	t.Helper()
	pk, id, sig, err := wire.SignEvent(sec, kind, ts, tags, content)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]interface{}{
		"id": id, "pubkey": pk, "created_at": ts, "kind": kind,
		"tags": tags, "content": content, "sig": sig,
	}
}

// loginAs runs the full member login: challenge → NIP-07-shaped kind-27235
// event → POST.
func (f *memberFixture) loginAs(t *testing.T, sec []byte, next string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/member/challenge", nil))
	if rec.Code != 200 {
		t.Fatalf("challenge: %d %s", rec.Code, rec.Body.String())
	}
	var ch struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ch); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Unix()
	tags := [][]string{{"url", "https://cp.example.com/auth"}, {"method", "GET"}}
	pkHex, _, sig, err := wire.SignEvent(sec, wire.KINDHTTPAuth, ts, tags, ch.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]interface{}{
		"nonce": ch.Nonce, "pubkey": pkHex, "created_at": ts,
		"tags": tags, "sig": sig, "next": next,
	})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/auth/member/login", strings.NewReader(string(body))))
	return rec
}

func cookieOf(t *testing.T, rec *httptest.ResponseRecorder, name string) string {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func opCookie(t *testing.T, f *memberFixture) *http.Cookie {
	t.Helper()
	tok, err := f.s.Auth.IssueSession(f.opPK)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: tok}
}

// ---- tests ----

// TestMemberNostrLoginGate pins the gate's admission rules: a listed member
// logs in and gets a cookie, a non-member is refused, a forged signature is
// refused, and a relay outage degrades per the cache — a CACHED member stays
// in (the grace window), an UNCACHED one fails closed with 503, never 200.
func TestMemberNostrLoginGate(t *testing.T) {
	f := newMemberFixture(t)
	rec := f.loginAs(t, f.memberSec, "")
	if rec.Code != 200 {
		t.Fatalf("member login: %d %s", rec.Code, rec.Body.String())
	}
	if cookieOf(t, rec, memberCookie) == "" {
		t.Fatal("no member cookie on a successful login")
	}

	// A pubkey the relay does not list is refused — the gate is the relay's
	// membership list, not "anyone with a Nostr extension".
	rec = f.loginAs(t, secret32(5), "")
	if rec.Code != 403 {
		t.Fatalf("non-member login must be 403, got %d %s", rec.Code, rec.Body.String())
	}

	// A forged signature is refused on a REAL challenge: a live nonce, then
	// a garbage sig — verifyNIP98 must be what rejects it. The relay is
	// closed for this fixture: if sig verification ever stopped rejecting,
	// this flow would fall through to the (unreachable) membership check and
	// 503, failing the expected signature-failure 401.
	f2 := newMemberFixture(t)
	f2.relay.Close()
	rec = httptest.NewRecorder()
	f2.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/member/challenge", nil))
	if rec.Code != 200 {
		t.Fatalf("challenge: %d %s", rec.Code, rec.Body.String())
	}
	var ch struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ch); err != nil {
		t.Fatal(err)
	}
	body := `{"nonce":"` + ch.Nonce + `","pubkey":"` + f2.memberPK + `","created_at":` +
		strconv.FormatInt(time.Now().Unix(), 10) +
		`,"tags":[["url","https://cp.example.com/auth"],["method","GET"]],"sig":"deadbeef"}`
	rec = httptest.NewRecorder()
	f2.s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/auth/member/login", strings.NewReader(body)))
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "signature verification failed") {
		t.Fatalf("forged sig must be 401 (signature failure), got %d %s", rec.Code, rec.Body.String())
	}

	// Relay outage: the cached member stays admitted (grace), an uncached
	// one fails closed.
	f.relay.Close()
	rec = f.loginAs(t, f.memberSec, "")
	if rec.Code != 200 {
		t.Fatalf("cached member must survive a relay outage, got %d %s", rec.Code, rec.Body.String())
	}
	rec = f.loginAs(t, secret32(6), "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("uncached login on a dead relay must be 503, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestForwardAuthVerify pins the gate endpoint: a member session admits,
// an operator session admits, nobody admits.
func TestForwardAuthVerify(t *testing.T) {
	f := newMemberFixture(t)
	rec := f.loginAs(t, f.memberSec, "")
	if rec.Code != 200 {
		t.Fatalf("member login: %d %s", rec.Code, rec.Body.String())
	}
	mtok := cookieOf(t, rec, memberCookie)
	if mtok == "" {
		t.Fatal("no member cookie")
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: mtok})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("member verify: %d", rec.Code)
	}

	// The operator passes the gate on their existing session.
	req = httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.AddCookie(opCookie(t, f))
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("operator verify: %d", rec.Code)
	}

	// Nobody else denies.
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/verify", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous verify must be 401, got %d", rec.Code)
	}
}

// TestDeviceLinkLifecycle pins the no-Nostr path: the operator mints, the
// click lands once (302 + cookie), a second click is dead, the session
// survives a serve restart, and the operator can revoke both invites and
// live sessions through the access card's API.
func TestDeviceLinkLifecycle(t *testing.T) {
	f := newMemberFixture(t)
	op := opCookie(t, f)

	// Mint.
	req := httptest.NewRequest(http.MethodPost, "/api/members/invites", strings.NewReader(`{"name":"grandma"}`))
	req.AddCookie(op)
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var mint struct {
		Link string `json:"link"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mint); err != nil || !strings.HasPrefix(mint.Link, "/auth/link/") {
		t.Fatalf("mint response: %s", rec.Body.String())
	}

	// The click lands: 302 back to the app (a next on the appliance's own
	// domain) + a device session cookie bound to the name.
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, mint.Link+"?next=https://app.cp.example.com/y", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://app.cp.example.com/y" {
		t.Fatalf("link land with next: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	tok := cookieOf(t, rec, memberCookie)
	if tok == "" {
		t.Fatal("no member cookie on link land")
	}

	// Single-use: the second click is dead.
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, mint.Link, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reused link must be 404, got %d", rec.Code)
	}

	// The session survives a serve restart (members.json, 0600).
	m2 := NewMembers(f.membersFile)
	if _, name, device, ok := m2.MemberIdentity(tok); !ok || name != "grandma" || !device {
		t.Fatalf("restart must keep the device session: ok=%v name=%q device=%v", ok, name, device)
	}

	// The operator sees the live session and can drop it.
	req = httptest.NewRequest(http.MethodGet, "/api/members", nil)
	req.AddCookie(op)
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("members list: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Sessions []struct {
			Name string `json:"name"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].Name != "grandma" {
		t.Fatalf("members list: %s", rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodDelete, "/api/members/sessions/grandma", nil)
	req.AddCookie(op)
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("drop: %d %s", rec.Code, rec.Body.String())
	}
	if _, _, _, ok := f.s.Members.MemberIdentity(tok); ok {
		t.Fatal("the dropped session must be dead")
	}

	// A fresh invite can be revoked before use: the link dies unconsumed.
	req = httptest.NewRequest(http.MethodPost, "/api/members/invites", strings.NewReader(`{"name":"grandpa"}`))
	req.AddCookie(op)
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &mint); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/members", nil)
	req.AddCookie(op)
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	var list2 struct {
		Invites []struct {
			Hash string `json:"hash"`
		} `json:"invites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list2); err != nil {
		t.Fatal(err)
	}
	if len(list2.Invites) != 1 {
		t.Fatalf("invite list: %s", rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodDelete, "/api/members/invites/"+list2.Invites[0].Hash, nil)
	req.AddCookie(op)
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, mint.Link, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a revoked link must be 404, got %d", rec.Code)
	}
}

// TestMemberRoutesNeedTheTier pins the dormant posture: without the member
// tier the routes 404 instead of half-serving.
func TestMemberRoutesNeedTheTier(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	opPK := pkOf(t, secret32(1))
	s := &Server{Store: store, Auth: NewAuth([]string{opPK}, ""), ConsoleSecret: secret32(4)}
	for _, path := range []string{"/auth", "/auth/verify", "/auth/link/x", "/api/auth/member/challenge"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s without the tier must 404, got %d", path, rec.Code)
		}
	}
}

// TestSafeNext pins the post-login redirect validation: paths on this origin
// and full URLs on the appliance's own registrable domain survive; anything
// else (another site, a scheme-relative host) collapses to "".
func TestSafeNext(t *testing.T) {
	pub := "https://cp.example.com"
	s := &Server{PublicOrigin: &pub}
	cases := map[string]string{
		"":                                 "",
		"/x":                               "/x",
		"//evil.com":                       "",
		"https://app.cp.example.com/y":     "https://app.cp.example.com/y",
		"https://cp.example.com/z":         "https://cp.example.com/z",
		"https://evil.com":                 "",
		"https://cp.example.com.evil.com/": "",
		"http://app.cp.example.com/y":      "http://app.cp.example.com/y",
	}
	for in, want := range cases {
		if got := s.safeNext(in); got != want {
			t.Fatalf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
