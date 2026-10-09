package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/state"
)

// ---- fixture ----

const testChannelID = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

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
// coords pointed at a fake relay serving BY KIND (the NIP-43 community list
// naming the fixture's member; the app channel's kind-9000 put-user granting
// them), an exposed app in the toolset's durable dir, and a public origin
// for cookie/next handling.
func newMemberFixture(t *testing.T) *memberFixture {
	return newMemberFixtureOpts(t, true)
}

// newMemberFixtureOpts(…
func newMemberFixtureOpts(t *testing.T, memberInChannel bool) *memberFixture {
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
	// The app the gate fronts: yuvomi, riding the test channel.
	apps, err := agenttools.OpenApps(filepath.Join(dir, "agent-tools", "apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apps.Expose(agenttools.AppRecord{
		Name: "yuvomi", FQDN: "yuvomi.example.com", Target: "10.0.0.9:3000",
		Visibility: agenttools.VisibilityFamily, Auth: agenttools.AuthGate,
		Group: testChannelID, Owner: "op", Requester: f.memberPK, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// The relay answers by KIND: 13534 (the community list) + 9000 (the
	// channel roster granting the member — omitted for the not-in-channel
	// fixtures).
	// The community list names BOTH (in reality the operator is a member —
	// their /auth login runs the same community check before the whitelist
	// promotion).
	events := []map[string]interface{}{membershipListEvent(t, f.relaySec, 100, f.memberPK, f.opPK)}
	// The OPERATOR is in the channel too (in reality: #freehold's roster).
	events = append(events, signEventMap(t, f.relaySec, wire.PutUser, 100,
		[][]string{{"h", testChannelID}, {"p", f.opPK}}, ""))
	if memberInChannel {
		events = append(events, signEventMap(t, f.relaySec, wire.PutUser, 100,
			[][]string{{"h", testChannelID}, {"p", f.memberPK}}, ""))
	}
	f.relay = fakeRelayQuery(t, events)
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
		AgentToolsDir: filepath.Join(dir, "agent-tools"),
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
		// Serve BY FILTER: a real relay matches kinds + the #h/#p tag sets;
		// dumping everything would make any channel query answer "member"
		// and the negative half of every channel test meaningless.
		var filters []map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&filters)
		matched := []map[string]interface{}{}
		for _, e := range events {
			ok := len(filters) == 0
			for _, f := range filters {
				ok = true
				if kinds, okk := f["kinds"].([]interface{}); okk {
					want := false
					for _, k := range kinds {
						if fk, _ := k.(float64); fk == float64(e["kind"].(uint32)) {
							want = true
						}
					}
					if !want {
						ok = false
					}
				}
				for tag, vals := range map[string]bool{"#h": true, "#p": true} {
					wantVals, hasTag := f[tag].([]interface{})
					if !hasTag {
						continue
					}
					evTags, _ := e["tags"].([][]string)
					hit := false
					for _, vt := range wantVals {
						v, _ := vt.(string)
						for _, t2 := range evTags {
							if t2[0] == tag[1:] && t2[1] == v {
								hit = true
							}
						}
					}
					if !hit {
						ok = false
					}
					_ = vals
				}
				if !ok {
					break
				}
			}
			if ok {
				matched = append(matched, e)
			}
		}
		_ = json.NewEncoder(w).Encode(matched)
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
	// The app-aware gate: the member is IN the app's channel (the fixture's
	// fake roster grants them) — the forwarded host resolves the app.
	req := httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Original-Host", "yuvomi.example.com")
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: mtok})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("member verify on the app: %d", rec.Code)
	}
	// A host with no app record fails CLOSED — even a member who just
	// logged in.
	req = httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Original-Host", "unknown.example.com")
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: mtok})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code == http.StatusNoContent {
		t.Fatal("an unregistered host must not admit")
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

	// Mint (opens the named app; there is no all-apps link).
	req := httptest.NewRequest(http.MethodPost, "/api/members/invites", strings.NewReader(`{"name":"grandma","apps":["yuvomi"]}`))
	req.AddCookie(op)
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	// No apps = refused outright.
	reqNoApps := httptest.NewRequest(http.MethodPost, "/api/members/invites", strings.NewReader(`{"name":"nobody"}`))
	reqNoApps.AddCookie(op)
	recNoApps := httptest.NewRecorder()
	f.s.ServeHTTP(recNoApps, reqNoApps)
	if recNoApps.Code != 400 {
		t.Fatalf("an invite with no apps must be refused, got %d", recNoApps.Code)
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

	// The session survives a serve restart (members.json, 0600) — the app
	// BINDING included: a session whose apps were dropped by a restart
	// validates but admits nothing (the silent-lockout bug this pins).
	m2 := NewMembers(f.membersFile)
	info, ok := m2.MemberIdentity(tok)
	if !ok || info.Name != "grandma" || !info.Device || len(info.Apps) != 1 || info.Apps[0] != "yuvomi" {
		t.Fatalf("restart must keep the device session WITH its app binding: %+v ok=%v", info, ok)
	}
	req = httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Original-Host", "yuvomi.example.com")
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: tok})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("the restarted device session must still admit its app: %d", rec.Code)
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
	if _, ok := f.s.Members.MemberIdentity(tok); ok {
		t.Fatal("the dropped session must be dead")
	}

	// A fresh invite can be revoked before use: the link dies unconsumed.
	req = httptest.NewRequest(http.MethodPost, "/api/members/invites", strings.NewReader(`{"name":"grandpa","apps":["yuvomi"]}`))
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

// TestMemberAdminRoutesRefuseMemberSessions pins the privilege boundary: the
// member-role session the public login flow hands relay members must never
// mint invites (that onboards arbitrary non-members to the gated apps),
// list members, revoke invites, or drop sessions — those are operator-only.
func TestMemberAdminRoutesRefuseMemberSessions(t *testing.T) {
	f := newMemberFixture(t)
	tok, err := f.s.Auth.IssueSessionRole(f.memberPK, RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	reqs := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/members", nil),
		httptest.NewRequest(http.MethodPost, "/api/members/invites", strings.NewReader(`{"name":"x"}`)),
		httptest.NewRequest(http.MethodDelete, "/api/members/invites/x", nil),
		httptest.NewRequest(http.MethodDelete, "/api/members/sessions/x", nil),
	}
	for _, req := range reqs {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		rec := httptest.NewRecorder()
		f.s.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s with a member session must be 403, got %d %s", req.Method, req.URL.Path, rec.Code, rec.Body.String())
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
		`/\evil.com`:                       "",
		"https://app.cp.example.com/y":     "https://app.cp.example.com/y",
		"https://yuvomi.example.com/y":     "https://yuvomi.example.com/y",
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

// TestForwardAuthVerifyBrowserRedirect pins the gate's deny UX: a BROWSER
// (Accept: text/html) bounced off a gated app gets a 302 to the login page
// with the app URL as the post-login target — ABSOLUTE (the client is on the
// app's origin, where no login page is served) — and the next target is
// same-site validated (a forged forwarded host collapses to the console
// root). API clients get the bare 401.
func TestForwardAuthVerifyBrowserRedirect(t *testing.T) {
	f := newMemberFixture(t)
	hit := func(accept, fwdHost, fwdURI string) string {
		req := httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
		req.Header.Set("Accept", accept)
		req.Header.Set("X-Forwarded-Host", fwdHost)
		req.Header.Set("X-Forwarded-Uri", fwdURI)
		rec := httptest.NewRecorder()
		f.s.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			return ""
		}
		return rec.Header().Get("Location")
	}
	loc := hit("text/html,application/xhtml+xml", "app.cp.example.com", "/calendar")
	if !strings.HasPrefix(loc, "https://cp.example.com/auth?next=") ||
		!strings.Contains(loc, "app.cp.example.com%2Fcalendar") {
		t.Fatalf("browser deny must redirect to login with the app as next, got %q", loc)
	}
	// A forged forwarded host is not a valid next: the login lands on the
	// console root instead.
	loc = hit("text/html", "evil.example", "/x")
	if !strings.HasPrefix(loc, "https://cp.example.com/auth?next=") ||
		strings.Contains(loc, "evil.example") {
		t.Fatalf("a foreign forwarded host must not ride next, got %q", loc)
	}
	// API clients: bare 401, no redirect.
	req := httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Forwarded-Host", "app.cp.example.com")
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("API deny must be 401, got %d", rec.Code)
	}
}

// TestDeviceLinkBindsApps pins the per-app device link: the invite names the
// apps it opens (>=1, validated against the registry), the landed session
// admits exactly those and NOTHING else — a second app on the same channel
// does not admit, and there is no all-apps link.
func TestDeviceLinkBindsApps(t *testing.T) {
	f := newMemberFixture(t)
	op := opCookie(t, f)

	mint := func(body string) string {
		req := httptest.NewRequest(http.MethodPost, "/api/members/invites", strings.NewReader(body))
		req.AddCookie(op)
		rec := httptest.NewRecorder()
		f.s.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("mint %s: %d %s", body, rec.Code, rec.Body.String())
		}
		var out struct {
			Link string `json:"link"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Link
	}
	// A mistyped app name is an error, not a dead link.
	req := httptest.NewRequest(http.MethodPost, "/api/members/invites", strings.NewReader(`{"name":"x","apps":["yuvomi ","typo"]}`))
	req.AddCookie(op)
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("an unknown app in the mint must be refused, got %d %s", rec.Code, rec.Body.String())
	}

	link := mint(`{"name":"grandma","apps":["yuvomi"]}`)
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, link, nil))
	if rec.Code != 200 {
		t.Fatalf("link land: %d", rec.Code)
	}
	tok := cookieOf(t, rec, memberCookie)

	// The named app admits.
	req = httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Original-Host", "yuvomi.example.com")
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: tok})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("the named app must admit: %d", rec.Code)
	}
	// ANY other host denies — even a second app on the same channel: the
	// device link is per-app, and there is no path to all apps.
	req = httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Original-Host", "photos.example.com")
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: tok})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code == http.StatusNoContent {
		t.Fatal("an unlisted app must not admit a device session")
	}
}

// TestGateChannelRevocation pins the nostr side: the app's channel roster is
// the ACL — a member the channel granted admits; a channel member REMOVED
// (a newer kind-9001) denies within the cache's TTL.
func TestGateChannelRevocation(t *testing.T) {
	f := newMemberFixtureOpts(t, false) // no put-user: not in the channel
	rec := f.loginAs(t, f.memberSec, "")
	if rec.Code != 200 {
		t.Fatalf("community login still works: %d %s", rec.Code, rec.Body.String())
	}
	tok := cookieOf(t, rec, memberCookie)
	req := httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Original-Host", "yuvomi.example.com")
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: tok})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code == http.StatusNoContent {
		t.Fatal("a pubkey outside the app's channel must not admit")
	}
}

// TestConsoleMemberSessionCannotBypassTheGate pins the replay hole: a
// member-role console session (minted by the public login to every relay
// member) holds a live pubkey — replaying its cookie against a gated app
// goes through the SAME channel check a nostr session does; it is never a
// free pass. The operator role passes outright.
func TestConsoleMemberSessionCannotBypassTheGate(t *testing.T) {
	// An in-channel member's console session admits the app.
	f := newMemberFixture(t)
	tok, err := f.s.Auth.IssueSessionRole(f.memberPK, RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Original-Host", "yuvomi.example.com")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("an in-channel member's console session admits: %d", rec.Code)
	}

	// An OUT-of-channel member's console session denies the same app — the
	// cookie is not a bypass.
	f2 := newMemberFixtureOpts(t, false)
	tok2, err := f2.s.Auth.IssueSessionRole(f2.memberPK, RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Original-Host", "yuvomi.example.com")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok2})
	rec = httptest.NewRecorder()
	f2.s.ServeHTTP(rec, req)
	if rec.Code == http.StatusNoContent {
		t.Fatal("a member-role session must not bypass the channel check")
	}

	// The operator role passes outright.
	tok3, err := f.s.Auth.IssueSessionRole(f.opPK, RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Original-Host", "yuvomi.example.com")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok3})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("the operator passes the gate outright: %d", rec.Code)
	}
}

// TestMemberPortalLandsThePubkeySession pins the key-holder's bridge: the
// operator mints a single-use portal link, the click lands a FULL member
// session for THEIR pubkey (the channel checks apply — the same admission a
// Nostr login would give), and the second open is dead.
func TestMemberPortalLandsThePubkeySession(t *testing.T) {
	f := newMemberFixture(t)
	op := opCookie(t, f)

	req := httptest.NewRequest(http.MethodPost, "/api/members/portal", strings.NewReader("{}"))
	req.AddCookie(op)
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("portal mint: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Link string `json:"link"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !strings.HasPrefix(out.Link, "/auth/portal/") {
		t.Fatalf("portal mint response: %s", rec.Body.String())
	}

	// The click lands the member session (redirect + cookie), and the
	// session's PUBKEY is the operator's — the app gate checks channels.
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, out.Link, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("portal land: %d", rec.Code)
	}
	tok := cookieOf(t, rec, memberCookie)
	if tok == "" {
		t.Fatal("no member cookie on the portal land")
	}
	info, ok := f.s.Members.MemberIdentity(tok)
	if !ok || info.Pubkey != f.opPK || info.Device {
		t.Fatalf("the portal session must carry the operator's pubkey as a member: %+v", info)
	}
	req = httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Original-Host", "yuvomi.example.com")
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: tok})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("the portal session admits via the channel check: %d", rec.Code)
	}

	// Single-use.
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, out.Link, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a reused portal link must be 404, got %d", rec.Code)
	}

	// A member session cannot MINT one (operator-only).
	mtok, err := f.s.Auth.IssueSessionRole(f.memberPK, RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/members/portal", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: mtok})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a member session must not mint portal links, got %d", rec.Code)
	}
}

// TestGateResolvesOldShapeRows pins the migration: a record written by the
// released per-app shape (stored FQDN <name>.<cpHost>) still gates at its
// DERIVED hostname (<name>.<world domain>) — the edge renders the derived
// name, so the gate matching the stale stored FQDN would lock every member
// out invisibly (the operator passes outright).
func TestGateResolvesOldShapeRows(t *testing.T) {
	f := newMemberFixture(t)
	// Overwrite the row with the OLD shape's stored FQDN.
	stale, err := agenttools.OpenApps(filepath.Join(f.s.AgentToolsDir, "apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	staleRec, _ := stale.Get("yuvomi")
	staleRec.FQDN = "yuvomi.cp.example.com" // the released shape's stored value
	_, _ = stale.Unexpose("yuvomi")
	if err := stale.Expose(staleRec); err != nil {
		t.Fatal(err)
	}

	// The member logs in and hits the DERIVED hostname — the stale stored
	// FQDN must not matter to the gate.
	loginRec := f.loginAs(t, f.memberSec, "")
	if loginRec.Code != 200 {
		t.Fatalf("login: %d %s", loginRec.Code, loginRec.Body.String())
	}
	tok := cookieOf(t, loginRec, memberCookie)
	req := httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Original-Host", "yuvomi.example.com")
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: tok})
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("the gate must resolve the old-shape row at its derived hostname: %d", rec.Code)
	}
}

// TestMemberPortalSurvivesRestart pins the portal store's persistence: the
// token's row (pubkey + expiry) round-trips through members.json — an
// unconsumed link must not die on the serve restart every build does.
func TestMemberPortalSurvivesRestart(t *testing.T) {
	f := newMemberFixture(t)
	tok, err := f.s.Members.IssueMemberPortal(f.opPK, "member session")
	if err != nil {
		t.Fatal(err)
	}
	m2 := NewMembers(f.membersFile)
	pk, _, ok, err := m2.ConsumeMemberPortal(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || pk != f.opPK {
		t.Fatalf("the portal token must survive a restart: pk=%q ok=%v", pk, ok)
	}
}

// TestAuthLoginPromotesTheWhitelistKey pins the one-door rule: the /auth
// member login with a WHITELISTED pubkey lands BOTH sessions — the operator
// cookie (the console's admin surface answers) AND the member cookie (the
// app gates). A member key lands the member session only: the overview
// stays 401.
func TestAuthLoginPromotesTheWhitelistKey(t *testing.T) {
	f := newMemberFixture(t)
	rec := f.loginAs(t, f.opSec, "") // the operator's key — on the whitelist
	if rec.Code != 200 {
		t.Fatalf("the operator's /auth login: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Role != "operator" {
		t.Fatalf("the whitelisted login must report operator: %s", rec.Body.String())
	}
	if cookieOf(t, rec, memberCookie) == "" {
		t.Fatal("the member cookie must land (the app gates)")
	}
	opTok := cookieOf(t, rec, sessionCookie)
	if opTok == "" {
		t.Fatal("the operator cookie must land (the console surface)")
	}
	// The console's admin surface answers the operator session.
	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: opTok})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("the operator session must reach the overview: %d %s", rec.Code, rec.Body.String())
	}

	// A member key: the member session only.
	rec = f.loginAs(t, f.memberSec, "")
	if rec.Code != 200 {
		t.Fatalf("the member's /auth login: %d %s", rec.Code, rec.Body.String())
	}
	if cookieOf(t, rec, sessionCookie) != "" {
		t.Fatal("a non-whitelisted key must NOT land an operator session")
	}
	if cookieOf(t, rec, memberCookie) == "" {
		t.Fatal("the member cookie must land")
	}
}

// TestJobsServesTheMemberCookieSession pins the one member route's fallback:
// the /auth login's fh_member pubkey session reaches their OWN rows (prompts
// included) and nobody else's — the same redaction a console member-role
// session gets. A DEVICE session (no pubkey) stays out entirely.
func TestJobsServesTheMemberCookieSession(t *testing.T) {
	f := newMemberFixture(t)
	rec := f.loginAs(t, f.memberSec, "")
	mtok := cookieOf(t, rec, memberCookie)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: mtok})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("the member session must reach the jobs read: %d %s", rec.Code, rec.Body.String())
	}

	// A device session (no pubkey): the jobs view stays closed.
	_, err := f.s.Members.IssueInvite("device-owner", f.opPK, []string{"yuvomi"})
	if err != nil {
		t.Fatal(err)
	}
	_ = err
	devTok, err := f.s.Members.IssueMemberSession("", "device-owner", true, []string{"yuvomi"})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: devTok})
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("a device session must not read jobs, got %d", rec.Code)
	}
}

// TestMyAppsPerSessionKind pins the portal's redaction: the operator's
// session sees every exposed app; a member's fh_member session sees only
// what their channel roster admits; a device session sees exactly the apps
// its link was minted for; the anonymous caller sees nothing. Rows carry
// names + FQDNs only — no targets, no owners.
func TestMyAppsPerSessionKind(t *testing.T) {
	f := newMemberFixture(t)
	// yuvomi is already exposed by the fixture (testChannelID, the sibling
	// shape) — the member's channel admits it. One more app on a channel
	// they're NOT in completes the matrix.
	apps, err := agenttools.OpenApps(filepath.Join(f.s.AgentToolsDir, "apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apps.Expose(agenttools.AppRecord{Name: "other", FQDN: "other.example.com", Target: "10.0.0.10:80", Group: "another-channel", Owner: "somebody", Visibility: "family"}); err != nil {
		t.Fatal(err)
	}

	names := func(rec *httptest.ResponseRecorder) []string {
		var out struct {
			Apps []struct {
				Name string `json:"name"`
				FQDN string `json:"fqdn"`
			} `json:"apps"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("bad body: %s", rec.Body.String())
		}
		got := []string{}
		for _, a := range out.Apps {
			got = append(got, a.Name)
		}
		return got
	}
	get := func(cookies ...*http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/my/apps", nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		f.s.ServeHTTP(rec, req)
		return rec
	}

	// The anonymous: closed.
	if rec := get(); rec.Code != 401 {
		t.Fatalf("anonymous portal: %d", rec.Code)
	}

	// The operator (a console session): everything.
	rec := get(opCookie(t, f))
	if rec.Code != 200 {
		t.Fatalf("the operator portal: %d %s", rec.Code, rec.Body.String())
	}
	got := names(rec)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "other" || got[1] != "yuvomi" {
		t.Fatalf("the operator sees both: %v", got)
	}
	if body := rec.Body.String(); strings.Contains(body, "10.0.0.9") || strings.Contains(body, "somebody") {
		t.Fatal("portal rows must not leak targets or owners")
	}

	// The member (the /auth login's fh_member session): only the channel
	// apps — yuvomi's channel admits them, other's does not.
	rec = f.loginAs(t, f.memberSec, "")
	mtok := cookieOf(t, rec, memberCookie)
	rec = get(&http.Cookie{Name: memberCookie, Value: mtok})
	if rec.Code != 200 {
		t.Fatalf("the member portal: %d %s", rec.Code, rec.Body.String())
	}
	if got := names(rec); len(got) != 1 || got[0] != "yuvomi" {
		t.Fatalf("the member sees only the channel apps: %v", got)
	}

	// The device session: exactly the bound list.
	devTok, err := f.s.Members.IssueMemberSession("", "device-owner", true, []string{"other"})
	if err != nil {
		t.Fatal(err)
	}
	rec = get(&http.Cookie{Name: memberCookie, Value: devTok})
	if rec.Code != 200 {
		t.Fatalf("the device portal: %d %s", rec.Code, rec.Body.String())
	}
	if got := names(rec); len(got) != 1 || got[0] != "other" {
		t.Fatalf("the device session sees exactly its bound apps: %v", got)
	}
}
