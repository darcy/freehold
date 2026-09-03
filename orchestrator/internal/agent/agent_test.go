package agent

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// systemPrompt is the real multi-line prompt, loaded from the repo root's
// prompts/ dir (what stageCpa passes to AgentManifestScript — the old tests
// passed a path string, which is why the block-scalar bug compiled).
func systemPrompt() string {
	b, err := os.ReadFile("../../../prompts/CPA_SYSTEM_PROMPT.md")
	if err != nil {
		panic(err)
	}
	return string(b)
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
		CPASystemPromptPath,
		"BUZZ_ACP_MCP_COMMAND",
		CpaMcpCommand,
		"BUZZ_ACP_AGENT_COMMAND",
		`value: "buzz-agent"`,
		"restartPolicy: Never",
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
	if got := cm.Data["CPA_SYSTEM_PROMPT.md"]; got != strings.TrimRight(sp, "\n") {
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

func TestCPAManifestScriptApplies(t *testing.T) {
	s := CPAManifestScript(105, "wss://relay.test", "/p/CPA_SYSTEM_PROMPT.md", "waldo")
	for _, want := range []string{
		"pct exec 105",
		`K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"`,
		"create ns agents 2>/dev/null || true",
		"apply -f /tmp/agent-manifests/waldo.yaml",
		"wait --for=condition=Ready pod/waldo -n agents --timeout=300s",
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
