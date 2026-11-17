package cpbuild

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"freehold/contract/crypto"
	"freehold/control-plane/api/agenttools"
)

func TestPublishShowcaseCardsCanaryAndCompare(t *testing.T) {
	ownerSec := make([]byte, 32)
	ownerSec[31] = 1
	ownerPk, _ := crypto.PubkeyFromSecret(ownerSec)
	cpaSec := make([]byte, 32)
	cpaSec[31] = 2
	cpaPk, _ := crypto.PubkeyFromSecret(cpaSec)
	productSec := make([]byte, 32)
	productSec[31] = 3
	productPk, _ := crypto.PubkeyFromSecret(productSec)

	reg, err := agenttools.OpenRegistry(t.TempDir() + "/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RegisterAgent("freehold", cpaPk, "#freehold"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RegisterAgent("product", productPk, "#freehold"); err != nil {
		t.Fatal(err)
	}
	f := newFakeFirstRunRelay(t, nil)
	s := &Spec{
		RelayURL:     f.srv.URL,
		RelayAuthURL: f.srv.URL,
		CpHost:       "cp.example",
		CpaName:      "freehold",
		OwnerPub:     ownerPk,
		OwnerSecret:  ownerSec,
	}
	if err := s.publishShowcaseCards(reg); err != nil {
		t.Fatalf("publish showcase: %v", err)
	}
	got := f.published()
	if len(got) != 2 {
		t.Fatalf("published %d cards, want 2 (freehold canary then product)", len(got))
	}
	check := func(raw, slug, wantAvatar string, wantPrompt string) {
		var ev struct {
			Kind    int        `json:"kind"`
			Pubkey  string     `json:"pubkey"`
			Tags    [][]string `json:"tags"`
			Content string     `json:"content"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Kind != 30175 || ev.Pubkey != ownerPk {
			t.Fatalf("%s wrong envelope: %+v", slug, ev)
		}
		if len(ev.Tags) != 2 || ev.Tags[0][0] != "d" || ev.Tags[0][1] != slug || ev.Tags[1][0] != "shared" || ev.Tags[1][1] != "true" {
			t.Fatalf("%s wrong tags: %v", slug, ev.Tags)
		}
		var body map[string]interface{}
		if err := json.Unmarshal([]byte(ev.Content), &body); err != nil {
			t.Fatal(err)
		}
		if body["display_name"] != slug {
			t.Fatalf("%s display_name = %v", slug, body["display_name"])
		}
		if body["avatar_url"] != wantAvatar {
			t.Fatalf("%s avatar = %v, want %s", slug, body["avatar_url"], wantAvatar)
		}
		if !strings.Contains(ev.Content, wantPrompt) {
			t.Fatalf("%s must carry its short showcase prompt", slug)
		}
		if strings.Contains(ev.Content, "## Purpose") || strings.Contains(ev.Content, "**What makes this role unusual:**") {
			t.Fatalf("%s leaked a full repo prompt", slug)
		}
	}
	check(got[0], "freehold", "https://cp.example/avatars/"+cpaPk+".png", "delegate expert work")
	check(got[1], "product", "https://cp.example/avatars/"+productPk+".png", "own the thesis")

	// A converged pair is not republished.
	if err := s.publishShowcaseCards(reg); err != nil {
		t.Fatal(err)
	}
	if got := f.published(); len(got) != 2 {
		t.Fatalf("converged cards republished: %d events", len(got))
	}
}

func TestPublishShowcaseCardsFallsBackForMissingProduct(t *testing.T) {
	ownerSec := make([]byte, 32)
	ownerSec[31] = 1
	ownerPk, _ := crypto.PubkeyFromSecret(ownerSec)
	reg, err := agenttools.OpenRegistry(t.TempDir() + "/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeFirstRunRelay(t, nil)
	s := &Spec{
		RelayURL:     f.srv.URL,
		RelayAuthURL: f.srv.URL,
		CpHost:       "cp.example",
		CpaName:      "freehold",
		OwnerPub:     ownerPk,
		OwnerSecret:  ownerSec,
	}
	if err := s.publishShowcaseCards(reg); err != nil {
		t.Fatal(err)
	}
	got := f.published()
	if len(got) != 2 {
		t.Fatalf("published %d cards, want 2", len(got))
	}
	var ev struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(got[1]), &ev); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ev.Content, "/avatars/"+avatarSeed("product")+".png") {
		t.Fatalf("missing product row must use the deterministic seed avatar: %s", ev.Content)
	}
}

func TestPublishShowcaseCardsFailsOnRelayRefusal(t *testing.T) {
	ownerSec := make([]byte, 32)
	ownerSec[31] = 1
	ownerPk, _ := crypto.PubkeyFromSecret(ownerSec)
	reg, err := agenttools.OpenRegistry(t.TempDir() + "/registry.json")
	if err != nil {
		t.Fatal(err)
	}
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
		CpaName:      "freehold",
		OwnerPub:     ownerPk,
		OwnerSecret:  ownerSec,
	}
	if err := s.publishShowcaseCards(reg); err == nil || !strings.Contains(err.Error(), "freehold") {
		t.Fatalf("a relay refusal must fail the canary loudly, got %v", err)
	}
}
