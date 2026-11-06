package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGoogle is the SA's token endpoint AND the Sheets API: the SA JSON's
// token_uri points here, so the REAL jwt-mint flow runs (a real RSA key
// signs a real assertion) and the Sheets calls land on a scripted REST
// surface.
type fakeGoogle struct {
	mu        sync.Mutex
	tokenURL  string
	sheetsGet []string
	lastAuth  string
	sheetBody string
	values    string
}

func (g *fakeGoogle) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.Header.Get("Content-Type") == "application/x-www-form-urlencoded":
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			if !strings.Contains(string(body), "grant_type=urn") {
				t.Errorf("token exchange: wrong grant_type: %s", string(body))
			}
			g.lastAuth = ""
			w.Write([]byte(`{"access_token":"fake-token-1","expires_in":3600}`))
		default:
			g.lastAuth = r.Header.Get("Authorization")
			g.sheetsGet = append(g.sheetsGet, r.URL.Path+r.URL.RawQuery)
			if strings.Contains(r.URL.Path, "/values/") {
				w.Write([]byte(g.values))
				return
			}
			w.Write([]byte(g.sheetBody))
		}
	})
}

// saJSONFor mints a REAL RSA keypair and renders the SA JSON pointing at the
// fake's token endpoint.
func saJSONFor(t *testing.T, tokenURL string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	raw, _ := json.Marshal(map[string]string{
		"client_email": "sheets-svc@example.iam.gserviceaccount.com",
		"private_key":  string(pemKey),
		"token_uri":    tokenURL,
	})
	return string(raw)
}

func newServer(t *testing.T, env map[string]string) (*server, *fakeGoogle) {
	t.Helper()
	g := &fakeGoogle{sheetBody: `{"properties":{"title":"Spike Sheet"},"sheets":[{"properties":{"title":"Sheet1"}}]}`, values: `{"values":[["a","b"],["1","2"]]}`}
	gw := httptest.NewServer(g.handler(t))
	t.Cleanup(gw.Close)
	g.tokenURL = gw.URL
	if _, ok := env["GOOGLE_SA_JSON"]; ok && env["GOOGLE_SA_JSON"] == "" {
		env["GOOGLE_SA_JSON"] = saJSONFor(t, gw.URL)
	}
	if _, ok := env["FREEHOLD_SHEETS_BASE"]; !ok {
		env["FREEHOLD_SHEETS_BASE"] = gw.URL
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	s, err := newServerFromEnv()
	if err != nil {
		t.Fatalf("newServerFromEnv: %v", err)
	}
	return s, g
}

func postMCP(t *testing.T, s *server, body, broker string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if broker != "" {
		req.Header.Set("Authorization", "Bearer "+broker)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestBrokerAuthRequired(t *testing.T) {
	s, _ := newServer(t, map[string]string{
		"FREEHOLD_BROKER_TOKEN":   "tok-1",
		"FREEHOLD_SPREADSHEET_ID": "sheet123",
		"GOOGLE_SA_JSON":          saJSONFor(t, "https://unused.example/token"),
	})
	code, _ := postMCP(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("no-token request = %d, want 401", code)
	}
	code, _ = postMCP(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "wrong")
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong-token request = %d, want 401", code)
	}
	code, body := postMCP(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "tok-1")
	if code != 200 || !strings.Contains(body, "get_cells") {
		t.Fatalf("good-token tools/list = %d %s", code, body)
	}
}

func TestReadOnlyHidesWriteTools(t *testing.T) {
	s, _ := newServer(t, map[string]string{
		"FREEHOLD_BROKER_TOKEN":   "tok-1",
		"FREEHOLD_SPREADSHEET_ID": "sheet123",
		"FREEHOLD_READ_ONLY":      "1",
		"GOOGLE_SA_JSON":          saJSONFor(t, "https://unused.example/token"),
	})
	_, body := postMCP(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "tok-1")
	if strings.Contains(body, "update_cells") || strings.Contains(body, "append_rows") {
		t.Fatalf("read-only must not advertise write tools: %s", body)
	}
	if !strings.Contains(body, "get_cells") {
		t.Fatalf("read tools must be advertised: %s", body)
	}
	// And a hardcoded write call is refused.
	_, body = postMCP(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"update_cells","arguments":{"range":"A1","data":[["x"]]}}}`, "tok-1")
	if !strings.Contains(body, "READ-ONLY") {
		t.Fatalf("a hardcoded write must be refused: %s", body)
	}
}

func TestGetCellsEndToEnd(t *testing.T) {
	s, g := newServer(t, map[string]string{
		"FREEHOLD_BROKER_TOKEN":   "tok-1",
		"FREEHOLD_SPREADSHEET_ID": "sheet123",
		"GOOGLE_SA_JSON":          "",
	})
	_ = g
	code, body := postMCP(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_cells","arguments":{"range":"Sheet1!A1:B2"}}}`, "tok-1")
	if code != 200 || !strings.Contains(body, `"isError":false`) {
		t.Fatalf("get_cells = %d %s", code, body)
	}
	if !strings.Contains(body, "a\\tb") {
		t.Fatalf("the sheet values must come back: %s", body)
	}
	// The Sheets call was addressed at the PINNED sheet only.
	if len(g.sheetsGet) == 0 || !strings.Contains(g.sheetsGet[0], "/spreadsheets/sheet123/values/") {
		t.Fatalf("the call must target the pinned sheet, got %v", g.sheetsGet)
	}
	if g.lastAuth != "Bearer fake-token-1" {
		t.Fatalf("the minted token must authenticate the Sheets call, got %q", g.lastAuth)
	}
}

func TestUpdateCellsEndToEnd(t *testing.T) {
	s, g := newServer(t, map[string]string{
		"FREEHOLD_BROKER_TOKEN":   "tok-1",
		"FREEHOLD_SPREADSHEET_ID": "sheet123",
		"GOOGLE_SA_JSON":          "",
	})
	code, body := postMCP(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"update_cells","arguments":{"range":"Sheet1!A1","data":[["x","y"]]}}}`, "tok-1")
	if code != 200 || !strings.Contains(body, `"isError":false`) {
		t.Fatalf("update_cells = %d %s", code, body)
	}
	_ = g
}

func TestNoSAConfiguredFailsClean(t *testing.T) {
	s, _ := newServer(t, map[string]string{
		"FREEHOLD_SPREADSHEET_ID": "sheet123",
	})
	_, body := postMCP(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_cells","arguments":{}}}`, "")
	if !strings.Contains(body, "no service account configured") {
		t.Fatalf("a missing SA must fail with a clear configuration error: %s", body)
	}
}

func TestNotificationGets202(t *testing.T) {
	s, _ := newServer(t, map[string]string{"FREEHOLD_BROKER_TOKEN": "tok-1"})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer tok-1")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notification = %d, want 202", rec.Code)
	}
}

// TestRangeTraversalRejected: a path-traversal range (or any character
// outside A1's safe charset) is refused — it must never reach the URL, where
// it would retarget the request at a different spreadsheet with the SA's
// credential.
func TestRangeTraversalRejected(t *testing.T) {
	s, g := newServer(t, map[string]string{
		"FREEHOLD_BROKER_TOKEN":   "tok-1",
		"FREEHOLD_SPREADSHEET_ID": "sheet123",
		"GOOGLE_SA_JSON":          "",
	})
	for _, bad := range []string{
		"x/../../../../spreadsheets/other-id/values/A1",
		"Sheet1!A1?x=1",
		"Sheet1!A1#frag",
		"Sheet1!A1%2F",
		"Sheet.1!A1",
	} {
		_, body := postMCP(t, s, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_cells","arguments":{"range":%s}}}`, jsStr(bad)), "tok-1")
		if !strings.Contains(body, "A1 notation only") {
			t.Fatalf("range %q must be refused, got: %s", bad, body)
		}
	}
	// Nothing reached the Sheets API.
	if len(g.sheetsGet) != 0 {
		t.Fatalf("a refused range must never reach the API, got %v", g.sheetsGet)
	}
}

// jsStr quotes s as a JSON string literal for a request body.
func jsStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
