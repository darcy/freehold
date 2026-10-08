package cpbuild

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cert "freehold/platform/services/certificates/letsencrypt"

	"freehold/contract/crypto"
	"freehold/control-plane/api/agent"
)

// keyGateway is the fake gateway's /key/generate + /key/delete recorder: every
// mint returns a fresh sk-N and records the body + Authorization header; every
// delete records the body. genStatus lets a refusal be exercisable.
type keyGateway struct {
	mu        sync.Mutex
	genStatus int
	keys      int
	genAuths  []string
	genBodies []string
	delBodies []string
}

func fakeKeyGateway(t *testing.T, genStatus int) (*httptest.Server, *keyGateway) {
	t.Helper()
	st := &keyGateway{genStatus: genStatus}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/key/generate":
			raw, _ := io.ReadAll(r.Body)
			st.mu.Lock()
			st.genAuths = append(st.genAuths, r.Header.Get("Authorization"))
			st.genBodies = append(st.genBodies, string(raw))
			status := st.genStatus
			st.mu.Unlock()
			if status != http.StatusOK {
				w.WriteHeader(status)
				w.Write([]byte("refused"))
				return
			}
			st.mu.Lock()
			st.keys++
			n := st.keys
			st.mu.Unlock()
			fmt.Fprintf(w, `{"key":"sk-%d"}`, n)
		case r.Method == http.MethodPost && r.URL.Path == "/key/delete":
			raw, _ := io.ReadAll(r.Body)
			st.mu.Lock()
			st.delBodies = append(st.delBodies, string(raw))
			st.mu.Unlock()
			w.Write([]byte(`{"deleted_keys":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

// storeEnvForTest reopens the fixture's sealed litellm store — the assertions
// look at the same map the production helpers read and write. Rooted at the
// CONSOLE state dir (the store's ONE home), not the Spec's own StateDir.
func storeEnvForTest(t *testing.T, stateDir string) map[string]string {
	t.Helper()
	consoleRoot := (&Spec{StateDir: stateDir}).consoleStateRoot()
	raw, err := os.ReadFile(consoleRoot + "/console/identity.json")
	if err != nil {
		t.Fatal(err)
	}
	var id struct {
		EncSecretHex string `json:"enc_secret_hex"`
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatal(err)
	}
	secret, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		t.Fatal(err)
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	_, env, err := cert.LoadCreds(consoleRoot+"/world-secrets/litellm.json", open, secret)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// TestEnsureAgentLitellmKey mints once (key_alias = the pod name, models =
// the alias set, Bearer master), persists the returned token in the store and
// reuses it verbatim on the second call — the gateway hands out the raw token
// exactly once, so a re-mint would orphan keys.
func TestEnsureAgentLitellmKey(t *testing.T) {
	srv, st := fakeKeyGateway(t, http.StatusOK)
	root := t.TempDir()
	stateDir := filepath.Join(root, "agent-tools")
	litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
	spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}

	key, err := spec.ensureAgentLitellmKey("Waldo")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if key != "sk-1" {
		t.Fatalf("mint returned %q", key)
	}
	if len(st.genBodies) != 1 {
		t.Fatalf("expected exactly one /key/generate, got %v", st.genBodies)
	}
	if st.genAuths[0] != "Bearer master-key" {
		t.Errorf("mint did not authenticate with the store's master key: %q", st.genAuths[0])
	}
	var body struct {
		KeyAlias string            `json:"key_alias"`
		Models   []string          `json:"models"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(st.genBodies[0]), &body); err != nil {
		t.Fatalf("bad /key/generate body %s: %v", st.genBodies[0], err)
	}
	if body.KeyAlias != "waldo" {
		t.Errorf("key_alias must be the POD name (the Secret + audit identifier), got %q", body.KeyAlias)
	}
	if len(body.Models) != len(agent.LiteLLMAliases) {
		t.Errorf("models must be the alias set (a compromised pod cannot call the raw upstream), got %v", body.Models)
	}
	if body.Metadata["agent"] != "Waldo" {
		t.Errorf("metadata must carry the display agent name, got %v", body.Metadata)
	}

	// The store now holds the minted key under the pod-keyed env entry, and
	// the second call returns the SAME key without a second POST.
	if env := storeEnvForTest(t, stateDir); env[litellmAgentKeyEnvKey("waldo")] != "sk-1" {
		t.Errorf("store record missing: %v", env)
	}
	if key, err := spec.ensureAgentLitellmKey("Waldo"); err != nil || key != "sk-1" {
		t.Fatalf("second ensure: %q, %v", key, err)
	}
	if len(st.genBodies) != 1 {
		t.Errorf("second call re-minted: %v", st.genBodies)
	}
}

// TestEnsureAgentLitellmKeyFailsLoudly: a gateway refusal and a missing store
// both fail the create loudly (a pod referencing a missing Secret never comes
// Ready); a world without a litellm base URL skips silently.
func TestEnsureAgentLitellmKeyFailsLoudly(t *testing.T) {
	t.Run("gateway refusal", func(t *testing.T) {
		srv, _ := fakeKeyGateway(t, http.StatusInternalServerError)
		root := t.TempDir()
		stateDir := filepath.Join(root, "agent-tools")
		litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
		spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}
		if _, err := spec.ensureAgentLitellmKey("waldo"); err == nil {
			t.Fatal("a 500 from /key/generate must fail the mint")
		}
	})
	t.Run("no litellm store", func(t *testing.T) {
		spec := &Spec{StateDir: t.TempDir(), LitellmBaseURL: "http://192.168.30.8:31400/v1"}
		if _, err := spec.ensureAgentLitellmKey("waldo"); err == nil || !strings.Contains(err.Error(), "no CP litellm store") {
			t.Fatalf("missing litellm store must fail loudly, got: %v", err)
		}
	})
	t.Run("no litellm base url", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "agent-tools")
		litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
		spec := &Spec{StateDir: stateDir}
		key, err := spec.ensureAgentLitellmKey("waldo")
		if err != nil || key != "" {
			t.Fatalf("a world without litellm must skip, got %q, %v", key, err)
		}
	})
}

// TestRevokeAgentLitellmKey deletes by the stored TOKEN (the gateway's
// key_alias drifts stale after a rename, so the alias is not a reliable
// delete handle) and drops the store record; a never-minted key and a second
// revoke are no-ops (idempotent ensure-revoked).
func TestRevokeAgentLitellmKey(t *testing.T) {
	srv, st := fakeKeyGateway(t, http.StatusOK)
	root := t.TempDir()
	stateDir := filepath.Join(root, "agent-tools")
	litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
	spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}
	if _, err := spec.ensureAgentLitellmKey("waldo"); err != nil {
		t.Fatalf("mint: %v", err)
	}

	if err := spec.revokeAgentLitellmKey("waldo"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if len(st.delBodies) != 1 || !strings.Contains(st.delBodies[0], `"keys":["sk-1"]`) {
		t.Fatalf("expected one token-keyed /key/delete, got %v", st.delBodies)
	}
	if env := storeEnvForTest(t, stateDir); env[litellmAgentKeyEnvKey("waldo")] != "" {
		t.Errorf("the store record must be dropped: %v", env)
	}

	// Idempotent: the record is gone, so a second revoke never POSTs.
	if err := spec.revokeAgentLitellmKey("waldo"); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if len(st.delBodies) != 1 {
		t.Errorf("second revoke re-POSTed: %v", st.delBodies)
	}
}

// TestMoveLitellmKeyRecord: the renamed agent keeps its key (and spend
// history) — the record re-keys to the new pod name, the old name's record is
// gone, and a mint under the old name again starts fresh.
func TestMoveLitellmKeyRecord(t *testing.T) {
	srv, st := fakeKeyGateway(t, http.StatusOK)
	root := t.TempDir()
	stateDir := filepath.Join(root, "agent-tools")
	litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
	spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}
	if _, err := spec.ensureAgentLitellmKey("waldo"); err != nil {
		t.Fatalf("mint: %v", err)
	}

	if err := spec.moveLitellmKeyRecord("waldo", "waldo2"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if key, err := spec.ensureAgentLitellmKey("waldo2"); err != nil || key != "sk-1" {
		t.Fatalf("renamed agent must reuse the same key, got %q, %v", key, err)
	}
	if len(st.genBodies) != 1 {
		t.Errorf("the rename must not re-mint: %v", st.genBodies)
	}
	if _, err := spec.ensureAgentLitellmKey("waldo"); err != nil {
		t.Fatalf("re-mint under the old name: %v", err)
	}
	if len(st.genBodies) != 2 {
		t.Errorf("a fresh mint under the old pod name was expected: %v", st.genBodies)
	}
}
