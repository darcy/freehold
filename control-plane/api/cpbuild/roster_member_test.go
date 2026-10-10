package cpbuild

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"freehold/contract/identity"
	"freehold/contract/wire"

	"freehold/control-plane/api/agent"
)

// fakeRelay stores published events and answers /query by kinds + #h + #p —
// the subset of the buzz HTTP bridge memberAgentToolsRoster exercises. NIP-98
// auth is contract-level (covered there); the fake skips it.
type fakeRelay struct {
	mu     sync.Mutex
	events []map[string]interface{}
	srv    *httptest.Server
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	f := &fakeRelay{}
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		var ev map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.mu.Lock()
		f.events = append(f.events, ev)
		f.mu.Unlock()
		w.WriteHeader(200)
	})
	mux.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
		var filters []map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&filters); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []map[string]interface{}{}
		for _, ev := range f.events {
			if membershipFilterMatches(ev, filters) {
				out = append(out, ev)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// membershipFilterMatches checks the fake's only supported filter shape:
// kinds + #h + #p (the membership query IsMemberAuth sends).
func membershipFilterMatches(ev map[string]interface{}, filters []map[string]interface{}) bool {
	if len(filters) == 0 {
		return true
	}
	f := filters[0]
	if kinds, ok := f["kinds"].([]interface{}); ok {
		matched := false
		for _, k := range kinds {
			if fk, ok := k.(float64); ok && uint32(fk) == uint32(ev["kind"].(float64)) {
				matched = true
			}
		}
		if !matched {
			return false
		}
	}
	for _, tag := range []string{"h", "p"} {
		want, ok := f["#"+tag].([]interface{})
		if !ok {
			continue
		}
		matched := false
		for _, w := range want {
			if ws, _ := w.(string); ws == evTag(ev, tag) {
				matched = true
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func evTag(ev map[string]interface{}, name string) string {
	tags, _ := ev["tags"].([]interface{})
	for _, t := range tags {
		tt, ok := t.([]interface{})
		if ok && len(tt) > 1 {
			if s, ok := tt[0].(string); ok && s == name {
				v, _ := tt[1].(string)
				return v
			}
		}
	}
	return ""
}

// TestMemberAgentToolsRoster: EVERY created agent is membered into the
// agent-tools server's own roster (signed by that identity, not the
// console's), and an existing member is not re-put (membership writes are
// state-idempotent but not event-idempotent; the reconcile re-runs the create
// on every build).
func TestMemberAgentToolsRoster(t *testing.T) {
	f := newFakeRelay(t)

	// Mint the agent-tools identity (the roster's owner) into a temp state
	// dir shaped like agentToolsRoot() infers it: <root>/agent-tools.
	root := t.TempDir()
	atRoot := filepath.Join(root, "agent-tools")
	if _, err := agent.EnsureIdentity(atRoot); err != nil {
		t.Fatalf("ensure agent-tools identity: %v", err)
	}
	atID, err := identity.Load(atRoot)
	if err != nil {
		t.Fatalf("load agent-tools identity: %v", err)
	}
	atPub, err := atID.NostrPubkeyHex()
	if err != nil {
		t.Fatalf("agent-tools pubkey: %v", err)
	}

	spec := &Spec{StateDir: filepath.Join(root, "control-plane"), RelayURL: f.srv.URL}

	// The created agents' identity dirs — create members the minted pubkey,
	// remove reads it back from here.
	dirA := filepath.Join(spec.agentIdentityDir(), "agents", "freehold")
	dirB := filepath.Join(spec.agentIdentityDir(), "agents", "network")
	pubA, err := agent.EnsureIdentity(dirA)
	if err != nil {
		t.Fatalf("ensure freehold identity: %v", err)
	}
	pubB, err := agent.EnsureIdentity(dirB)
	if err != nil {
		t.Fatalf("ensure network identity: %v", err)
	}
	for _, m := range []struct{ name, pub string }{
		{"freehold", pubA}, // the CPA
		{"network", pubB},  // a department — the membering is not CPA-only
	} {
		if err := spec.memberAgentToolsRoster(m.name, m.pub); err != nil {
			t.Fatalf("member %s: %v", m.name, err)
		}
	}

	f.mu.Lock()
	if len(f.events) != 2 {
		t.Fatalf("got %d events after membering two agents, want 2", len(f.events))
	}
	for _, ev := range f.events {
		if uint32(ev["kind"].(float64)) != wire.PutUser {
			t.Errorf("event kind %v, want put-user", ev["kind"])
		}
		if ev["pubkey"] != atPub {
			t.Errorf("put-user signed by %v, want the agent-tools identity %s (a console-signed put-user reads 'not a channel member')", ev["pubkey"], atPub)
		}
		p := evTag(ev, "p")
		if p != pubA && p != pubB {
			t.Errorf("put-user p tag = %v, want a membered agent", p)
		}
	}
	f.mu.Unlock()

	// A member is not re-put.
	if err := spec.memberAgentToolsRoster("network", pubB); err != nil {
		t.Fatalf("re-member: %v", err)
	}
	f.mu.Lock()
	if len(f.events) != 2 {
		t.Errorf("re-member re-put: %d events, want 2 (already a member)", len(f.events))
	}
	f.mu.Unlock()

	// The revoke leg: a removed agent's seat is published as a remove-user,
	// signed by the agent-tools identity, and the roster read flips.
	seat, err := spec.removeAgentToolsRoster("network")
	if err != nil {
		t.Fatalf("remove roster seat: %v", err)
	}
	f.mu.Lock()
	removes := 0
	for _, ev := range f.events {
		if uint32(ev["kind"].(float64)) == wire.RemoveUser {
			removes++
			if ev["pubkey"] != atPub {
				t.Errorf("remove-user signed by %v, want the agent-tools identity", ev["pubkey"])
			}
			if evTag(ev, "p") != pubB {
				t.Errorf("remove-user p tag = %v, want the removed agent", evTag(ev, "p"))
			}
		}
	}
	f.mu.Unlock()
	if removes != 1 {
		t.Fatalf("got %d remove-user events, want 1", removes)
	}
	if !strings.Contains(seat, "verified") {
		t.Errorf("seat report %q — the read-back should verify the removal", seat)
	}
}
