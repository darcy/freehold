package agent

import (
	"strings"
	"testing"
)

func TestCPAPodManifestBasics(t *testing.T) {
	m := CPAPodManifest("waldo", "wss://relay.test", "/prompts/CPA_SYSTEM_PROMPT.md")
	for _, want := range []string{
		"kind: Pod",
		"name: cpa",
		"namespace: agents",
		"image: ghcr.io/block/buzz-sprig:main",
		"exec buzz-acp",
		"BUZZ_RELAY_URL",
		`value: "wss://relay.test"`,
		"BUZZ_ACP_SYSTEM_PROMPT_FILE",
		"/prompts/CPA_SYSTEM_PROMPT.md",
		"BUZZ_ACP_AGENT_COMMAND",
		`value: "buzz-agent"`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q", want)
		}
	}
	// The nsec must NEVER be embedded in the manifest (it rides the Secret).
	if strings.Contains(m, "BUZZ_PRIVATE_KEY") {
		if strings.Contains(m, "value:") && !strings.Contains(m, "secretKeyRef") {
			t.Errorf("manifest embeds a private-key literal")
		}
	}
	// The display name must appear only as the cpa-name annotation.
	if !strings.Contains(m, "freehold.fh/cpa-name: waldo") {
		t.Errorf("manifest missing the cpa-name annotation %q", "waldo")
	}
	// No restart supervisor: the pod must be a plain v1 Pod (I5 — no
	// Deployment controller that would resurrect an intentional exit).
	if strings.Contains(m, "kind: Deployment") {
		t.Errorf("CPA must be a bare Pod, not a Deployment (intentional exit must stay terminal)")
	}
}

func TestCPAManifestScriptApplies(t *testing.T) {
	s := CPAManifestScript(105, "wss://relay.test", "/p/CPA_SYSTEM_PROMPT.md", "waldo")
	for _, want := range []string{
		"pct exec 105",
		`K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"`,
		"create ns agents 2>/dev/null || true",
		"apply -f /tmp/cpa-manifests/cpa.yaml",
		"wait --for=condition=Ready pod/cpa -n agents --timeout=300s",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("deploy script missing %q", want)
		}
	}
}
