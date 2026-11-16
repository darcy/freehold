package cpbuild

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"freehold/contract/crypto"
)

func TestEnsureAgentProfilePublishesOnceAndRepairsOnDifference(t *testing.T) {
	ownerSec := make([]byte, 32)
	ownerSec[31] = 1
	ownerPk, err := crypto.PubkeyFromSecret(ownerSec)
	if err != nil {
		t.Fatal(err)
	}
	agentSec := make([]byte, 32)
	agentSec[31] = 2
	agentPk, err := crypto.PubkeyFromSecret(agentSec)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeFirstRunRelay(t, nil)
	s := &Spec{
		RelayURL:     f.srv.URL,
		RelayAuthURL: f.srv.URL,
		CpHost:       "cp.example",
		OwnerPub:     ownerPk,
		OwnerSecret:  ownerSec,
	}
	if err := s.ensureAgentProfile("freehold", "the control plane agent", agentPk, agentSec); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got := f.published()
	if len(got) != 1 {
		t.Fatalf("published %d, want 1", len(got))
	}
	var ev struct {
		Kind    int        `json:"kind"`
		Pubkey  string     `json:"pubkey"`
		Content string     `json:"content"`
		Tags    [][]string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(got[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != 0 || ev.Pubkey != agentPk {
		t.Fatalf("wrong envelope: kind=%d pubkey=%s", ev.Kind, ev.Pubkey)
	}
	if len(ev.Tags) != 1 || len(ev.Tags[0]) != 4 || ev.Tags[0][0] != "auth" || ev.Tags[0][1] != ownerPk || ev.Tags[0][2] != "kind=0" {
		t.Fatalf("wrong auth tag: %v", ev.Tags)
	}
	if !strings.Contains(ev.Content, `"name":"freehold"`) || !strings.Contains(ev.Content, `"is_agent":true`) {
		t.Fatalf("profile body missing fields: %s", ev.Content)
	}
	if !strings.Contains(ev.Content, `"picture":"https://cp.example/avatars/`+agentPk+`.png"`) {
		t.Fatalf("profile body missing hosted avatar: %s", ev.Content)
	}
	if !strings.Contains(ev.Content, `"nip05":"freehold@cp.example"`) {
		t.Fatalf("profile body missing self-declared nip05: %s", ev.Content)
	}

	// A converged head is not republished.
	if err := s.ensureAgentProfile("freehold", "the control plane agent", agentPk, agentSec); err != nil {
		t.Fatal(err)
	}
	if got := f.published(); len(got) != 1 {
		t.Fatalf("converged profile republished: %d events", len(got))
	}

	// A changed owned field is a real difference.
	if err := s.ensureAgentProfile("freehold", "a changed purpose", agentPk, agentSec); err != nil {
		t.Fatal(err)
	}
	if got := f.published(); len(got) != 2 {
		t.Fatalf("changed purpose must republish, got %d events", len(got))
	}
}

func TestEnsureAgentProfileFailsLoudlyWithoutOwnerOrHost(t *testing.T) {
	agentSec := make([]byte, 32)
	agentSec[31] = 2
	agentPk, _ := crypto.PubkeyFromSecret(agentSec)
	s := &Spec{RelayURL: "http://relay.example", RelayAuthURL: "http://relay.example", CpHost: "cp.example"}
	if err := s.ensureAgentProfile("freehold", "purpose", agentPk, agentSec); err == nil || !strings.Contains(err.Error(), "owner key") {
		t.Fatalf("missing owner key must fail loudly, got %v", err)
	}
	ownerSec := make([]byte, 32)
	ownerSec[31] = 1
	ownerPk, _ := crypto.PubkeyFromSecret(ownerSec)
	s.OwnerPub, s.OwnerSecret = ownerPk, ownerSec
	s.CpHost = ""
	s.RelayHost = ""
	if err := s.ensureAgentProfile("freehold", "purpose", agentPk, agentSec); err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("missing host must fail loudly, got %v", err)
	}
}

func TestEnsureAgentProfileFailsOnRelayRefusal(t *testing.T) {
	ownerSec := make([]byte, 32)
	ownerSec[31] = 1
	ownerPk, _ := crypto.PubkeyFromSecret(ownerSec)
	agentSec := make([]byte, 32)
	agentSec[31] = 2
	agentPk, _ := crypto.PubkeyFromSecret(agentSec)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/query") {
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}
		http.Error(w, "restricted: unknown event kind", http.StatusForbidden)
	}))
	defer srv.Close()
	s := &Spec{
		RelayURL:     srv.URL,
		RelayAuthURL: srv.URL,
		CpHost:       "cp.example",
		OwnerPub:     ownerPk,
		OwnerSecret:  ownerSec,
	}
	if err := s.ensureAgentProfile("freehold", "purpose", agentPk, agentSec); err == nil {
		t.Fatal("a relay refusal must fail the profile publish loudly")
	}
}
