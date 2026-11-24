// freehold-mcp-sheets is the MCP capability plane's Google Sheets service:
// a minimal MCP Streamable-HTTP server that speaks the Sheets API for ONE
// pinned spreadsheet, authenticated to the broker by a static bearer token
// and to Google by a service-account key. It exists because the community
// Sheets MCP servers have no auth and no pin — this one answers only its
// broker and only its sheet.
//
// Posture:
//   - Broker auth: every /mcp request must carry Authorization: Bearer
//     $FREEHOLD_BROKER_TOKEN (the token the CP sealed for this capability;
//     LiteLLM attaches it as a static header). Unset = the endpoint is open
//     — for a slotted capability the CP always sets it.
//   - Pin: every tool addresses $FREEHOLD_SPREADSHEET_ID — the service
//     cannot name another spreadsheet. The HARD bound is still the SA's
//     Google ACL (share only that sheet with the service account).
//   - Read-only: $FREEHOLD_READ_ONLY=1 advertises only the read tools AND
//     requests the spreadsheets.readonly OAuth scope — belt and suspenders
//     with a Viewer service account.
//   - The SA key arrives as $GOOGLE_SA_JSON (the raw service-account JSON)
//     or $GOOGLE_SA_JSON_B64 (base64). When absent the server runs and
//     answers MCP, but every Google call fails with a clear configuration
//     error — the plumbing is verifiable without a credential.
//
// stdlib only: the service-account flow is a JWT (RSA-SHA256) exchanged at
// the SA's own token_uri; the Sheets calls are plain REST.
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type saKey struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

func (k *saKey) rsaKey() (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(k.PrivateKey))
	if block == nil {
		return nil, errors.New("service account key: no PEM block")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsa, ok := key.(*rsa.PrivateKey); ok {
			return rsa, nil
		}
		return nil, errors.New("service account key: not RSA")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

type googleAuth struct {
	key         *saKey
	rsa         *rsa.PrivateKey
	scope       string
	mu          sync.Mutex
	accessToken string
	expires     time.Time
	http        *http.Client
}

// token returns a cached (or freshly minted) service-account access token:
// a JWT signed with the SA's key, exchanged at the SA's own token_uri.
func (a *googleAuth) token() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.accessToken != "" && time.Now().Before(a.expires.Add(-60*time.Second)) {
		return a.accessToken, nil
	}
	if a.key == nil || a.rsa == nil {
		return "", fmt.Errorf("no service account configured (set GOOGLE_SA_JSON)")
	}
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	header := b64([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]interface{}{
		"iss":   a.key.ClientEmail,
		"scope": a.scope,
		"aud":   a.key.TokenURI,
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + b64(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.rsa, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	assertion := signingInput + "." + b64(sig)
	form := strings.NewReader("grant_type=" + urlQueryEscape("urn:ietf:params:oauth:grant-type:jwt-bearer") +
		"&assertion=" + urlQueryEscape(assertion))
	req, err := http.NewRequest(http.MethodPost, a.key.TokenURI, form)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange: HTTP %d: %s", resp.StatusCode, trim(body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("token exchange: unreadable response")
	}
	a.accessToken = tok.AccessToken
	a.expires = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return a.accessToken, nil
}

func urlQueryEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "%", "%25"), "&", "%26")
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
