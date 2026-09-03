// Package agent: Chunk 4 CPA (Control Plane Agent) deployment.
//
// The CPA is a real reasoning agent running on the SAME harness class expert
// agents use — Buzz's remote-agent mechanism (VISION_REMOTE_AGENTS.md): a
// kube Pod running the `ghcr.io/block/buzz-sprig` image, whose entrypoint
// `exec`s `buzz-acp`, wired to the community's relay with the agent's own
// nsec. Freehold already owns a k3s LXC (Chunk 3), so the CPA deploys as a
// Pod into it — no separate substrate.
//
// This package builds the agent pod manifest and the kube-apply script,
// mirroring the litellm workload pattern (stageLitellm): manifests written
// host-side, pushed into the k3s LXC, applied via the in-guest kubectl. The
// k8s object names (Pod/Service/Secret) are DERIVED from the agent's display
// name, so the CPA and every agent it creates own distinct objects — a
// second agent never applies over the first.
package agent

import (
	"fmt"
	"strings"
)

// SprigImage is the default agent harness image (buzz multipain — buzz-acp,
// buzz-agent, buzz-dev-mcp, rg, tree; Alpine + bash + git + CA certs). It
// tracks the moving main tag today; a build-time digest pin is a named
// follow-up (the release workflow must resolve the multi-arch manifest).
const SprigImage = "ghcr.io/block/buzz-sprig:main"

// DefaultCPAName is the default CPA display name (A1's fallback).
const DefaultCPAName = "freehold"

// sanitizePodName turns an agent display name into a legal k8s object name
// (DNS-1123: lowercase letters/digits with internal dashes, <=63 chars).
// "My CPA!" -> "my-cpa". Every object name (pod/service/secret) is derived
// from this so a SECOND agent never collides with the first.
func sanitizePodName(name string) string {
	if name == "" {
		name = DefaultCPAName
	}
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "agent"
	}
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return s
}

// CPAAbsolutePromptPath is where the CPA pod reads its purpose from: the
// <pod>-prompt ConfigMap mounts CPA_SYSTEM_PROMPT.md (embedded from the repo
// root) read-only into the pod, and the agent re-reads it on every spawn —
// never cached.
const CPAAbsolutePromptPath = "/srv/freehold/CPA_SYSTEM_PROMPT.md"

// CpaMcpCommand/CpaMcpArgs wire the CPA's dedicated toolset into the harness
// (B2: the toolset is registered as an MCP surface the pod can actually
// call, not a dead config knob). No skill-execution tools yet (Chunk 5/6).
const (
	CpaMcpCommand = "/usr/local/bin/freehold-agent-tools"
	CpaMcpArgs    = "serve --addr 127.0.0.1:8787"
)

// AgentPodManifest is the agent Pod + Service manifest for a named agent. The
// agent is ONE pod (at-most-one-live-instance, I4); the harness is the
// container's PID-1 process (entrypoint `exec`), presence is kind:20001, and
// the pod is reaped by the k3s namespace's own lifecycle. `restartPolicy:
// Never` honors I5 — an intentional clean exit stays terminal; the kubelet
// must not resurrect a pod that stopped on purpose.
//
// The nsec NEVER rides the manifest: it comes from the `<pod>-identity` Secret
// (a `secretKeyRef`), which the deploy step writes ONLY when absent — the
// same first-run-wins discipline as litellm's keys. The object names are
// derived from the agent's sanitized name, so each agent owns its own Pod,
// Service, and Secret.
//
// systemPromptPath is where the pod reads its purpose from — the <pod>-prompt
// ConfigMap mounts it at CPAbsolutePromptPath and the agent re-reads it on
// every spawn (never cached; editing CPA_SYSTEM_PROMPT.md and redeploying is
// the only way the CPA's behavior changes). mcpCommand is the harness's
// dedicated toolset server (CpaMcpCommand) so the pod actually exposes the
// create/grant/manage-agent tools to the reasoning agent.
func AgentPodManifest(agentName, relayURL, systemPromptPath string) string {
	pod := sanitizePodName(agentName)
	secret := pod + "-identity"
	promptCm := pod + "-prompt"
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: agents
data:
  CPA_SYSTEM_PROMPT.md: |
    %s
---
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: agents
  labels:
    app: %s
    app.kubernetes.io/managed-by: freehold
  annotations:
    freehold.fh/agent-name: %s
spec:
  restartPolicy: Never
  containers:
  - name: %s
    image: %s
    command: ["/bin/bash", "-c", "exec buzz-acp"]
    env:
    - {name: BUZZ_RELAY_URL, value: %q}
    - {name: BUZZ_ACP_SYSTEM_PROMPT_FILE, value: %q}
    - {name: BUZZ_ACP_MCP_COMMAND, value: %q}
    - {name: BUZZ_ACP_MCP_ARGS, value: %q}
    - {name: BUZZ_ACP_AGENT_COMMAND, value: "buzz-agent"}
    - {name: BUZZ_ACP_RESPOND_TO, value: "allowlist"}
    - name: BUZZ_PRIVATE_KEY
      valueFrom:
        secretKeyRef: {name: %s, key: nsec}
    - name: BUZZ_ACP_AGENT_OWNER
      valueFrom:
        secretKeyRef: {name: %s, key: owner}
    volumeMounts:
    - {name: prompt, mountPath: %s, readOnly: true}
  volumes:
  - name: prompt
    configMap: {name: %s}
---
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: agents
spec:
  selector: {app: %s}
  ports:
  - {port: 443}
`,
		promptCm, indentSystemPrompt(systemPromptPath),
		pod, pod, agentName, pod, SprigImage, relayURL, systemPromptPath,
		CpaMcpCommand, CpaMcpArgs,
		secret, secret, CPAAbsolutePromptPath, promptCm, pod, pod)
}

// indentSystemPrompt indents every prompt line by two spaces so it embeds as
// a valid k8s ConfigMap block scalar (`  CPA_SYSTEM_PROMPT.md: |`).
func indentSystemPrompt(prompt string) string {
	lines := strings.Split(strings.TrimRight(prompt, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// CPAPodManifest is the CPA's pod manifest — AgentPodManifest with the CPA's
// display name (A1's stored value, default freehold).
func CPAPodManifest(cpaName, relayURL, systemPromptPath string) string {
	return AgentPodManifest(cpaName, relayURL, systemPromptPath)
}

// AgentManifestScript applies an agent's Pod inside the k3s LXC, mirroring
// litellmManifestScript. agentName is the display name (sanitized into the
// pod name). The nsec is provided separately via the identity-secret step
// (never embedded here).
func AgentManifestScript(k3sVmid uint32, relayURL, systemPromptPath, agentName string) string {
	pod := sanitizePodName(agentName)
	return fmt.Sprintf(`set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec %d -- sh -c"
$EX "$K create ns agents 2>/dev/null || true"
mkdir -p /tmp/agent-manifests
cat >/tmp/agent-manifests/%s.yaml <<'YAML'
%s
YAML
pct push %d /tmp/agent-manifests/%s.yaml /tmp/agent-manifests/%s.yaml
$EX "$K apply -f /tmp/agent-manifests/%s.yaml"
$EX "$K wait --for=condition=Ready pod/%s -n agents --timeout=300s"
echo AGENT_LEG1_OK`,
		k3sVmid, pod, AgentPodManifest(agentName, relayURL, systemPromptPath),
		k3sVmid, pod, pod, pod, pod)
}

// CPAManifestScript applies the CPA pod (AgentManifestScript with the CPA
// display name).
func CPAManifestScript(k3sVmid uint32, relayURL, systemPromptPath, cpaName string) string {
	return AgentManifestScript(k3sVmid, relayURL, systemPromptPath, cpaName)
}

// AgentIdentityScript creates the agent's identity Secret (nsec + owner) in
// the agents namespace. First-run-wins: a re-run must never re-roll the
// agent's key (identity continuity across rebuilds). The nsec travels as a
// shell-quoted literal in the exec script — the same shape the CP's deploy
// flags use for generated material; it never rides the persisted manifest.
func AgentIdentityScript(k3sVmid uint32, nsecSecretHex, ownerPub, agentName string) string {
	pod := sanitizePodName(agentName)
	secret := pod + "-identity"
	return fmt.Sprintf(`set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec %d -- sh -c"
$EX "$K create ns agents 2>/dev/null || true"
$EX "$K get secret %s -n agents >/dev/null 2>&1 || $K create secret generic %s -n agents --from-literal=nsec=%s --from-literal=owner=%s"
echo AGENT_IDENTITY_OK`,
		k3sVmid, secret, secret, shQ(nsecSecretHex), ownerPub)
}

// CPAIdentityScript creates the CPA's identity Secret (AgentIdentityScript
// with the CPA display name).
func CPAIdentityScript(k3sVmid uint32, nsecSecretHex, ownerPub string) string {
	return AgentIdentityScript(k3sVmid, nsecSecretHex, ownerPub, "")
}

// shQ single-quotes a value for a shell-embedded literal (no embedded quotes
// in the values we pass — 64-hex nsec and owner pubkey).
func shQ(s string) string {
	return "'" + s + "'"
}
