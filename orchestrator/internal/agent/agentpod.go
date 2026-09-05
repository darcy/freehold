package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"freehold/orchestrator/internal/flows"
)

// AgentPod is the deploy spec for a created agent (E1's create-agent, made
// concrete for A4): a named buzz-acp-class identity with its own durable,
// relay-scoped memory — a sprig pod like the CPA but with the new agent's key.
type AgentPod struct {
	Name             string // agent display name
	Purpose          string // one-line purpose (recorded, unused this phase)
	K3sVmid          uint32
	RelayURL         string // wss:// origin
	SystemPromptPath string
	IdentityDir      string // durable identity dir (survives rebuilds)
	OwnerPub         string // respond-to allowlist owner
	LiteLLMKeySecret string // k8s Secret (agents ns) for OPENAI_COMPAT key; "" → own <pod>-litellm-key
	LiteLLMBaseURL   string // reachable litellm base URL (hostNetwork: NodePort, else in-kube)
}

// Prepare mints (or reuses) the agent's durable identity, returns its pubkey,
// and builds the identity-secret + pod-manifest scripts to apply. The caller
// runs the scripts through the runner (the same transport stageLitellm uses).
func (p *AgentPod) Prepare() (pubkey string, identityScript, manifestScript string, err error) {
	if p.Name == "" || p.RelayURL == "" || p.OwnerPub == "" {
		return "", "", "", fmt.Errorf("agent pod needs a name, relay url, and owner pubkey")
	}
	dir := p.IdentityDir
	if dir == "" {
		dir = filepath.Join("agent-" + p.Name) // caller supplies a durable dir
	}
	if err := ensureIdentity(dir); err != nil {
		return "", "", "", err
	}
	id, err := flows.LoadIdentity(dir)
	if err != nil {
		return "", "", "", err
	}
	pubkey, err = id.NostrPubkeyHex()
	if err != nil {
		return "", "", "", err
	}
	base := p.LiteLLMBaseURL
	if base == "" {
		base = LiteLLMServiceURL
	}
	base = strings.TrimSuffix(base, "/") + "/v1"
	keySec := p.LiteLLMKeySecret
	if keySec == "" {
		keySec = sanitizePodName(p.Name) + "-litellm-key"
	}
	return pubkey,
		AgentIdentityScript(p.K3sVmid, id.NostrSecretHex, p.OwnerPub, p.Name),
		AgentManifestScript(p.K3sVmid, p.RelayURL, p.SystemPromptPath, base, CpaLiteLLMModel, p.Name, keySec),
		nil
}

// mintIdentityIn mints a fresh runner-style identity (nostr + enc secrets) in
// dir, persisting it atomically-won on first use. Returns nil (not an error
// when a reuse path raced it — LoadIdentity in ensureIdentity re-checks).
func mintIdentityIn(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("mint identity: %w", err)
	}
	enc := make([]byte, 32)
	if _, err := rand.Read(enc); err != nil {
		return fmt.Errorf("mint identity: %w", err)
	}
	id := flows.Identity{
		NostrSecretHex: hex.EncodeToString(secret),
		EncSecretHex:   hex.EncodeToString(enc),
	}
	raw, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0o600)
}

// EnsureIdentity mints an identity in dir on first use and returns its pubkey;
// reuses the recorded identity on later calls (identity continuity across
// rebuilds). Shared by the CPA stage and the create-agent tool.
func EnsureIdentity(dir string) (string, error) {
	if err := ensureIdentity(dir); err != nil {
		return "", err
	}
	id, err := flows.LoadIdentity(dir)
	if err != nil {
		return "", err
	}
	return id.NostrPubkeyHex()
}

// ensureIdentity mints a runner-style identity in dir if absent, returns nil.
func ensureIdentity(dir string) error {
	if _, err := flows.LoadIdentity(dir); err == nil {
		return nil
	}
	// Mint on demand (same shape as min identity in the rebuild engine's
	// stageCpa). The identity must survive compute-only teardown, so the
	// caller passes a durable dir (under the CP's durable-plane area).
	return mintIdentityIn(dir)
}
