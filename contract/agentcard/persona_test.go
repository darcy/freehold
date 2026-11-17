package agentcard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"freehold/contract/crypto"
)

func goodPersona() Persona {
	return Persona{
		DisplayName:  "freehold",
		SystemPrompt: "You are freehold, the control plane agent. Help the operator plainly.",
		ACPCommand:   "buzz-acp",
		AvatarURL:    "https://cp.example/avatars/" + strings.Repeat("ab", 32) + ".png",
		Runtime:      "buzz-agent",
		RespondTo:    "owner-only",
		Parallelism:  1,
		Description:  "the control plane agent",
	}
}

func TestValidateGoodPersona(t *testing.T) {
	if err := Validate(goodPersona()); err != nil {
		t.Fatalf("good card rejected: %v", err)
	}
}

func TestValidateRejectsUnsafeCards(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Persona)
		want string
	}{
		{"empty name", func(p *Persona) { p.DisplayName = "" }, "display_name"},
		{"invisible name", func(p *Persona) { p.DisplayName = "free\u200Bhold" }, "visible"},
		{"bad avatar", func(p *Persona) { p.AvatarURL = "http://cp.example/a.png" }, "avatar"},
		{"allowlist", func(p *Persona) { p.RespondTo = "allowlist" }, "allowlist"},
		{"allowlist values", func(p *Persona) { p.RespondToAllowlist = []string{strings.Repeat("a", 64)} }, "allowlist"},
		{"bad acp", func(p *Persona) { p.ACPCommand = "/usr/local/bin/goose" }, "acp_command"},
		{"bad parallelism", func(p *Persona) { p.Parallelism = 64 }, "parallelism"},
		{"secret in prompt", func(p *Persona) { p.SystemPrompt = "use sk-" + strings.Repeat("a", 32) }, "credential-like"},
		{"private key", func(p *Persona) { p.SystemPrompt = "-----BEGIN PRIVATE KEY-----" }, "credential-like"},
		{"long content", func(p *Persona) { p.SystemPrompt = strings.Repeat("x", MaxContentBytes+1) }, "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := goodPersona()
			tc.edit(&p)
			if err := Validate(p); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidSlug(t *testing.T) {
	for _, ok := range []string{"freehold", "product", "a-b_c", strings.Repeat("a", 64)} {
		if !ValidSlug(ok) {
			t.Errorf("slug %q rejected", ok)
		}
	}
	for _, bad := range []string{"", "-freehold", "Freehold", "a b", strings.Repeat("a", 65)} {
		if ValidSlug(bad) {
			t.Errorf("slug %q accepted", bad)
		}
	}
}

func TestBodyWireOrderAndShape(t *testing.T) {
	p := Persona{
		DisplayName:  "Test Agent",
		SystemPrompt: "You are a test assistant.",
		AvatarURL:    "https://example.com/avatar.png",
		Runtime:      "goose",
		Model:        "claude-opus-4",
		Provider:     "anthropic",
		NamePool:     []string{"Alpha", "Beta"},
	}
	body, err := Body(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"display_name":"Test Agent","system_prompt":"You are a test assistant.","avatar_url":"https://example.com/avatar.png","runtime":"goose","model":"claude-opus-4","provider":"anthropic","name_pool":["Alpha","Beta"]}`
	if string(body) != want {
		t.Fatalf("body =\n%s\nwant\n%s", body, want)
	}
}

func TestBuildEventJSONShape(t *testing.T) {
	ownerSec := make([]byte, 32)
	ownerSec[31] = 1
	ownerPk, _ := crypto.PubkeyFromSecret(ownerSec)
	evJSON, err := BuildEventJSON(goodPersona(), "freehold", ownerSec, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Kind    int        `json:"kind"`
		Pubkey  string     `json:"pubkey"`
		Tags    [][]string `json:"tags"`
		Content string     `json:"content"`
	}
	if err := json.Unmarshal([]byte(evJSON), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != PersonaKind || ev.Pubkey != ownerPk {
		t.Fatalf("wrong envelope: %+v", ev)
	}
	if len(ev.Tags) != 2 || ev.Tags[0][0] != "d" || ev.Tags[0][1] != "freehold" || ev.Tags[1][0] != "shared" || ev.Tags[1][1] != "true" {
		t.Fatalf("wrong tags: %v", ev.Tags)
	}
	if !strings.Contains(ev.Content, `"display_name":"freehold"`) {
		t.Fatalf("wrong content: %s", ev.Content)
	}
}

func TestPersonaMatches(t *testing.T) {
	p := goodPersona()
	body, _ := Body(p)
	tags := [][]string{{"d", "freehold"}, {"shared", "true"}}
	if !PersonaMatches(string(body), tags, p, "freehold") {
		t.Fatal("body/tags must match their own projection")
	}
	if !PersonaMatches(`{"display_name":"freehold","system_prompt":"You are freehold, the control plane agent. Help the operator plainly.","acp_command":"buzz-acp","avatar_url":"https://cp.example/avatars/`+strings.Repeat("ab", 32)+`.png","runtime":"buzz-agent","respond_to":"owner-only","parallelism":1,"description":"the control plane agent","extra":"kept"}`, tags, p, "freehold") {
		t.Fatal("extra fields must not force a republish")
	}
	p.Description = "changed"
	if PersonaMatches(string(body), tags, p, "freehold") {
		t.Fatal("a changed owned field must republish")
	}
	if PersonaMatches(string(body), [][]string{{"d", "freehold"}}, goodPersona(), "freehold") {
		t.Fatal("a missing shared tag must republish")
	}
	if PersonaMatches(string(body), [][]string{{"d", "other"}, {"shared", "true"}}, goodPersona(), "freehold") {
		t.Fatal("a wrong d tag must republish")
	}
}

func TestPublishAndQueryPersona(t *testing.T) {
	ownerSec := make([]byte, 32)
	ownerSec[31] = 1
	ownerPk, _ := crypto.PubkeyFromSecret(ownerSec)
	var published []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			var ev map[string]interface{}
			_ = json.Unmarshal(body, &ev)
			published = append(published, ev)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/query"):
			_ = json.NewEncoder(w).Encode(published)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := goodPersona()
	if err := PublishPersonaAt(srv.URL, srv.URL, ownerSec, p, "freehold", 1700000000); err != nil {
		t.Fatalf("publish: %v", err)
	}
	content, tags, ts, err := QueryPersonaAuth(srv.URL, srv.URL, ownerSec, ownerPk, "freehold")
	if err != nil {
		t.Fatal(err)
	}
	if content == "" || ts != 1700000000 {
		t.Fatalf("query returned content=%q ts=%d", content, ts)
	}
	if !PersonaMatches(content, tags, p, "freehold") {
		t.Fatalf("published card does not match: %v", tags)
	}
}
