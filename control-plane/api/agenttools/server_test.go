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

	"freehold/control-plane/api/agent"
	"freehold/contract/console"
	"freehold/contract/crypto"
	"freehold/platform/migrations"
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
	if len(resp.Result.Tools) != 10 {
		t.Fatalf("expected 10 tools, got %d", len(resp.Result.Tools))
	}
	for _, name := range []string{"create_agent", "grant_agent", "manage_agent", "world_status", "world_teardown", "world_migrate", "world_build", "world_authorize_door", "world_revoke_door", "world_register_facts"} {
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

	// world_migrate runs the bound verify-gated migration runner.
	migrated := false
	srv.Tools.Migrate = func() ([]migrations.Result, error) {
		migrated = true
		return []migrations.Result{{Name: "001-x", OK: true, Applied: true}}, nil
	}
	out := call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"world_migrate","arguments":{}}}`)
	if !migrated {
		t.Fatal("world_migrate did not invoke the bound migrator")
	}
	if !strings.Contains(out, "001-x") {
		t.Fatalf("world_migrate result missing migration: %s", out)
	}

	// world_build runs the CP world-build/reconcile driver.
	built := false
	srv.Tools.World = func() (string, error) { built = true; return "world-build ok", nil }
	out = call(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"world_build","arguments":{}}}`)
	if !built {
		t.Fatal("world_build did not invoke the CP build driver")
	}
	if !strings.Contains(out, "world-build ok") {
		t.Fatalf("world_build result missing report: %s", out)
	}
}

// TestServerScopeAuth proves the per-channel tool-visibility split: a REGISTRY
// agent caller (a pubkey in the registry) is denied the world_* actions while
// an operator caller (roster member, not in the registry) can drive them — the
// same boundary the CPA's stdio bridge filters, now enforced server-side so a
// direct call cannot bypass it.
func TestServerScopeAuth(t *testing.T) {
	const aud = "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"
	agentSec := make([]byte, 32)
	agentSec[0] = 9
	agentPK, err := crypto.PubkeyFromSecret(agentSec)
	if err != nil {
		t.Fatal(err)
	}
	opSec := make([]byte, 32)
	opSec[0] = 10
	opPK, err := crypto.PubkeyFromSecret(opSec)
	if err != nil {
		t.Fatal(err)
	}
	ops := &fakeOps{}
	_, _ = ops.RegisterAgent("cpa", agentPK, "cpa")
	srv := &Server{
		Audience: aud,
		Grants:   func() ([]string, error) { return []string{agentPK, opPK}, nil },
		Tools:    &agent.Tools{Console: ops},
		IsAgent: func(pk string) bool {
			return pk == agentPK // the CPA row
		},
	}

	post := func(secret []byte, pk, tool string) (string, bool) {
		t.Helper()
		raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":{}}}`
		ts := time.Now().Unix()
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
		req.Header.Set(PubkeyHeader, pk)
		req.Header.Set(SigHeader, signForTest(secret, aud, ts, raw))
		req.Header.Set(TSHeader, strconv.FormatInt(ts, 10))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		var resp struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Error == nil {
			return "", false
		}
		return resp.Error.Message, true
	}

	// The CPA (a registry agent) must be DENIED world_* AND grant_agent (both
	// operator-scoped: a grant hands direct exec access to a runner).
	msg, denied := post(agentSec, agentPK, "world_status")
	if !denied || !strings.Contains(msg, "cannot call") {
		t.Fatalf("registry agent must be denied world_status, got denied=%v msg=%q", denied, msg)
	}
	msg, denied = post(agentSec, agentPK, "world_teardown")
	if !denied || !strings.Contains(msg, "cannot call") {
		t.Fatalf("registry agent must be denied world_teardown, got denied=%v msg=%q", denied, msg)
	}
	msg, denied = post(agentSec, agentPK, "grant_agent")
	if !denied || !strings.Contains(msg, "cannot call") {
		t.Fatalf("registry agent must be denied grant_agent (operator-scoped), got denied=%v msg=%q", denied, msg)
	}
	msg, denied = post(agentSec, agentPK, "world_authorize_door")
	if !denied || !strings.Contains(msg, "cannot call") {
		t.Fatalf("registry agent must be denied world_authorize_door, got denied=%v msg=%q", denied, msg)
	}
	msg, denied = post(agentSec, agentPK, "world_revoke_door")
	if !denied || !strings.Contains(msg, "cannot call") {
		t.Fatalf("registry agent must be denied world_revoke_door, got denied=%v msg=%q", denied, msg)
	}
	msg, denied = post(agentSec, agentPK, "world_register_facts")
	if !denied || !strings.Contains(msg, "cannot call") {
		t.Fatalf("registry agent must be denied world_register_facts, got denied=%v msg=%q", denied, msg)
	}

	// The operator (roster member, NOT in the registry) drives world_* freely.
	if _, denied := post(opSec, opPK, "world_status"); denied {
		t.Fatal("operator must be allowed world_status")
	}

	// The CPA can still create/manage agents (the toolset it has).
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"manage_agent","arguments":{}}}`
	ts := time.Now().Unix()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
	req.Header.Set(PubkeyHeader, agentPK)
	req.Header.Set(SigHeader, signForTest(agentSec, aud, ts, raw))
	req.Header.Set(TSHeader, strconv.FormatInt(ts, 10))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "-32003") {
		t.Fatalf("registry agent manage_agent must be allowed: %s", rec.Body.String())
	}
}

// TestWorldStatusInventory proves the single-inventory read: the bound Status
// closure feeds agents + runners + DNS into one world_status payload.
func TestWorldStatusInventory(t *testing.T) {
	const aud = "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"
	sec := make([]byte, 32)
	sec[0] = 3
	pk, _ := crypto.PubkeyFromSecret(sec)
	ops := &fakeOps{}
	_, _ = ops.RegisterAgent("cpa", pk, "cpa")
	srv := &Server{
		Audience: aud,
		Grants:   func() ([]string, error) { return []string{pk}, nil },
		Tools: &agent.Tools{
			Console: ops,
			Status: func() (map[string]interface{}, error) {
				return map[string]interface{}{
					"agents": []console.AgentInfo{{Name: "cpa", Pubkey: pk}},
					"runners": []map[string]interface{}{{"name": "box", "nostr_pubkey": "R1"}},
					"dns":     []map[string]interface{}{{"name": "relay", "ip": "192.168.30.8"}},
				}, nil
			},
		},
	}
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"world_status","arguments":{}}}`
	ts := time.Now().Unix()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
	req.Header.Set(PubkeyHeader, pk)
	req.Header.Set(SigHeader, signForTest(sec, aud, ts, raw))
	req.Header.Set(TSHeader, strconv.FormatInt(ts, 10))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"cpa", "R1", "192.168.30.8"} {
		if !strings.Contains(body, want) {
			t.Fatalf("world_status inventory missing %q: %s", want, body)
		}
	}
}

// TestRegistryGrantFailClosed proves grant_agent fails loudly (never silently
// succeeds) when the console-owner wiring is absent, and refuses an unprovisioned
// runner name when wired.
func TestRegistryGrantFailClosed(t *testing.T) {
	r, err := OpenRegistry(t.TempDir() + "/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Grant("box", "AA"); err == nil {
		t.Fatal("grant without console-owner wiring must fail closed")
	}
	r.ConsoleStateDir = t.TempDir()
	r.RelayURL = "http://127.0.0.1:9" // unreachable; the runner lookup fails first
	r.ConsoleSecret = make([]byte, 32)
	if _, err := r.Grant("nope", "AA"); err == nil {
		t.Fatal("grant for an unprovisioned runner must refuse")
	}
}
