// Package console is the CP's loopback admin/ops web console — the "console
// underneath" the unified api/ front reads. It serves the /api/* surface
// (auth, overview, world,
// provision/rotate/revoke/grant, DNS, agents, portal) with the SAME security
// guards: NIP-98 operator login, HttpOnly+SameSite=Strict session cookies,
// single-use portal tokens, login freshness windows, and the DNS-rebinding
// Origin guard. This replaces the Rust console crate at parity.
package console

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"freehold/contract/wire"
)

// Loopback hosts allowed by the DNS-rebinding guard (http/https only; any
// port is fine). The console never leaves the machine.
var loopbackHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "[::1]": true}

// Session/session cookie/portal/auth freshness constants (web.rs).
const (
	sessionCookie    = "fh_session"
	sessionTTL       = 24 * 3600 * time.Second
	challengeTTL     = 120 * time.Second
	authFreshness    = 60 * time.Second
	portalTTL        = 60 * time.Second
	authFreshnessSec = int64(60)
)

func now() time.Time { return time.Now() }
func nowSecs() int64 { return time.Now().Unix() }

// randomHex returns n random bytes as hex (core::nip98::random_hex).
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Session is an operator's authenticated session.
type Session struct {
	expires time.Time
	pubkey  string
}

// Portal is a single-use browser-launch token.
type Portal struct {
	expires time.Time
	pubkey  string
}

// Auth is the NIP-98 operator auth. Present ONLY when an admin whitelist is
// configured; absent => the loopback-only posture (all /api routes open).
type Auth struct {
	admins     map[string]bool
	sessions   map[string]Session
	challenges map[string]time.Time
	portals    map[string]Portal
	mu         sync.Mutex
}

// NewAuth builds an Auth from the admin whitelist.
func NewAuth(admins []string) *Auth {
	set := make(map[string]bool, len(admins))
	for _, a := range admins {
		set[a] = true
	}
	return &Auth{
		admins:     set,
		sessions:   map[string]Session{},
		challenges: map[string]time.Time{},
		portals:    map[string]Portal{},
	}
}

// AdminCount reports the number of seeded admins.
func (a *Auth) AdminCount() int { return len(a.admins) }

// isAdmin reports whether a pubkey is a seeded operator.
func (a *Auth) isAdmin(pubkey string) bool { return a.admins[pubkey] }

// IssueChallenge mints a single-use login challenge (returns the nonce).
func (a *Auth) IssueChallenge() (string, error) {
	nonce, err := randomHex(16)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.challenges[nonce] = now()
	return nonce, nil
}

// ConsumeChallenge single-use consumes a challenge if fresh.
func (a *Auth) ConsumeChallenge(nonce string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	ts, ok := a.challenges[nonce]
	if !ok {
		return false
	}
	delete(a.challenges, nonce)
	return now().Sub(ts) <= challengeTTL
}

// IssueSession mints a session token for an operator.
func (a *Auth) IssueSession(pubkey string) (string, error) {
	token, err := randomHex(32)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[token] = Session{expires: now().Add(sessionTTL), pubkey: pubkey}
	return token, nil
}

// SessionIdentity validates a session (sliding refresh) + returns the operator
// pubkey. None for unknown/expired tokens.
func (a *Auth) SessionIdentity(token string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[token]
	if !ok {
		return "", false
	}
	if s.expires.Before(now()) {
		delete(a.sessions, token)
		return "", false
	}
	s.expires = now().Add(sessionTTL)
	a.sessions[token] = s
	return s.pubkey, true
}

// IssuePortal mints a single-use portal token bound to an operator.
func (a *Auth) IssuePortal(pubkey string) (string, error) {
	token, err := randomHex(16)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.portals[token] = Portal{expires: now().Add(portalTTL), pubkey: pubkey}
	return token, nil
}

// ConsumePortal single-use consumes a portal token if fresh (operator pubkey).
func (a *Auth) ConsumePortal(token string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.portals[token]
	if !ok {
		return "", false
	}
	delete(a.portals, token)
	if p.expires.Before(now()) {
		return "", false
	}
	return p.pubkey, true
}

// sessionToken extracts the session cookie value from the request.
func sessionToken(r *http.Request) string {
	for _, c := range r.Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	return ""
}

// requireSession is the gate for /api/* routes. No-op without auth (loopback
// posture); 401 with a configured-but-missing/invalid session. Returns the
// operator pubkey when auth is configured.
func (s *Server) requireSession(r *http.Request) (string, error) {
	if s.Auth == nil {
		return "", nil
	}
	tok := sessionToken(r)
	if tok == "" {
		return "", errUnauthorized
	}
	pk, ok := s.Auth.SessionIdentity(tok)
	if !ok {
		return "", errUnauthorized
	}
	return pk, nil
}

// requireSessionPubkey is like requireSession but 404s when auth is NOT
// configured (the portal/world routes only exist when there is something to
// session into).
func (s *Server) requireSessionPubkey(r *http.Request) (string, error) {
	if s.Auth == nil {
		return "", errAuthNotConfigured
	}
	return s.requireSession(r)
}

var (
	errUnauthorized      = errors.New("unauthenticated — log in via NIP-98: GET /api/auth/challenge, POST /api/auth/login")
	errAuthNotConfigured = errors.New("console auth is not configured")
	errForbidden         = errors.New("cross-origin request refused (console is loopback-only)")
)

// checkOrigin is the DNS-rebinding guard: a page hosted anywhere else must not
// drive this loopback console. A missing Origin (curl, hand-rolled clients) is
// allowed; a PRESENT non-loopback Origin is refused. The operator's configured
// public domain (fronted by a proxy) is also allowed (C3.5).
func checkOrigin(r *http.Request, publicOrigin *string) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	// scheme + host only (port optional): a port-less loopback Origin is fine.
	scheme, rest, ok := strings.Cut(origin, "://")
	if !ok {
		return errForbidden
	}
	if scheme != "http" && scheme != "https" {
		return errForbidden
	}
	// Bracket-aware: [::1]:8080 yields the literal [::1].
	var host string
	if idx := strings.Index(rest, "["); idx == 0 {
		end := strings.Index(rest, "]")
		if end < 0 {
			return errForbidden
		}
		host = rest[:end+1]
	} else {
		host = rest
		if i := strings.IndexAny(host, ":/"); i >= 0 {
			host = host[:i]
		}
	}
	if loopbackHosts[host] {
		return nil
	}
	if publicOrigin != nil && *publicOrigin != "" {
		pub := *publicOrigin
		if i := strings.Index(pub, "://"); i >= 0 {
			pub = pub[i+3:]
		}
		if i := strings.Index(pub, ":"); i >= 0 {
			pub = pub[:i]
		}
		if host == pub {
			return nil
		}
	}
	return errForbidden
}

// setSessionCookie writes the HttpOnly; SameSite=Strict session cookie.
func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// verifyNIP98 verifies a kind-27235 login event over an admin pubkey.
func (s *Server) verifyNIP98(pubkey string, createdAt int64, tags [][]string, content, sig string) error {
	_, err := wire.VerifyEvent(pubkey, createdAt, wire.KINDHTTPAuth, tags, content, sig)
	return err
}

// ValidateLoopbackBind refuses a non-loopback bind when auth is absent (the
// loopback-only posture C3). A host:port where the host is loopback passes.
func ValidateLoopbackBind(addr string) error {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return fmt.Errorf("%q is not a host:port address", addr)
	}
	host := strings.TrimPrefix(addr[:i], "[")
	host = strings.TrimSuffix(host, "]")
	if host == "localhost" || host == "::1" || strings.HasPrefix(host, "127.") {
		return nil
	}
	return fmt.Errorf("refusing to bind the console to %q: loopback-only (no authn/TLS on the HTTP surface); reach it from elsewhere with an SSH tunnel", addr)
}
