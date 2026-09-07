package agenttools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"freehold/orchestrator/internal/agent"
	"freehold/orchestrator/internal/console"
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
		Grants:   func() ([]string, error) { return []string{}, nil },
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
	if len(resp.Result.Tools) != 5 {
		t.Fatalf("expected 5 tools, got %d", len(resp.Result.Tools))
	}
	for _, name := range []string{"create_agent", "grant_agent", "manage_agent", "world_status", "world_teardown"} {
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

// fakeOps is a minimal ConsoleOps backed by an in-memory agent list, so the
// world tools can be exercised hermetically (status reads it; teardown clears it).
type fakeOps struct {
	agents []console.AgentInfo
}

func (f *fakeOps) RegisterAgent(name, pubkey, channel string) (json.RawMessage, error) {
	f.agents = append(f.agents, console.AgentInfo{Name: name, Pubkey: pubkey})
	return json.RawMessage(`{}`), nil
}
func (f *fakeOps) UnregisterAgent(name string) (json.RawMessage, error) {
	out := f.agents[:0]
	for _, a := range f.agents {
		if a.Name != name {
			out = append(out, a)
		}
	}
	f.agents = out
	return json.RawMessage(`{}`), nil
}
func (f *fakeOps) Agents() ([]console.AgentInfo, error) { return f.agents, nil }
func (f *fakeOps) Grant(runner, pubkey string) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// TestWorldStatusAndTeardown proves the roster-gated world-action surface: a
// granted caller reads what the CP manages and clears it.
func TestWorldStatusAndTeardown(t *testing.T) {
	const aud = "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"
	secret := make([]byte, 32)
	secret[0] = 7
	pk, err := crypto.PubkeyFromSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	ops := &fakeOps{}
	_, _ = ops.RegisterAgent("alice", "AA", "alice")
	_, _ = ops.RegisterAgent("bob", "BB", "bob")
	srv := &Server{
		Audience: aud,
		Grants:   func() ([]string, error) { return []string{pk}, nil },
		Tools:    &agent.Tools{Console: ops},
	}

	call := func(raw string) string {
		t.Helper()
		ts := time.Now().Unix()
		sig := signForTest(secret, aud, ts, raw)
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
		req.Header.Set(PubkeyHeader, pk)
		req.Header.Set(SigHeader, sig)
		req.Header.Set(TSHeader, strconv.FormatInt(ts, 10))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Error != nil {
			t.Fatalf("rpc error: %s", resp.Error.Message)
		}
		if len(resp.Result.Content) == 0 {
			t.Fatal("no text result")
		}
		return resp.Result.Content[0].Text
	}

	// world_status lists the managed agents.
	status := call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"world_status","arguments":{}}}`)
	if !strings.Contains(status, "alice") || !strings.Contains(status, "bob") {
		t.Fatalf("world_status missing agents: %s", status)
	}

	// world_teardown clears them.
	td := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"world_teardown","arguments":{}}}`)
	if !strings.Contains(td, "removed 2") {
		t.Fatalf("world_teardown unexpected: %s", td)
	}
	left, _ := ops.Agents()
	if got := len(left); got != 0 {
		t.Fatalf("world_teardown left %d agents", got)
	}
}
