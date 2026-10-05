package cpbuild

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	cert "freehold/platform/services/certificates/letsencrypt"

	"freehold/contract/crypto"
	"freehold/control-plane/api/agent"
)

// litellmFixture seeds a Spec's sealed litellm store (console identity + the
// master/provider keys + the provider choice, AAD "litellm" — the box's
// CPSecretBlob shape). Empty prefix/model seeds a store that predates the
// choice (the freehold-default fallback path).
func litellmFixture(t *testing.T, stateDir, master, provider, prefix, model string) {
	t.Helper()
	secret := make([]byte, 32)
	secret[0] = 7
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir+"/console", 0o700); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf(`{"enc_secret_hex":%q}`, hex.EncodeToString(secret))
	if err := os.WriteFile(stateDir+"/console/identity.json", []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"master": master, "provider": provider}
	if prefix != "" || model != "" {
		env["provider-prefix"], env["provider-model"] = prefix, model
	}
	if err := cert.SaveCreds(stateDir+"/world-secrets/litellm.json", "litellm", env,
		crypto.Seal, pub, "litellm"); err != nil {
		t.Fatal(err)
	}
}

// fakeGateway answers /health/liveliness 200 (the stage's readiness wait) and
// /model/info with every name pre-registered plus every name /model/new has
// registered; it records every /model/new body. newStatus lets failures be
// exercisable.
func fakeGateway(t *testing.T, pre map[string]string, newStatus int) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	registered := pre
	if registered == nil {
		registered = map[string]string{}
	}
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health/liveliness":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/model/info":
			mu.Lock()
			names := slices.Sorted(maps.Keys(registered))
			mu.Unlock()
			var out []string
			for _, n := range names {
				out = append(out, fmt.Sprintf(`{"model_name":%q,"litellm_params":{"model":%q,"api_key":"redacted"}}`, n, registered[n]))
			}
			w.Write([]byte("[" + strings.Join(out, ",") + "]"))
		case r.Method == http.MethodPost && r.URL.Path == "/model/new":
			raw, _ := io.ReadAll(r.Body)
			if got := r.Header.Get("Authorization"); got != "Bearer master-key" {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte("bad master key"))
				return
			}
			mu.Lock()
			posts = append(posts, string(raw))
			mu.Unlock()
			var body struct {
				ModelName string `json:"model_name"`
			}
			_ = json.Unmarshal(raw, &body)
			if newStatus != http.StatusOK {
				w.WriteHeader(newStatus)
				w.Write([]byte("refused"))
				return
			}
			mu.Lock()
			registered[body.ModelName] = "upstream"
			mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), posts...)
	}
}

// TestStageLitellmAliases registers every alias pointing at the store's
// recorded provider choice, then is a no-op on the second run (and never
// re-POSTs a name already on the gateway).
func TestStageLitellmAliases(t *testing.T) {
	srv, postsFn := fakeGateway(t, nil, http.StatusOK)
	defer srv.Close()

	stateDir := t.TempDir()
	litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
	spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}

	if err := spec.stageLitellmAliases(); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(postsFn()) != len(agent.LiteLLMAliases) {
		t.Fatalf("registered %d aliases, want %d: %v", len(postsFn()), len(agent.LiteLLMAliases), postsFn())
	}
	seen := map[string]bool{}
	for _, raw := range postsFn() {
		var body struct {
			ModelName     string `json:"model_name"`
			LitellmParams struct {
				Model  string `json:"model"`
				APIKey string `json:"api_key"`
			} `json:"litellm_params"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatalf("bad /model/new body %s: %v", raw, err)
		}
		seen[body.ModelName] = true
		if body.LitellmParams.APIKey != "provider-key" {
			t.Errorf("alias %s: api_key not the sealed provider key", body.ModelName)
		}
		if body.LitellmParams.Model != "openai/gpt-4o" {
			t.Errorf("alias %s: not riding the recorded provider choice: %q", body.ModelName, body.LitellmParams.Model)
		}
	}
	for _, alias := range agent.LiteLLMAliases {
		if !seen[alias] {
			t.Errorf("alias %s not registered", alias)
		}
	}

	// Idempotent: every name is on the gateway now — the second run no-ops.
	if err := spec.stageLitellmAliases(); err != nil {
		t.Fatalf("second stage: %v", err)
	}
	if len(postsFn()) != len(agent.LiteLLMAliases) {
		t.Errorf("second run re-registered aliases: %d posts", len(postsFn()))
	}

	// A name already on the gateway is never re-POSTed (a deliberate retarget
	// is the AI department's work, not a build side effect).
	srv2, postsFn2 := fakeGateway(t, map[string]string{"Code": "upstream/other"}, http.StatusOK)
	defer srv2.Close()
	spec2 := &Spec{StateDir: stateDir, LitellmBaseURL: srv2.URL + "/v1"}
	if err := spec2.stageLitellmAliases(); err != nil {
		t.Fatalf("stage with pre-registered alias: %v", err)
	}
	for _, raw := range postsFn2() {
		if strings.Contains(raw, `"model_name":"Code"`) {
			t.Errorf("Code was re-POSTed despite being registered: %s", raw)
		}
	}
}

// TestParseLitellmModelsBothShapes: /model/info returns a bare list on older
// litellm and {"data": [...]} on newer ones (the main-stable tag floats) —
// both decode, and neither-neither is an error.
func TestParseLitellmModelsBothShapes(t *testing.T) {
	entry := `{"model_name":"m","litellm_params":{"model":"upstream/m"}}`
	for name, body := range map[string]string{
		"bare list":       `[` + entry + `]`,
		"wrapped in data": `{"data":[` + entry + `]}`,
	} {
		models, err := parseLitellmModels([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(models) != 1 || models[0].ModelName != "m" || models[0].LitellmParams.Model != "upstream/m" {
			t.Errorf("%s: bad parse: %+v", name, models)
		}
	}
	if _, err := parseLitellmModels([]byte(`{"unexpected": true}`)); err == nil {
		t.Error("a body of neither shape must error")
	}
}

// TestStageLitellmAliasesLegacyStore: a store that predates the provider
// choice (no provider-prefix/provider-model) falls back to the freehold
// default (fireworks glm) rather than failing the build.
func TestStageLitellmAliasesLegacyStore(t *testing.T) {
	srv, postsFn := fakeGateway(t, nil, http.StatusOK)
	defer srv.Close()

	stateDir := t.TempDir()
	litellmFixture(t, stateDir, "master-key", "provider-key", "", "")
	spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}
	if err := spec.stageLitellmAliases(); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(postsFn()) != len(agent.LiteLLMAliases) {
		t.Fatalf("registered %d aliases, want %d", len(postsFn()), len(agent.LiteLLMAliases))
	}
	for _, raw := range postsFn() {
		if !strings.Contains(raw, `"model":"fireworks_ai/accounts/fireworks/models/glm-5p3-flash"`) {
			t.Errorf("legacy store must fall back to the fireworks default: %s", raw)
		}
	}
}

// TestStageLitellmAliasesFailsLoudly: a gateway refusal must fail the build
// (an unregistered alias is a 400ing agent), as must a missing litellm store;
// a world without litellm skips silently.
func TestStageLitellmAliasesFailsLoudly(t *testing.T) {
	newStateDir := func(t *testing.T) string {
		t.Helper()
		stateDir := t.TempDir()
		litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
		return stateDir
	}

	t.Run("gateway refusal", func(t *testing.T) {
		srv, _ := fakeGateway(t, nil, http.StatusInternalServerError)
		defer srv.Close()
		spec := &Spec{StateDir: newStateDir(t), LitellmBaseURL: srv.URL + "/v1"}
		if err := spec.stageLitellmAliases(); err == nil {
			t.Fatal("a 500 from /model/new must fail the stage")
		}
	})
	t.Run("no litellm store", func(t *testing.T) {
		spec := &Spec{StateDir: t.TempDir(), LitellmBaseURL: "http://192.168.30.8:31400/v1"}
		if err := spec.stageLitellmAliases(); err == nil || !strings.Contains(err.Error(), "no CP litellm store") {
			t.Fatalf("missing litellm store must fail loudly, got: %v", err)
		}
	})
	t.Run("no litellm base url", func(t *testing.T) {
		spec := &Spec{StateDir: newStateDir(t)}
		if err := spec.stageLitellmAliases(); err != nil {
			t.Fatalf("a world without litellm must skip, got: %v", err)
		}
	})
}
