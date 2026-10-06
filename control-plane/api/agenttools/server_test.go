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

	"freehold/contract/console"
	"freehold/contract/crypto"
	"freehold/control-plane/api/agent"
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
	if len(resp.Result.Tools) != 14 {
		t.Fatalf("expected 14 tools, got %d", len(resp.Result.Tools))
	}
	for _, name := range []string{"create_agent", "update_agent", "grant_agent", "provision_runner", "revoke_runner", "manage_agent", "world_status", "world_teardown", "world_migrate", "world_build", "world_exec", "world_authorize_door", "world_revoke_door", "world_register_facts"} {
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

// TestServerUpdateAgentValidation pins the dispatch wiring for update_agent:
// the model allowlist is enforced server-side before the flow is invoked, and
// a valid call reaches the bound flow (nil here, so the error names it).
func TestServerUpdateAgentValidation(t *testing.T) {
	aud := "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"
	secret := make([]byte, 32)
	secret[0] = 1
	pk, err := crypto.PubkeyFromSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Audience: aud,
		Grants:   func() ([]string, error) { return []string{pk}, nil },
		Tools:    &agent.Tools{},
	}
	call := func(args string) string {
		raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"update_agent","arguments":` + args + `}}`
		ts := time.Now().Unix()
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
		req.Header.Set(PubkeyHeader, pk)
		req.Header.Set(TSHeader, strconv.FormatInt(ts, 10))
		req.Header.Set(SigHeader, signForTest(secret, aud, ts, raw))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		var resp struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad response for %s: %s", args, rec.Body.String())
		}
		if resp.Error == nil {
			t.Fatalf("expected an error for %s: %s", args, rec.Body.String())
		}
		return resp.Error.Message
	}
	if msg := call(`{"name":"bob","model":"Bogus"}`); !strings.Contains(msg, "model must be one of") {
		t.Fatalf("bad model not refused: %s", msg)
	}
	if msg := call(`{"name":"bob"}`); !strings.Contains(msg, "no update path bound") {
		t.Fatalf("the call did not reach the bound flow: %s", msg)
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

	// world_teardown runs the bound CP-owned teardown driver; the durable
	// agent registry is NOT cleared (the CP + identities survive).
	srv.Tools.WorldTeardownFn = func() (string, error) { return "world-teardown ok", nil }
	td := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"world_teardown","arguments":{}}}`)
	if !strings.Contains(td, "world-teardown ok") {
		t.Fatalf("world_teardown unexpected: %s", td)
	}
	left, _ := ops.Agents()
	if got := len(left); got != 2 {
		t.Fatalf("world_teardown must keep the durable registry, left %d agents", got)
	}

	// world_migrate runs the bound migration runner.
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

// TestWorldExec proves the drive-through-CP exec surface: an operator's
// world_exec runs the command through the Tools.Exec (CP co-located runner)
// driver and echoes its stdout.
func TestWorldExec(t *testing.T) {
	const aud = "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"
	secret := make([]byte, 32)
	secret[0] = 7
	pk, err := crypto.PubkeyFromSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	gotExec := ""
	srv := &Server{
		Audience: aud,
		Grants:   func() ([]string, error) { return []string{pk}, nil },
		Tools: &agent.Tools{
			Console: &fakeOps{},
			Exec: func(target, cmd string, timeoutS uint64, secrets ...string) (string, error) {
				gotExec = target + ":" + cmd
				return "diskspace ok\n", nil
			},
		},
	}
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"world_exec","arguments":{"target":"proxmox-box","cmd":"df -h"}}}`
	ts := time.Now().Unix()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
	req.Header.Set(PubkeyHeader, pk)
	req.Header.Set(SigHeader, signForTest(secret, aud, ts, raw))
	req.Header.Set(TSHeader, strconv.FormatInt(ts, 10))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotExec != "proxmox-box:df -h" {
		t.Fatalf("world_exec did not invoke the CP exec driver, got %q", gotExec)
	}
	if !strings.Contains(rec.Body.String(), "diskspace ok") {
		t.Fatalf("world_exec result missing stdout: %s", rec.Body.String())
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
	msg, denied = post(agentSec, agentPK, "world_exec")
	if !denied || !strings.Contains(msg, "cannot call") {
		t.Fatalf("registry agent must be denied world_exec (operator-scoped exec), got denied=%v msg=%q", denied, msg)
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

// TestProvisionRunnerAgentGate pins the grant-giving carve-out: a registry
// agent (the CPA) may call provision_runner in the default confirm mode — the
// operator-in-thread / DM-confirm discipline lives in the granting skill —
// with the args plumbed to the staging path verbatim; `agent_grants: off` is
// the server-side kill switch that denies EVERY caller.
func TestProvisionRunnerAgentGate(t *testing.T) {
	const aud = "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"
	agentSec := make([]byte, 32)
	agentSec[0] = 9
	agentPK, err := crypto.PubkeyFromSecret(agentSec)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	var got agent.ProvisionArgs
	srv := &Server{
		Audience: aud,
		Grants:   func() ([]string, error) { return []string{agentPK}, nil },
		Tools: &agent.Tools{
			Provision: func(a agent.ProvisionArgs) (string, error) {
				called++
				got = a
				return "runner staged report", nil
			},
		},
		IsAgent: func(pk string) bool { return pk == agentPK },
	}
	call := func(mode string) string {
		t.Helper()
		raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"provision_runner","arguments":` +
			`{"name":"rtx3090-ssh-root","kind":"ssh","address":"darcy@192.168.1.50","grant_to":["ai"],` +
			`"probe":"GET /v2/account bearer","probe_body":"{\"kind\":\"SelfSubjectReview\"}"}}}`
		ts := time.Now().Unix()
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
		req.Header.Set(PubkeyHeader, agentPK)
		req.Header.Set(SigHeader, signForTest(agentSec, aud, ts, raw))
		req.Header.Set(TSHeader, strconv.FormatInt(ts, 10))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Body.String()
	}

	if body := call(""); called != 1 || !strings.Contains(body, "runner staged report") {
		t.Fatalf("confirm-mode (default) must dispatch to the staging path: called=%d body=%s", called, body)
	}
	if got.Name != "rtx3090-ssh-root" || got.Kind != "ssh" || got.Address != "darcy@192.168.1.50" ||
		len(got.GrantTo) != 1 || got.GrantTo[0] != "ai" {
		t.Fatalf("args not plumbed verbatim: %+v", got)
	}
	// The probe + body plumb verbatim — the MCP handler is where a dropped
	// probe refuses every agent-provisioned api door (the args struct and the
	// bridge schema are the two agent-facing copies of the contract).
	if got.Probe != "GET /v2/account bearer" || got.ProbeBody != `{"kind":"SelfSubjectReview"}` {
		t.Fatalf("probe/probe_body not plumbed verbatim: %+v", got)
	}

	srv.AgentGrants = func() string { return "off" }
	if body := call("off"); !strings.Contains(body, "agent_grants is off") {
		t.Fatalf("the kill switch must deny provision_runner: %s", body)
	}
	if called != 1 {
		t.Fatalf("the kill switch must stop the staging path from running (called=%d)", called)
	}
}

// TestRevokeRunnerAgentGate pins the take-away carve-out's server gate: a
// registry agent (the CPA) may call revoke_runner in the default confirm mode,
// its args reach the retirement path verbatim (a silently dropped revoke_from
// would turn a single-grantee removal into a whole-door retirement — the worst
// possible skew between what was asked and what was done), and the SAME
// agent_grants kill switch that denies provisioning also denies revocation.
func TestRevokeRunnerAgentGate(t *testing.T) {
	const aud = "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"
	agentSec := make([]byte, 32)
	agentSec[0] = 11
	agentPK, err := crypto.PubkeyFromSecret(agentSec)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	var got agent.RetireArgs
	srv := &Server{
		Audience: aud,
		Grants:   func() ([]string, error) { return []string{agentPK}, nil },
		Tools: &agent.Tools{
			Revoke: func(a agent.RetireArgs) (string, error) {
				called++
				got = a
				return "door retired report", nil
			},
		},
		IsAgent: func(pk string) bool { return pk == agentPK },
	}
	call := func() string {
		t.Helper()
		raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"revoke_runner","arguments":` +
			`{"name":"rtx3090-ssh-root","revoke_from":["ai"]}}}`
		ts := time.Now().Unix()
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
		req.Header.Set(PubkeyHeader, agentPK)
		req.Header.Set(SigHeader, signForTest(agentSec, aud, ts, raw))
		req.Header.Set(TSHeader, strconv.FormatInt(ts, 10))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Body.String()
	}

	if body := call(); called != 1 || !strings.Contains(body, "door retired report") {
		t.Fatalf("confirm-mode (default) must dispatch to the retirement path: called=%d body=%s", called, body)
	}
	if got.Name != "rtx3090-ssh-root" || len(got.RevokeFrom) != 1 || got.RevokeFrom[0] != "ai" {
		t.Fatalf("args not plumbed verbatim (a lost revoke_from would retire the whole door): %+v", got)
	}

	// revoke_runner must NOT be operator-scoped the way grant_agent is — the CPA
	// is the touchpoint that takes capability away, so the world-tool gate has to
	// stay closed for it (the kill switch above is the only server-side "no").
	if isWorldTool("revoke_runner") {
		t.Fatal("revoke_runner must stay callable by registry agents")
	}

	srv.AgentGrants = func() string { return "off" }
	if body := call(); !strings.Contains(body, "agent_grants is off") {
		t.Fatalf("the kill switch must deny revoke_runner too: %s", body)
	}
	if called != 1 {
		t.Fatalf("the kill switch must stop the retirement path from running (called=%d)", called)
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
					"agents":  []console.AgentInfo{{Name: "cpa", Pubkey: pk}},
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

// TestCreateAgentChannelsAndPrivate: the multi-channel create form reaches the
// deploy path with channels + private, and the single `channel` form is
// prepended to `channels`.
func TestCreateAgentChannelsAndPrivate(t *testing.T) {
	aud := strings.Repeat("ab", 32)
	secret := make([]byte, 32)
	secret[0] = 9
	pk, err := crypto.PubkeyFromSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	var gotChannels []string
	var gotPrivate bool
	var gotModel string
	srv := &Server{
		Audience: aud,
		Grants:   func() ([]string, error) { return []string{pk}, nil },
		Tools: &agent.Tools{Console: &fakeOps{}, Create: func(name, purpose string, channels []string, private bool, model string) (string, error) {
			gotChannels, gotPrivate, gotModel = channels, private, model
			return strings.Repeat("c", 64), nil
		}},
	}
	post := func(raw string) {
		t.Helper()
		ts := time.Now().Unix()
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
		req.Header.Set(PubkeyHeader, pk)
		req.Header.Set(SigHeader, signForTest(secret, aud, ts, raw))
		req.Header.Set(TSHeader, strconv.FormatInt(ts, 10))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != 200 || strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("create_agent failed: %s", rec.Body.String())
		}
	}
	post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_agent","arguments":{"name":"data","channels":["#freehold","#data"],"private":true}}}`)
	if strings.Join(gotChannels, ",") != "#freehold,#data" {
		t.Errorf("channels not passed through: %v", gotChannels)
	}
	if !gotPrivate {
		t.Errorf("private not passed through")
	}
	if gotModel != "" {
		t.Errorf("model must default empty (the deploy path resolves the alias), got %q", gotModel)
	}
	post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_agent","arguments":{"name":"helper","channel":"ops","channels":["#freehold"]}}}`)
	if strings.Join(gotChannels, ",") != "ops,#freehold" {
		t.Errorf("single channel must prepend the list: %v", gotChannels)
	}
	if gotPrivate {
		t.Errorf("private must default false")
	}
}

// TestCreateAgentModelPins: the optional model reaches the deploy path, an
// unknown alias is refused, and the core-only alias (Freehold) is not
// selectable for a custom agent.
func TestCreateAgentModelPins(t *testing.T) {
	aud := strings.Repeat("cd", 32)
	secret := make([]byte, 32)
	secret[0] = 10
	pk, err := crypto.PubkeyFromSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	var gotModel string
	srv := &Server{
		Audience: aud,
		Grants:   func() ([]string, error) { return []string{pk}, nil },
		Tools: &agent.Tools{Console: &fakeOps{}, Create: func(name, purpose string, channels []string, private bool, model string) (string, error) {
			gotModel = model
			return strings.Repeat("d", 64), nil
		}},
	}
	call := func(raw string) string {
		t.Helper()
		ts := time.Now().Unix()
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
		req.Header.Set(PubkeyHeader, pk)
		req.Header.Set(SigHeader, signForTest(secret, aud, ts, raw))
		req.Header.Set(TSHeader, strconv.FormatInt(ts, 10))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if body := call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_agent","arguments":{"name":"coder","model":"Code"}}}`); strings.Contains(body, `"error"`) {
		t.Fatalf("create_agent with model=Code failed: %s", body)
	}
	if gotModel != agent.CodeLiteLLMModel {
		t.Errorf("model not passed through: %q", gotModel)
	}
	if body := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_agent","arguments":{"name":"x","model":"Turbo"}}}`); !strings.Contains(body, "model must be one of") {
		t.Errorf("unknown alias must be refused, got: %s", body)
	}
	if body := call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_agent","arguments":{"name":"x","model":"Freehold"}}}`); !strings.Contains(body, "model must be one of") {
		t.Errorf("the core alias must not be selectable for a custom agent, got: %s", body)
	}
}

// TestConsolePeerScope pins the local-peer path: the console (the CP's
// session-authed operator surface, deliberately NOT a roster member) may call
// world_migrate — whose execution must stay in this process for the registry
// lock — and nothing else. The signature carries the authorization, not the
// roster; an unknown caller is still denied.
func TestConsolePeerScope(t *testing.T) {
	const aud = "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"
	consoleSec := make([]byte, 32)
	consoleSec[0] = 11
	consolePK, err := crypto.PubkeyFromSecret(consoleSec)
	if err != nil {
		t.Fatal(err)
	}
	migrated := false
	srv := &Server{
		Audience:    aud,
		Grants:      func() ([]string, error) { return []string{}, nil }, // peer is NOT on the roster
		Tools:       &agent.Tools{Console: &fakeOps{}},
		ConsolePeer: consolePK,
	}
	srv.Tools.Migrate = func() ([]migrations.Result, error) {
		migrated = true
		return []migrations.Result{{Name: "001-x", OK: true, Applied: true}}, nil
	}
	post := func(secret []byte, pk, tool, arguments string) (string, bool) {
		t.Helper()
		raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + arguments + `}}`
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

	// The console peer runs world_migrate without any roster membership.
	msg, denied := post(consoleSec, consolePK, "world_migrate", "{}")
	if denied {
		t.Fatalf("console peer must call world_migrate, got error: %s", msg)
	}
	if !migrated {
		t.Fatal("console peer world_migrate did not invoke the bound migrator")
	}

	// ...and nothing else.
	msg, denied = post(consoleSec, consolePK, "create_agent", `{"name":"bob"}`)
	if !denied || !strings.Contains(msg, "world_migrate only") {
		t.Fatalf("console peer must be denied create_agent, got denied=%v msg=%q", denied, msg)
	}
	msg, denied = post(consoleSec, consolePK, "world_status", "{}")
	if !denied || !strings.Contains(msg, "world_migrate only") {
		t.Fatalf("console peer must be denied world_status, got denied=%v msg=%q", denied, msg)
	}

	// An unknown caller (neither roster nor peer) still fails closed.
	strangerSec := make([]byte, 32)
	strangerSec[0] = 12
	strangerPK, err := crypto.PubkeyFromSecret(strangerSec)
	if err != nil {
		t.Fatal(err)
	}
	if _, denied = post(strangerSec, strangerPK, "world_migrate", "{}"); !denied {
		t.Fatal("an ungranted non-peer caller must be denied world_migrate")
	}
}

// TestOperatorPeerScope pins the seed's break-glass path: the operator
// (--owner-pubkey) authenticates by signature alone — a peer, never a roster
// member — with full operator scope (create + the world tools). This is also
// what keeps a stale CLI's operator-signed migration sweep working across a
// version jump: the seed revokes the operator's channel membership, and the
// peer rule authorizes the signature regardless.
func TestOperatorPeerScope(t *testing.T) {
	const aud = "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"
	opSec := make([]byte, 32)
	opSec[0] = 13
	opPK, err := crypto.PubkeyFromSecret(opSec)
	if err != nil {
		t.Fatal(err)
	}
	migrated := false
	srv := &Server{
		Audience:     aud,
		Grants:       func() ([]string, error) { return []string{}, nil }, // the operator is NOT on the roster
		Tools:        &agent.Tools{Console: &fakeOps{}},
		OperatorPeer: opPK,
	}
	srv.Tools.Migrate = func() ([]migrations.Result, error) {
		migrated = true
		return []migrations.Result{{Name: "001-x", OK: true, Applied: true}}, nil
	}
	post := func(tool, arguments string) (string, bool) {
		t.Helper()
		raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + arguments + `}}`
		ts := time.Now().Unix()
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
		req.Header.Set(PubkeyHeader, opPK)
		req.Header.Set(SigHeader, signForTest(opSec, aud, ts, raw))
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

	// The operator peer drives the world tools roster-free.
	if _, denied := post("world_migrate", "{}"); denied {
		t.Fatal("operator peer must call world_migrate without roster membership")
	}
	if !migrated {
		t.Fatal("operator peer world_migrate did not invoke the bound migrator")
	}
	if _, denied := post("world_status", "{}"); denied {
		t.Fatal("operator peer must call world_status")
	}
	if _, denied := post("manage_agent", "{}"); denied {
		t.Fatal("operator peer must call manage_agent (full operator scope)")
	}

	// An unknown caller without a peer match still fails closed.
	strangerSec := make([]byte, 32)
	strangerSec[0] = 14
	strangerPK, err := crypto.PubkeyFromSecret(strangerSec)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"world_status","arguments":{}}}`
	ts := time.Now().Unix()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
	req.Header.Set(PubkeyHeader, strangerPK)
	req.Header.Set(SigHeader, signForTest(strangerSec, aud, ts, raw))
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
		t.Fatal("an ungranted non-peer caller must be denied")
	}
}
