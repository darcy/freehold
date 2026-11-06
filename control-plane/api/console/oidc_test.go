package console

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/crypto"
	cert "freehold/platform/services/certificates/letsencrypt"
)

// newOIDCFixture builds a console whose world carries a sealed OIDC client
// record (the shape the CP's pangolin ensure writes) + the member store.
func newOIDCFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	encSec := bytes32(0x51)
	idJSON, _ := json.Marshal(map[string]string{
		"nostr_secret_hex": hex.EncodeToString(bytes32(0x11)),
		"enc_secret_hex":   hex.EncodeToString(encSec),
	})
	if err := os.MkdirAll(filepath.Join(dir, "console"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "console", "identity.json"), idJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	pub, err := crypto.X25519PublicKey(encSec)
	if err != nil {
		t.Fatal(err)
	}
	clientID := "c1"
	clientSecret := "s3cr3t"
	redirect := "https://pangolin.example.test/auth/idp/1/oidc/callback"
	if err := cert.SaveCreds(
		filepath.Join(dir, "world-secrets", "oidc.json"), "oidc",
		map[string]string{"OIDC_CLIENT_ID": clientID, "OIDC_CLIENT_SECRET": clientSecret, "OIDC_REDIRECT_URI": redirect},
		func(p, aad, plain []byte) ([]byte, error) { return crypto.Seal(p, aad, plain) },
		pub, "oidc",
	); err != nil {
		t.Fatal(err)
	}
	return &Server{StateDir: dir, Members: NewMembers("")}, clientID, clientSecret
}

func bytes32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func pkcePair(t *testing.T) (verifier, challenge string) {
	t.Helper()
	verifier = "test-verifier-0123456789abcdefghijklmnopqrstuvwxyz"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// TestOIDCAuthorizeRequiresMemberLogin: without a member session the browser is
// sent through the member NIP-07 login (returning here); with one, a code is
// issued and the state echoed back to Pangolin's callback.
func TestOIDCAuthorizeRequiresMemberLogin(t *testing.T) {
	s, clientID, _ := newOIDCFixture(t)
	_, challenge := pkcePair(t)
	authorize := "/api/oidc/authorize?response_type=code&client_id=" + clientID +
		"&redirect_uri=" + url.QueryEscape("https://pangolin.example.test/auth/idp/1/oidc/callback") +
		"&state=st-123&code_challenge=" + challenge + "&code_challenge_method=S256"

	// No session: the login page, with this URL as the next target.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, authorize, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("anonymous authorize = %d, want 302 to the login", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/auth?next=") {
		t.Fatalf("anonymous authorize must go to the member login, got %q", loc)
	}
	next, _ := url.QueryUnescape(strings.TrimPrefix(loc, "/auth?next="))
	if next != authorize {
		t.Fatalf("the login must return to the authorize URL: %q", next)
	}

	// A member session: the code lands on the callback with the state.
	pk := "a" + strings.Repeat("b", 63)
	tok, err := s.Members.IssueMemberSession(pk, "Darcy", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, authorize, nil)
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: tok})
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("member authorize = %d, want 302 to the callback", rec.Code)
	}
	loc = rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "pangolin.example.test" || u.Path != "/auth/idp/1/oidc/callback" {
		t.Fatalf("the code must return to Pangolin's callback, got %q", loc)
	}
	if u.Query().Get("state") != "st-123" || u.Query().Get("code") == "" {
		t.Fatalf("the callback must carry the state + code, got %q", loc)
	}
}

// TestOIDCAuthorizeRejectsForeignClientOrRedirect: only the sealed client and
// Pangolin's own callback are accepted.
func TestOIDCAuthorizeRejectsForeignClientOrRedirect(t *testing.T) {
	s, clientID, _ := newOIDCFixture(t)
	cases := map[string]string{
		"wrong client":   "/api/oidc/authorize?response_type=code&client_id=other&redirect_uri=" + url.QueryEscape("https://pangolin.example.test/auth/idp/1/oidc/callback"),
		"wrong redirect": "/api/oidc/authorize?response_type=code&client_id=" + clientID + "&redirect_uri=" + url.QueryEscape("https://evil.example/steal"),
	}
	for name, u := range cases {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))
		if rec.Code == http.StatusFound {
			t.Fatalf("%s: must not redirect (got %d)", name, rec.Code)
		}
	}
}

// TestOIDCTokenExchange: the code is single-use, bound to the client + redirect
// + PKCE verifier, and the id_token carries the member's pubkey as `sub`
// (Pangolin's identifierPath).
func TestOIDCTokenExchange(t *testing.T) {
	s, clientID, clientSecret := newOIDCFixture(t)
	verifier, challenge := pkcePair(t)
	pk := "a" + strings.Repeat("c", 63)
	tok, err := s.Members.IssueMemberSession(pk, "Darcy", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	authorize := "/api/oidc/authorize?response_type=code&client_id=" + clientID +
		"&redirect_uri=" + url.QueryEscape("https://pangolin.example.test/auth/idp/1/oidc/callback") +
		"&state=st&code_challenge=" + challenge + "&code_challenge_method=S256"
	req := httptest.NewRequest(http.MethodGet, authorize, nil)
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: tok})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	u, _ := url.Parse(rec.Header().Get("Location"))
	code := u.Query().Get("code")
	if code == "" {
		t.Fatal("no code issued")
	}

	exchange := func(form url.Values, basic bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/oidc/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if basic {
			r.SetBasicAuth(clientID, clientSecret)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		return rec
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"https://pangolin.example.test/auth/idp/1/oidc/callback"},
		"code_verifier": {verifier},
	}

	// Wrong verifier: rejected (and the code burns).
	bad := url.Values{}
	for k, v := range form {
		bad[k] = v
	}
	bad.Set("code_verifier", "wrong-verifier")
	if rec := exchange(bad, true); rec.Code == http.StatusOK {
		t.Fatal("a wrong PKCE verifier must not mint a token")
	}

	// Re-authorize (the code burned above) and exchange properly.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, authorize, nil)
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: tok})
	s.ServeHTTP(rec, req)
	u, _ = url.Parse(rec.Header().Get("Location"))
	form.Set("code", u.Query().Get("code"))

	rec = exchange(form, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("token exchange = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		IDToken     string `json:"id_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.IDToken == "" || out.AccessToken == "" || out.TokenType != "Bearer" || out.ExpiresIn == 0 {
		t.Fatalf("the token response is incomplete: %+v", out)
	}
	// Decode the id_token claims (Pangolin decodes without verifying; the
	// payload must still carry the pubkey).
	parts := strings.Split(out.IDToken, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", out.IDToken)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Sub  string `json:"sub"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Sub != pk || claims.Name != "Darcy" {
		t.Fatalf("claims = %+v, want sub %s", claims, pk)
	}

	// Replay: the code is single-use.
	if rec := exchange(form, true); rec.Code == http.StatusOK {
		t.Fatal("a consumed code must not mint a second token")
	}
	// Wrong client secret.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, authorize, nil)
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: tok})
	s.ServeHTTP(rec, req)
	u, _ = url.Parse(rec.Header().Get("Location"))
	form.Set("code", u.Query().Get("code"))
	r := httptest.NewRequest(http.MethodPost, "/api/oidc/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(clientID, "nope")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code == http.StatusOK {
		t.Fatal("a wrong client secret must not mint a token")
	}
}
