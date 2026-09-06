package agenttools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"freehold/orchestrator/internal/agent"
	"freehold/orchestrator/internal/crypto"
)

func signForTest(secret []byte, audience string, ts int64, raw string) string {
	canonical := audience + "|" + strconv.FormatInt(ts, 10) + "|" + raw
	digest := sha256.Sum256([]byte(canonical))
	sig, _ := crypto.SignBIP340(secret, digest[:])
	return hex.EncodeToString(sig)
}

func TestVerifyRequest(t *testing.T) {
	aud := "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"
	secret := make([]byte, 32)
	secret[0] = 1
	pk, err := crypto.PubkeyFromSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Unix()
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_agent","arguments":{"name":"bob"}}}`
	grants := []string{pk}

	good := signForTest(secret, aud, ts, raw)
	if _, err := VerifyRequest(grants, aud, pk, good, strconv.FormatInt(ts, 10), raw); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
	if _, err := VerifyRequest(nil, aud, pk, good, strconv.FormatInt(ts, 10), raw); err == nil {
		t.Error("ungranted pubkey must be rejected")
	}
	badsig := signForTest(secret, aud, ts, raw+"x")
	if _, err := VerifyRequest(grants, aud, pk, badsig, strconv.FormatInt(ts, 10), raw); err == nil {
		t.Error("bad signature must be rejected")
	}
	if _, err := VerifyRequest(grants, aud, pk, good, strconv.FormatInt(ts-200, 10), raw); err == nil {
		t.Error("stale timestamp must be rejected")
	}
}

func TestServerAuthGateAndDispatch(t *testing.T) {
	srv := &Server{
		Audience: "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55",
		Grants:   []string{},
		Tools:    &agent.Tools{Console: nil, Create: nil},
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_agent","arguments":{"name":"bob"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatalf("expected an auth error for unauthenticated tools/call: %s", rec.Body.String())
	}
}

func TestServerToolList(t *testing.T) {
	srv := &Server{Audience: "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var resp struct {
		Result struct {
			Tools []map[string]interface{} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Result.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(resp.Result.Tools))
	}
	for _, name := range []string{"create_agent", "grant_agent", "manage_agent"} {
		found := false
		for _, tl := range resp.Result.Tools {
			if tl["name"] == name {
				found = true
			}
		}
		if !found {
			t.Errorf("missing tool %s", name)
		}
	}
}
