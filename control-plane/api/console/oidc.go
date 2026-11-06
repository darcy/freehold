// The world's OIDC identity provider (docs/NETWORK.md, the public path's
// SSO): Pangolin's resource auth delegates to us, and OUR login page is the
// member NIP-07 page — the family's Nostr keys ARE the SSO identity. Only
// relay members get a code (the member session is minted by memberLogin,
// which admits only the relay whitelist), so Pangolin can only auto-provision
// shadow users for admitted keys. The id_token carries the pubkey as `sub`
// (Pangolin's identifierPath), which its badger middleware forwards as
// Remote-User to the app behind the tunnel — the home gate re-checks the
// relay roster per request from it.
package console

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"freehold/contract/crypto"
	cert "freehold/platform/services/certificates/letsencrypt"
)

const oidcCodeTTL = 5 * time.Minute

// oidcCode is one pending authorization code: single-use, short-lived,
// bound to the client + redirect + PKCE challenge + the member identity.
type oidcCode struct {
	clientID    string
	redirectURI string
	challenge   string
	sub         string
	name        string
	expires     time.Time
}

// oidcCreds opens the world's OIDC client record (world-secrets/oidc.json,
// sealed to the console identity by the CP's pangolin ensure): the client id +
// secret the Pangolin IdP was created with, and the callback Pangolin will
// redirect to (the one redirect_uri we accept).
func (s *Server) oidcCreds() (clientID, clientSecret, redirectURI string, err error) {
	dir := s.stateDir()
	if dir == "" {
		return "", "", "", errors.New("the console has no state dir")
	}
	path := filepath.Join(dir, "world-secrets", "oidc.json")
	raw, rerr := os.ReadFile(filepath.Join(dir, "console", "identity.json"))
	if rerr != nil {
		return "", "", "", fmt.Errorf("console identity: %w", rerr)
	}
	var id struct {
		EncSecretHex string `json:"enc_secret_hex"`
	}
	if jerr := json.Unmarshal(raw, &id); jerr != nil {
		return "", "", "", fmt.Errorf("console identity: %w", jerr)
	}
	secret, herr := hex.DecodeString(id.EncSecretHex)
	if herr != nil || len(secret) != 32 {
		return "", "", "", errors.New("console identity enc secret is invalid")
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	_, env, lerr := cert.LoadCreds(path, open, secret)
	if lerr != nil {
		return "", "", "", fmt.Errorf("oidc credential: %w", lerr)
	}
	return env["OIDC_CLIENT_ID"], env["OIDC_CLIENT_SECRET"], env["OIDC_REDIRECT_URI"], nil
}

// randomToken returns n random bytes as hex.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// oidcAuthorize is the SSO login endpoint Pangolin redirects to (arctic's
// OAuth2Client + PKCE S256). With a valid member session it issues a code and
// returns to Pangolin; without one it sends the browser through the member
// NIP-07 login, which returns here (the next target is this very URL).
func (s *Server) oidcAuthorize(w http.ResponseWriter, r *http.Request) {
	clientID, _, redirectURI, err := s.oidcCreds()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "the world has no OIDC client record (run `freehold build`): "+err.Error())
		return
	}
	q := r.URL.Query()
	if q.Get("response_type") != "code" {
		writeErr(w, http.StatusBadRequest, "unsupported response_type")
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("client_id")), []byte(clientID)) != 1 {
		writeErr(w, http.StatusUnauthorized, "unknown client")
		return
	}
	if redirectURI == "" || q.Get("redirect_uri") != redirectURI {
		writeErr(w, http.StatusBadRequest, "redirect_uri mismatch")
		return
	}
	if m := q.Get("code_challenge_method"); m != "" && m != "S256" {
		writeErr(w, http.StatusBadRequest, "unsupported code_challenge_method")
		return
	}
	if s.Members == nil {
		writeErr(w, http.StatusServiceUnavailable, "member login is not enabled on this console")
		return
	}
	// A Nostr (pubkey) session is the identity; a device link has no pubkey to
	// assert as `sub` — it logs in through the member page like everyone else.
	if tok := cookieValue(r, memberCookie); tok != "" {
		if info, ok := s.Members.MemberIdentity(tok); ok && info.Pubkey != "" {
			code, cerr := randomToken(32)
			if cerr != nil {
				writeErr(w, http.StatusInternalServerError, cerr.Error())
				return
			}
			s.oidcMu.Lock()
			if s.oidcCodes == nil {
				s.oidcCodes = map[string]oidcCode{}
			}
			s.oidcCodes[code] = oidcCode{
				clientID: clientID, redirectURI: redirectURI,
				challenge: q.Get("code_challenge"),
				sub:       strings.ToLower(info.Pubkey), name: info.Name,
				expires: time.Now().Add(oidcCodeTTL),
			}
			s.oidcMu.Unlock()
			u := redirectURI + "?code=" + url.QueryEscape(code)
			if st := q.Get("state"); st != "" {
				u += "&state=" + url.QueryEscape(st)
			}
			http.Redirect(w, r, u, http.StatusFound)
			return
		}
	}
	http.Redirect(w, r, "/auth?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
}

// signIDToken builds an HS256 JWT (Pangolin decodes the claims without
// verifying — but a standard, signed token is the right shape for any client).
func signIDToken(secret, sub, name string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	now := time.Now().Unix()
	claims := map[string]interface{}{
		"sub": sub,
		"iss": "freehold",
		"aud": "pangolin",
		"iat": now,
		"exp": now + 3600,
	}
	if name != "" {
		claims["name"] = name
	}
	payload, _ := json.Marshal(claims)
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(header + "." + p))
	return header + "." + p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// oidcToken is the code exchange (arctic posts grant_type/code/redirect_uri/
// code_verifier with HTTP Basic client auth). Codes are single-use; PKCE S256
// is verified when the authorize request carried a challenge.
func (s *Server) oidcToken(w http.ResponseWriter, r *http.Request) {
	clientID, clientSecret, _, err := s.oidcCreds()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "the world has no OIDC client record (run `freehold build`): "+err.Error())
		return
	}
	if err := r.ParseForm(); err != nil {
		writeErr(w, http.StatusBadRequest, "bad token form")
		return
	}
	id, sec, ok := r.BasicAuth()
	if !ok {
		id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if subtle.ConstantTimeCompare([]byte(id), []byte(clientID)) != 1 ||
		subtle.ConstantTimeCompare([]byte(sec), []byte(clientSecret)) != 1 {
		writeErr(w, http.StatusUnauthorized, "invalid client")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		writeErr(w, http.StatusBadRequest, "unsupported grant_type")
		return
	}
	code := r.PostForm.Get("code")
	s.oidcMu.Lock()
	rec, found := s.oidcCodes[code]
	if found {
		delete(s.oidcCodes, code) // single-use, even on a failed exchange
	}
	s.oidcMu.Unlock()
	if !found || time.Now().After(rec.expires) {
		writeErr(w, http.StatusBadRequest, "invalid or expired code")
		return
	}
	if rec.clientID != clientID || r.PostForm.Get("redirect_uri") != rec.redirectURI {
		writeErr(w, http.StatusBadRequest, "code was issued for another client/redirect")
		return
	}
	if rec.challenge != "" {
		v := r.PostForm.Get("code_verifier")
		if v == "" {
			writeErr(w, http.StatusBadRequest, "code_verifier required")
			return
		}
		sum := sha256.Sum256([]byte(v))
		got := base64.RawURLEncoding.EncodeToString(sum[:])
		if subtle.ConstantTimeCompare([]byte(got), []byte(rec.challenge)) != 1 {
			writeErr(w, http.StatusBadRequest, "code_verifier mismatch")
			return
		}
	}
	access, aerr := randomToken(24)
	if aerr != nil {
		writeErr(w, http.StatusInternalServerError, aerr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     signIDToken(clientSecret, rec.sub, rec.name),
	})
}
