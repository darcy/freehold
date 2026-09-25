package agent

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"freehold/agents"
)

// systemPrompt is the real multi-line prompt exactly as production ships it:
// stageCpa passes agents.CPASystemPrompt("") into AgentManifestScript, so the
// test uses that same composed value — the old tests passed a path string,
// which is why the block-scalar bug compiled.
func systemPrompt() string {
	return agents.CPASystemPrompt("")
}

func TestCPAPodManifestBasics(t *testing.T) {
	sp := systemPrompt()
	m := CPAPodManifest("waldo", "wss://relay.test", sp)
	for _, want := range []string{
		"kind: Pod",
		"kind: ConfigMap",
		"name: waldo",
		"namespace: agents",
		"image: ghcr.io/block/buzz-sprig:main",
		"exec buzz-acp",
		"BUZZ_RELAY_URL",
		`value: "wss://relay.test"`,
		"BUZZ_ACP_SYSTEM_PROMPT_FILE",
		SystemPromptPath,
		// D1: the CPA reaches its reasoning model through the litellm gateway
		// as an OpenAI-compatible endpoint (alias ControlPlaneAgent).
		"BUZZ_AGENT_PROVIDER",
		`value: "openai-compat"`,
		"OPENAI_COMPAT_BASE_URL",
		LiteLLMServiceURL,
		"OPENAI_COMPAT_MODEL",
		CpaLiteLLMModel,
		"BUZZ_ACP_AGENT_COMMAND",
		`value: "buzz-agent"`,
		"restartPolicy: Never",
		// The agent joins its own relay over the LAN (pod-CNI egress to
		// external LAN IPs is often blocked); hostNetwork makes resolution +
		// reachability go through the node exactly like any guest.
		"hostNetwork: true",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q", want)
		}
	}
	// The manifest must apply: every doc parses and the ConfigMap's block
	// scalar (which embeds the real multi-line prompt) parses as one string.
	docs := strings.Split(m, "\n---\n")
	if len(docs) != 3 {
		t.Fatalf("manifest has %d YAML docs, want 3", len(docs))
	}
	var cm struct {
		Kind string            `yaml:"kind"`
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(docs[0]), &cm); err != nil {
		t.Fatalf("ConfigMap doc does not parse: %v", err)
	}
	if cm.Kind != "ConfigMap" {
		t.Fatalf("first doc is %q, want ConfigMap", cm.Kind)
	}
	if got := cm.Data[SystemPromptFile]; got != strings.TrimRight(sp, "\n") {
		t.Errorf("ConfigMap content differs from the prompt file")
	}
	// The nsec must NEVER be embedded in the manifest (it rides the Secret).
	if strings.Contains(m, "BUZZ_PRIVATE_KEY") {
		if strings.Contains(m, "value:") && !strings.Contains(m, "secretKeyRef") {
			t.Errorf("manifest embeds a private-key literal")
		}
	}
	// The identity Secret must be agent-specific (waldo-identity), derived
	// from the sanitized name — never a shared/fixed name.
	if !strings.Contains(m, "secretKeyRef: {name: waldo-identity, key: nsec}") {
		t.Errorf("manifest missing the agent-specific identity Secret ref")
	}
	// The CPA's inbound author gate is "anyone" — relay membership is the
	// bound (the CPA is the system's main touchpoint and every agent's
	// delegate) — so no allowlist env rides its manifest.
	if !strings.Contains(m, `name: BUZZ_ACP_RESPOND_TO, value: "anyone"`) {
		t.Errorf("CPA manifest must run the anyone gate")
	}
	if strings.Contains(m, "BUZZ_ACP_RESPOND_TO_ALLOWLIST") {
		t.Errorf("anyone gate must not carry an allowlist")
	}
	// The litellm key must also ride a per-agent Secret (waldo-litellm-key),
	// never a literal in the manifest.
	if !strings.Contains(m, "OPENAI_COMPAT_API_KEY") {
		t.Errorf("manifest missing OPENAI_COMPAT_API_KEY")
	}
	if !strings.Contains(m, "secretKeyRef: {name: waldo-litellm-key, key: key}") {
		t.Errorf("litellm key must come from the waldo-litellm-key Secret, not a literal")
	}
	if strings.Contains(m, "sk-") || strings.Contains(m, "Bearer ") {
		t.Errorf("manifest embeds a litellm key literal")
	}
	// The display name must appear only as the agent-name annotation.
	if !strings.Contains(m, "freehold.fh/agent-name: waldo") {
		t.Errorf("manifest missing the agent-name annotation %q", "waldo")
	}
	// I5: no supervisor resurrects an intentional exit — the pod must be a
	// bare v1 Pod with restartPolicy Never, not a Deployment.
	if strings.Contains(m, "kind: Deployment") {
		t.Errorf("agent must be a bare Pod, not a Deployment")
	}
	if strings.Contains(m, "restartPolicy: Always") {
		t.Errorf("restartPolicy must be Never (I5: intentional exit stays terminal)")
	}
}

// TestAgentPodManifestDistinctNames: a second agent must own DIFFERENT k8s
// objects (pod + secret + service) than the first — deploying two agents must
// never apply over each other.
func TestAgentPodManifestDistinctNames(t *testing.T) {
	alice := CPAPodManifest("alice", "wss://relay.test", "/p/x.md")
	bob := CPAPodManifest("bob", "wss://relay.test", "/p/x.md")
	for _, want := range []string{"name: alice", "alice-identity", "name: bob", "bob-identity"} {
		if !strings.Contains(alice, want) && !strings.Contains(bob, want) {
			t.Errorf("neither manifest contains %q", want)
		}
	}
	// alice must never reference bob's objects and vice versa.
	if strings.Contains(alice, "bob-identity") || strings.Contains(bob, "alice-identity") {
		t.Errorf("agent identity secrets must be distinct per agent")
	}
}

// TestAgentPodManifestMultiRunner pins the multi-runner pod wiring: every
// capability runner's coords ride as ALIGNED comma lists (env + the bridge
// conf file), one entry per runner, so a department pod can reach each of its
// capability doors.
func TestAgentPodManifestMultiRunner(t *testing.T) {
	runners := []RunnerCoords{
		{URL: "http://10.0.0.5:8791", Pubkey: "pkA", Target: "pve-ssh-root", Secret: "pve-ssh-root"},
		{URL: "http://10.0.0.5:8793", Pubkey: "pkB", Target: "kube-api-caddysa", Secret: "kube-api-caddysa"},
	}
	m := AgentPodManifest("network", "wss://relay.test", "/p/x.md", "http://gw:31400/v1", "m", "k", "http://at:8080", "atpk", "allowlist", "op,cpa", runners...)
	urls, pubs, targets, secrets := runnerLists(runners)
	for _, want := range []string{
		`value: "` + urls + `"`,
		`value: "` + pubs + `"`,
		`value: "` + targets + `"`,
		`value: "` + secrets + `"`,
		"runner_urls=" + urls,
		"runner_targets=" + targets,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q", want)
		}
	}
	// The old singular env must be gone — the bridge parses lists only.
	for _, gone := range []string{"FREEHOLD_RUNNER_URL,", "FREEHOLD_RUNNER_TARGET,"} {
		if strings.Contains(m, gone) {
			t.Errorf("manifest still carries singular runner env %q", gone)
		}
	}
}

// TestAgentPodRespondGate pins the inbound author gate wiring: the CPA runs
// "anyone" (relay membership is the bound); every other agent runs an explicit
// "allowlist" whose pubkeys ride the manifest as a plain env value — pubkeys
// are public, the identity Secret carries only the nsec + agent-owner.
func TestAgentPodRespondGate(t *testing.T) {
	dept := AgentPodManifest("network", "wss://relay.test", "/p/x.md", "http://gw:31400/v1", "m", "k", "http://at:8080", "atpk", "allowlist", "op,network,data,compute,ai,cpa")
	if !strings.Contains(dept, `name: BUZZ_ACP_RESPOND_TO, value: "allowlist"`) {
		t.Errorf("department manifest must run the allowlist gate")
	}
	if !strings.Contains(dept, `name: BUZZ_ACP_RESPOND_TO_ALLOWLIST, value: "op,network,data,compute,ai,cpa"`) {
		t.Errorf("department manifest must carry its respond-to allowlist")
	}
	custom := AgentPodManifest("helper", "wss://relay.test", "/p/x.md", "http://gw:31400/v1", "m", "k", "http://at:8080", "atpk", "allowlist", "op,cpa")
	if !strings.Contains(custom, `name: BUZZ_ACP_RESPOND_TO_ALLOWLIST, value: "op,cpa"`) {
		t.Errorf("custom-agent manifest must carry its asker + CPA allowlist")
	}
	cpa := CPAPodManifest("waldo", "wss://relay.test", "/p/x.md")
	if !strings.Contains(cpa, `name: BUZZ_ACP_RESPOND_TO, value: "anyone"`) {
		t.Errorf("CPA manifest must run the anyone gate")
	}
	// An empty allowlist must omit the env entirely (buzz-acp then wakes for
	// the owner only — the legacy owner-only behavior).
	ownerOnly := AgentPodManifest("legacy", "wss://relay.test", "/p/x.md", "http://gw:31400/v1", "m", "k", "", "", "allowlist", "")
	if strings.Contains(ownerOnly, "BUZZ_ACP_RESPOND_TO_ALLOWLIST") {
		t.Errorf("empty allowlist must omit the allowlist env")
	}
}

func TestCPAManifestScriptApplies(t *testing.T) {
	s := CPAManifestScript(105, "wss://relay.test", systemPrompt(), "waldo", "http://192.168.30.8:31400/v1", "waldo-litellm-key", "", "")
	for _, want := range []string{
		"pct exec 105",
		`K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"`,
		"create ns agents 2>/dev/null || true",
		"apply -f /tmp/agent-manifests/waldo.yaml",
		"delete pod waldo -n agents",
		"wait --for=condition=Ready pod/waldo -n agents --timeout=300s",
		"http://192.168.30.8:31400/v1",
		"buzz-dev-mcp",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("deploy script missing %q", want)
		}
	}
}

// TestSanitizePodName: display names become DNS-1123-safe object names.
func TestSanitizePodName(t *testing.T) {
	for in, want := range map[string]string{
		"freehold": "freehold",
		"My CPA!":  "my-cpa",
		"Über":     "ber",   // non-ASCII -> dashes; leading/trailing dashes trimmed (DNS-1123)
		"---":      "agent", // all dashes -> fallback
	} {
		if got := sanitizePodName(in); got != want {
			t.Errorf("sanitizePodName(%q) = %q, want %q", in, got, want)
		}
	}
}
