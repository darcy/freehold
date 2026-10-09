// The member identity tier: the console's second session class for the
// appliance's USERS — family, team — beside the operator's NIP-98 admin
// auth. Two ways in, both ending in a `fh_member` cookie that Caddy's
// forward_auth validates on /auth/verify:
//
//   - NIP-07 Nostr login, admitted only when the pubkey is on the relay's
//     own NIP-43 membership list — the same identity the member already
//     chats with; no separate user store exists.
//   - A single-use device-link invite: the operator mints a link (a name +
//     a token shown once), the click binds a session to the device. The
//     no-Nostr path for the people the appliance serves.
//
// An operator session also passes the gate (operators are members of their
// own appliance). State persists beside sessions.json (0600) so a serve
// restart — every build/update restarts this process — does not log the
// family out.
package console

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"freehold/contract/relay"
	"freehold/control-plane/api/agenttools"
)

const (
	memberCookie     = "fh_member"
	memberTTL        = 30 * 24 * 3600 * time.Second
	memberMaxAge     = int(memberTTL / time.Second)
	inviteTTL        = 7 * 24 * time.Hour
	memberCheckTTL   = 10 * time.Minute
	memberCheckGrace = 24 * time.Hour
	memberCacheCap   = 4096
)

// MemberSession is one authenticated member: a Nostr login (pubkey set —
// the app gate checks their CHANNEL membership per app) or a device-link
// session (Device — the gate checks the app against the link's static app
// list; there is no all-apps path).
type MemberSession struct {
	expires time.Time
	pubkey  string
	name    string
	device  bool
	apps    []string // device sessions: the app NAMES the link opens
}

// memberInvite is an unconsumed device link. The raw token is shown ONCE at
// mint and never stored — the file holds its sha256.
type memberInvite struct {
	expires   time.Time
	createdBy string
	name      string
	apps      []string
}

// Members is the member session + invite store, persisted to members.json.
type Members struct {
	file     string
	mu       sync.Mutex
	sessions map[string]MemberSession // token -> session
	invites  map[string]memberInvite  // sha256(token) -> invite
}

// memberSessionRow / memberInviteRow are the on-disk shapes (expires as unix
// seconds).
type memberSessionRow struct {
	Pubkey  string   `json:"pubkey,omitempty"`
	Name    string   `json:"name"`
	Device  bool     `json:"device"`
	Apps    []string `json:"apps,omitempty"`
	Expires int64    `json:"expires"`
}

type memberInviteRow struct {
	Name      string   `json:"name"`
	CreatedBy string   `json:"created_by"`
	Apps      []string `json:"apps"`
	Expires   int64    `json:"expires"`
}

type memberFile struct {
	Sessions map[string]memberSessionRow `json:"sessions"`
	Invites  map[string]memberInviteRow  `json:"invites"`
}

// NewMembers loads the member store ("" = memory-only, for tests). A missing
// or corrupt file is a fresh start; expired rows are dropped on load.
func NewMembers(file string) *Members {
	m := &Members{
		file:     file,
		sessions: map[string]MemberSession{},
		invites:  map[string]memberInvite{},
	}
	m.load()
	return m
}

func (m *Members) load() {
	if m.file == "" {
		return
	}
	raw, err := os.ReadFile(m.file)
	if err != nil {
		return
	}
	var f memberFile
	if json.Unmarshal(raw, &f) != nil {
		return
	}
	for tok, r := range f.Sessions {
		exp := time.Unix(r.Expires, 0)
		if exp.Before(now()) {
			continue
		}
		m.sessions[tok] = MemberSession{expires: exp, pubkey: r.Pubkey, name: r.Name, device: r.Device, apps: r.Apps}
	}
	for h, r := range f.Invites {
		exp := time.Unix(r.Expires, 0)
		if exp.Before(now()) {
			continue
		}
		m.invites[h] = memberInvite{expires: exp, createdBy: r.CreatedBy, name: r.Name, apps: r.Apps}
	}
}

// save persists the store (0600, temp+rename — a crash mid-write never
// leaves a corrupt file behind). Same contract as Auth.saveSessions: an
// error propagates, persistence silently stopping would be exactly the
// logout-on-restart this file exists to prevent.
func (m *Members) save() error {
	if m.file == "" {
		return nil
	}
	f := memberFile{
		Sessions: make(map[string]memberSessionRow, len(m.sessions)),
		Invites:  make(map[string]memberInviteRow, len(m.invites)),
	}
	for tok, s := range m.sessions {
		f.Sessions[tok] = memberSessionRow{Pubkey: s.pubkey, Name: s.name, Device: s.device, Apps: s.apps, Expires: s.expires.Unix()}
	}
	for h, i := range m.invites {
		f.Invites[h] = memberInviteRow{Name: i.name, CreatedBy: i.createdBy, Apps: i.apps, Expires: i.expires.Unix()}
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := m.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.file)
}

// IssueInvite mints a single-use device link for `name`, opening exactly
// the named apps (>= 1 — there is no all-apps link: access comes from
// channels and named apps, never from a blank check). The returned token
// rides /auth/link/<token> and is never persisted (only its sha256 is).
func (m *Members) IssueInvite(name, createdBy string, apps []string) (string, error) {
	token, err := randomHex(16)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(token))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invites[hex.EncodeToString(sum[:])] = memberInvite{expires: now().Add(inviteTTL), createdBy: createdBy, name: name, apps: apps}
	if err := m.save(); err != nil {
		return "", err
	}
	return token, nil
}

// ConsumeInvite single-use consumes an invite if fresh; returns the name it
// was minted for + the apps the link opens. A persistence failure
// propagates — swallowing it here would resurrect the consumed invite on
// the next restart (single-use is the invite's whole security).
func (m *Members) ConsumeInvite(token string) (string, []string, bool, error) {
	sum := sha256.Sum256([]byte(token))
	h := hex.EncodeToString(sum[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[h]
	if !ok {
		return "", nil, false, nil
	}
	delete(m.invites, h)
	if err := m.save(); err != nil {
		return "", nil, false, err
	}
	if inv.expires.Before(now()) {
		return "", nil, false, nil
	}
	return inv.name, inv.apps, true, nil
}

// RevokeInvite drops an unconsumed invite (by its listed hash). notFound
// distinguishes a stale hash from a persistence failure.
func (m *Members) RevokeInvite(hash string) (notFound bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.invites[hash]; !ok {
		return true, nil
	}
	delete(m.invites, hash)
	return false, m.save()
}

// IssueMemberSession mints a member session token (apps ride device
// sessions; nostr sessions check channels dynamically and pass nil).
func (m *Members) IssueMemberSession(pubkey, name string, device bool, apps []string) (string, error) {
	token, err := randomHex(32)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[token] = MemberSession{expires: now().Add(memberTTL), pubkey: pubkey, name: name, device: device, apps: apps}
	if err := m.save(); err != nil {
		delete(m.sessions, token)
		return "", err
	}
	return token, nil
}

// MemberInfo is one session's resolved identity: the pubkey ("" for a
// device session), the person's name, the device flag, and the device
// session's app list (nil for nostr sessions).
type MemberInfo struct {
	Pubkey string
	Name   string
	Device bool
	Apps   []string
}

// MemberIdentity validates a member session (sliding refresh) and resolves it.
func (m *Members) MemberIdentity(token string) (MemberInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, found := m.sessions[token]
	if !found {
		return MemberInfo{}, false
	}
	if s.expires.Before(now()) {
		delete(m.sessions, token)
		return MemberInfo{}, false
	}
	s.expires = now().Add(memberTTL)
	m.sessions[token] = s
	return MemberInfo{Pubkey: s.pubkey, Name: s.name, Device: s.device, Apps: s.apps}, true
}

// KillSession ends one member session (logout). A persistence failure
// propagates — a session silently surviving a restart is a logout that
// isn't.
func (m *Members) KillSession(token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[token]; !ok {
		return nil
	}
	delete(m.sessions, token)
	return m.save()
}

// DropMemberSessions ends every session named `name` (the operator revoking
// a person: their device can re-enter only through a fresh invite).
func (m *Members) DropMemberSessions(name string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for tok, s := range m.sessions {
		if s.name == name {
			delete(m.sessions, tok)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, m.save()
}

// MemberInviteJSON is one listed invite (the hash, never the token).
type MemberInviteJSON struct {
	Name      string   `json:"name"`
	Apps      []string `json:"apps"`
	Hash      string   `json:"hash"`
	CreatedBy string   `json:"created_by"`
	Expires   int64    `json:"expires"`
}

// MemberSessionJSON is one listed live session.
type MemberSessionJSON struct {
	Name    string   `json:"name"`
	Pubkey  string   `json:"pubkey,omitempty"`
	Device  bool     `json:"device"`
	Apps    []string `json:"apps,omitempty"`
	Expires int64    `json:"expires"`
}

// List returns the current invites + sessions for the operator's access card.
func (m *Members) List() (invites []MemberInviteJSON, sessions []MemberSessionJSON) {
	m.mu.Lock()
	defer m.mu.Unlock()
	invites = []MemberInviteJSON{}
	sessions = []MemberSessionJSON{}
	for h, i := range m.invites {
		invites = append(invites, MemberInviteJSON{Name: i.name, Apps: i.apps, Hash: h, CreatedBy: i.createdBy, Expires: i.expires.Unix()})
	}
	sort.Slice(invites, func(i, j int) bool { return invites[i].Name < invites[j].Name })
	for _, s := range m.sessions {
		sessions = append(sessions, MemberSessionJSON{Name: s.name, Pubkey: s.pubkey, Device: s.device, Apps: s.apps, Expires: s.expires.Unix()})
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Name < sessions[j].Name })
	return invites, sessions
}

// ---- member routes ----

// hostOf extracts the bare host from an origin (public origins are DNS
// names; the bracket-aware IPv6 handling of checkOrigin is not needed here).
func hostOf(origin string) string {
	_, rest, ok := strings.Cut(origin, "://")
	if !ok {
		return origin
	}
	if i := strings.IndexAny(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.Index(rest, ":"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// memberCookieDomain scopes the member cookie to the appliance's whole
// registrable domain — every gated app subdomain must present it. Host-only
// when no public origin is configured (loopback/dev).
func (s *Server) memberCookieDomain() string {
	if s.PublicOrigin == nil || *s.PublicOrigin == "" {
		return ""
	}
	return "." + hostOf(*s.PublicOrigin)
}

// safeNext validates a post-login redirect target: a relative path on this
// origin, or a full URL on the appliance's own registrable domain (the gate
// hands the ORIGINAL app host back — app.cp.domain — so login returns to the
// app that was asked for). Anything else (another site, a scheme-relative
// //host) collapses to "".
func (s *Server) safeNext(next string) string {
	if next == "" {
		return ""
	}
	// A path on this origin — but not "//host" and not "/\host": browsers
	// normalize the backslash form to "//host", so both are open redirects.
	if strings.HasPrefix(next, "/") && (len(next) < 2 || (next[1] != '/' && next[1] != '\\')) {
		return next
	}
	if s.PublicOrigin == nil || *s.PublicOrigin == "" {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	pub := hostOf(*s.PublicOrigin)
	host := u.Hostname()
	if host == pub || strings.HasSuffix(host, "."+pub) {
		return next
	}
	return ""
}

func setMemberCookie(w http.ResponseWriter, token, domain string) {
	c := &http.Cookie{
		Name:     memberCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   memberMaxAge,
		HttpOnly: true,
		Secure:   true,
		// Lax, not Strict: the cookie must ride the FIRST top-level
		// navigation to a gated app from an external link (an emailed invite)
		// — Strict would drop it there and force a second bounce through
		// /auth. Subdomain requests are same-site either way.
		SameSite: http.SameSiteLaxMode,
	}
	if domain != "" {
		c.Domain = domain
	}
	http.SetCookie(w, c)
}

func clearMemberCookie(w http.ResponseWriter, domain string) {
	c := &http.Cookie{Name: memberCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true}
	if domain != "" {
		c.Domain = domain
	}
	http.SetCookie(w, c)
}

// cookieValue extracts a named cookie's value from the request.
func cookieValue(r *http.Request, name string) string {
	for _, c := range r.Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// memberCheck is one cached relay-membership answer.
type memberCheck struct {
	ok      bool
	checked time.Time
}

// cachedMembership is the outage-aware cache ALL relay-membership checks
// share (the community read at login, the channel reads at the gate): the
// answer re-checks every memberCheckTTL; a relay outage keeps serving the
// last answer for memberCheckGrace — chat being down must not lock the
// family out of their apps mid-outage, and a blip must not fall the gate
// open either. An uncached key on a dead relay fails CLOSED. Bounded: a
// fresh signed key reaches a check on every forged login attempt through
// the public edge, so an uncapped map is a memory DoS — clear-when-full is
// the whole policy (the cost is one extra relay read per admitted member).
func (s *Server) cachedMembership(key string, check func() (bool, error)) (bool, error) {
	s.memberMu.Lock()
	if s.memberCache == nil {
		s.memberCache = map[string]memberCheck{}
	}
	if c, ok := s.memberCache[key]; ok && now().Sub(c.checked) < memberCheckTTL {
		s.memberMu.Unlock()
		return c.ok, nil
	}
	s.memberMu.Unlock()

	ok, err := check()

	s.memberMu.Lock()
	if err != nil {
		if c, ok := s.memberCache[key]; ok && now().Sub(c.checked) < memberCheckGrace {
			cached := c.ok
			s.memberMu.Unlock()
			return cached, nil
		}
	} else {
		if len(s.memberCache) >= memberCacheCap {
			s.memberCache = map[string]memberCheck{}
		}
		s.memberCache[key] = memberCheck{ok: ok, checked: now()}
	}
	s.memberMu.Unlock()
	return ok, err
}

// memberRelayAllowed consults the relay's NIP-43 membership list — the
// member LOGIN's gate (the app gate checks channels; this checks the
// community). No relay coords at all is a hard error: the gate never fails
// open on a misconfigured world.
func (s *Server) memberRelayAllowed(pubkey string) (bool, error) {
	return s.cachedMembership("community|"+pubkey, func() (bool, error) {
		// The ONE membership read (console login's member role uses it too):
		// dial/auth resolution + the not-configured guard live there.
		_ = s.Store.Reload()
		return s.isRelayMember(s.Store.Snapshot(), pubkey)
	})
}

// memberLoginPage serves the self-contained NIP-07 login page.
func (s *Server) memberLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.Members == nil {
		writeErr(w, http.StatusNotFound, "member login is not enabled on this console")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(memberHTML))
}

func (s *Server) memberChallenge(w http.ResponseWriter, r *http.Request) {
	if s.Members == nil || s.Auth == nil {
		writeErr(w, http.StatusNotFound, "member login is not enabled on this console")
		return
	}
	nonce, err := s.Auth.IssueChallenge()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"nonce": nonce, "ts": nowSecs()})
}

// memberLoginRequest is the operator login body plus the redirect target the
// gate hands back (the app host that asked).
type memberLoginRequest struct {
	loginRequest
	Next string `json:"next"`
}

func (s *Server) memberLogin(w http.ResponseWriter, r *http.Request) {
	if s.Members == nil || s.Auth == nil {
		writeErr(w, http.StatusNotFound, "member login is not enabled on this console")
		return
	}
	var req memberLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad login body")
		return
	}
	if !s.Auth.ConsumeChallenge(req.Nonce) {
		writeErr(w, http.StatusUnauthorized, "unknown, expired, or reused challenge")
		return
	}
	delta := nowSecs() - req.CreatedAt
	if delta < 0 {
		delta = -delta
	}
	if delta > authFreshnessSec {
		writeErr(w, http.StatusUnauthorized, "stale signature timestamp")
		return
	}
	if err := s.verifyNIP98(req.Pubkey, req.CreatedAt, req.Tags, req.Nonce, req.Sig); err != nil {
		writeErr(w, http.StatusUnauthorized, "signature verification failed")
		return
	}
	ok, err := s.memberRelayAllowed(req.Pubkey)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "membership unavailable: "+err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusForbidden, "not a member of this relay")
		return
	}
	token, err := s.Members.IssueMemberSession(req.Pubkey, req.Pubkey, false, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	setMemberCookie(w, token, s.memberCookieDomain())
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "next": s.safeNext(req.Next)})
}

// memberLinkLand consumes a single-use device link and binds a session to
// the device — the no-Nostr path.
func (s *Server) memberLinkLand(w http.ResponseWriter, r *http.Request, token string) {
	if s.Members == nil {
		writeErr(w, http.StatusNotFound, "member login is not enabled on this console")
		return
	}
	name, apps, ok, err := s.Members.ConsumeInvite(token)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<!doctype html><meta charset='utf-8'><body style='background:#0d1117;color:#8b949e;font-family:monospace;display:flex;align-items:center;justify-content:center;min-height:100vh'><p>This link is unknown, expired, or already used. Ask for a fresh one.</p></body>"))
		return
	}
	mtok, err := s.Members.IssueMemberSession("", name, true, apps)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	setMemberCookie(w, mtok, s.memberCookieDomain())
	if next := s.safeNext(r.URL.Query().Get("next")); next != "" {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><meta charset='utf-8'><body style='background:#0d1117;color:#c9d1d9;font-family:monospace;display:flex;align-items:center;justify-content:center;min-height:100vh;text-align:center'><div><h1 style='font-size:16px;letter-spacing:2px'>signed in</h1><p style='color:#8b949e'>Welcome, " + htmlEsc(name) + " — you can close this tab and open your app.</p></div></body>"))
}

// forwardAuthVerify is the gate endpoint Caddy's forward_auth calls for every
// request to a gated app: the original request's cookies ride the
// subrequest; 204 admits, anything else denies. A denial is content-negotiated:
// a browser (Accept: text/html) gets a 302 to the login page with the app's
// URL as the post-login target; API clients get the bare 401.
func (s *Server) forwardAuthVerify(w http.ResponseWriter, r *http.Request) {
	if s.Members == nil {
		writeErr(w, http.StatusNotFound, "member gate is not enabled on this console")
		return
	}
	if s.Auth != nil {
		if tok := cookieValue(r, sessionCookie); tok != "" {
			// Any console session role passes the gate — operator or member:
			// both are admitted identities (the member role was verified
			// against the relay's list at ITS login).
			if _, _, ok := s.Auth.SessionIdentity(tok); ok {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	}
	if tok := cookieValue(r, memberCookie); tok != "" {
		if info, ok := s.Members.MemberIdentity(tok); ok {
			admitted, err := s.memberAdmits(info, r)
			if err != nil {
				// The membership source is unreachable AND the cache's grace
				// window passed: fail closed, say why.
				writeErr(w, http.StatusServiceUnavailable, "membership unavailable: "+err.Error())
				return
			}
			if admitted {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	}
	// A denied BROWSER gets the login page: the redirect must be ABSOLUTE —
	// Caddy copies this response (status + Location) back to the client, and
	// the client is on the APP's origin, where no login page is served.
	// API clients (no Accept: text/html) get the bare 401.
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		next := s.gateNext(r)
		if next == "" {
			next = strings.TrimSuffix(ofStr(s.PublicOrigin), "/")
		}
		login := "/auth?next=" + url.QueryEscape(next)
		if pub := ofStr(s.PublicOrigin); pub != "" {
			login = strings.TrimSuffix(pub, "/") + login
		}
		http.Redirect(w, r, login, http.StatusFound)
		return
	}
	writeErr(w, http.StatusUnauthorized, "no valid session — sign in at /auth")
}

// ofStr dereferences an optional string ("" for nil) — the optional PublicOrigin.
func ofStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// memberAdmits is the app-aware gate: WHICH app did this request hit, and
// does the session's identity reach it?
//   - a device session: the app is on the link's static list (the invite
//     bound the apps at mint; there is no all-apps link),
//   - a Nostr session: the pubkey is IN the app's channel (IsMemberAuth —
//     the channel roster IS the ACL; revocation is a roster change landing
//     within the cache TTL),
//   - a host with no app record: fail closed (the vhost template gates apps
//     only; a hand-added gate on a non-app host admits nobody).
func (s *Server) memberAdmits(info MemberInfo, r *http.Request) (bool, error) {
	host := r.Header.Get("X-Original-Host")
	if host == "" {
		host = r.Header.Get("X-Forwarded-Host")
	}
	if host == "" {
		return false, nil
	}
	rec, ok := s.appByHost(host)
	if !ok {
		return false, nil
	}
	if info.Device {
		for _, name := range info.Apps {
			if name == rec.Name {
				return true, nil
			}
		}
		return false, nil
	}
	if info.Pubkey == "" {
		return false, nil
	}
	return s.memberChannelAllowed(info.Pubkey, rec.Group)
}

// appByHost resolves the forwarded host to an app record. The registry is
// read FRESH per verify — expose writes land in the separate agent-tools
// process, and one small JSON read per gated request is the price of never
// serving from a stale ACL.
func (s *Server) appByHost(host string) (agenttools.AppRecord, bool) {
	if s.AgentToolsDir == "" {
		return agenttools.AppRecord{}, false
	}
	apps, err := agenttools.OpenApps(filepath.Join(s.AgentToolsDir, "apps.json"))
	if err != nil {
		return agenttools.AppRecord{}, false
	}
	for _, rec := range apps.List() {
		if rec.FQDN == host {
			return rec, true
		}
	}
	return agenttools.AppRecord{}, false
}

// memberChannelAllowed is the app gate's channel read: is the pubkey in the
// app's relay channel — the SAME channel-scoped read the runner grants use,
// cached with the same outage discipline as the community check (re-check
// every memberCheckTTL; a relay outage keeps the last answer for
// memberCheckGrace; an uncached pubkey on a dead relay fails CLOSED).
func (s *Server) memberChannelAllowed(pubkey, groupID string) (bool, error) {
	return s.cachedMembership("channel|"+groupID+"|"+pubkey, func() (bool, error) {
		_ = s.Store.Reload()
		snap := s.Store.Snapshot()
		dial, auth := s.relayDialAuth(snap)
		if dial == "" {
			return false, errRelayUnknown
		}
		rpk := ""
		if snap.RelayPubkey != nil {
			rpk = *snap.RelayPubkey
		}
		if rpk == "" && s.Builder != nil {
			rpk = s.Builder.RelayPK
		}
		if rpk == "" {
			return false, errRelayUnknown
		}
		return relay.IsMemberAuth(dial, auth, s.ConsoleSecret, groupID, pubkey)
	})
}

// gateNext composes the app URL a denied browser should return to after
// login: the forwarded proto/host/uri headers (Caddy's forward_auth passes
// the original request's headers; the auth subrequest's own URI is /auth/verify
// and is never used). An empty PublicOrigin (loopback/dev) yields "" — the
// browser falls back to the login page alone. Same-site validated by
// safeNext before use.
func (s *Server) gateNext(r *http.Request) string {
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		return ""
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "https"
	}
	return s.safeNext(proto + "://" + host + r.Header.Get("X-Forwarded-Uri"))
}

func (s *Server) memberLogout(w http.ResponseWriter, r *http.Request) {
	if s.Members == nil {
		writeErr(w, http.StatusNotFound, "member login is not enabled on this console")
		return
	}
	if tok := cookieValue(r, memberCookie); tok != "" {
		if err := s.Members.KillSession(tok); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	clearMemberCookie(w, s.memberCookieDomain())
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// ---- operator-scoped member admin ----

func (s *Server) membersList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.Members == nil {
		writeErr(w, http.StatusNotFound, "member login is not enabled on this console")
		return
	}
	invites, sessions := s.Members.List()
	writeJSON(w, http.StatusOK, map[string]interface{}{"invites": invites, "sessions": sessions})
}

type memberInviteReq struct {
	Name string   `json:"name"`
	Apps []string `json:"apps"`
}

func (s *Server) memberInviteMint(w http.ResponseWriter, r *http.Request) {
	operator, err := s.requireAdmin(r)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.Members == nil {
		writeErr(w, http.StatusNotFound, "member login is not enabled on this console")
		return
	}
	var req memberInviteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "an invite needs a name (who the link is for)")
		return
	}
	// The link opens NAMED apps, at least one — there is no all-apps link
	// (access comes from channels and named apps, never a blank check). Each
	// name must be a REGISTERED app: a typo would be a dead link, not a
	// narrower one.
	if len(req.Apps) == 0 {
		writeErr(w, http.StatusBadRequest, "an invite opens at least one app (pick from /api/apps)")
		return
	}
	regApps, err := agenttools.OpenApps(filepath.Join(s.AgentToolsDir, "apps.json"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, name := range req.Apps {
		if _, ok := regApps.Get(name); !ok {
			writeErr(w, http.StatusBadRequest, "unknown app "+name+" — pick from /api/apps")
			return
		}
	}
	name := strings.TrimSpace(req.Name)
	token, err := s.Members.IssueInvite(name, operator, req.Apps)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"link":    "/auth/link/" + token,
		"name":    name,
		"apps":    req.Apps,
		"expires": now().Add(inviteTTL).Unix(),
	})
}

func (s *Server) memberInviteRevoke(w http.ResponseWriter, r *http.Request, hash string) {
	if _, err := s.requireAdmin(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.Members == nil {
		writeErr(w, http.StatusNotFound, "member login is not enabled on this console")
		return
	}
	notFound, err := s.Members.RevokeInvite(hash)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if notFound {
		writeErr(w, http.StatusNotFound, "no such invite")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *Server) memberDrop(w http.ResponseWriter, r *http.Request, name string) {
	if _, err := s.requireAdmin(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.Members == nil {
		writeErr(w, http.StatusNotFound, "member login is not enabled on this console")
		return
	}
	n, err := s.Members.DropMemberSessions(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "no live sessions named "+name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// htmlEsc is the same escaping rule the admin SPA's esc() applies — names
// come from the operator, link lands from a URL; nothing is interpolated raw.
func htmlEsc(v string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#39;", "'", "&#39;")
	return r.Replace(v)
}

//go:embed member.html
var memberHTML string
