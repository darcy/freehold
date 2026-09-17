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

// AgentPodName returns the sanitized k8s object name (pod/service/secret base)
// for an agent's display name — the prefix every derived object name hangs
// off (`<pod>-identity`, `<pod>-prompt`, `<pod>-litellm-key`). Exported so the
// the operator CLI can derive the same names the manifest uses.
func AgentPodName(agentName string) string {
	return sanitizePodName(agentName)
}

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

// KeySecretFor returns the name of the litellm-key k8s Secret an agent's pod
// reads its OPENAI_COMPAT_API_KEY from: the sanitized pod name + "-litellm-key".
// stageLitellm seeds the CPA's with this name; a created agent either gets its
// own seeded secret or reuses the CPA's gateway key secret.
func KeySecretFor(agentName string) string {
	return sanitizePodName(agentName) + "-litellm-key"
}

// PodName is the sanitized k8s object name (pod/service/secret/configmap
// prefix) an agent display name maps to. Exported so callers can guard against
// names that would collide with the CPA or another pce agent.
func PodName(agentName string) string { return sanitizePodName(agentName) }

// SystemPromptFile is the ConfigMap data key (and the mounted subPath) that
// carries every agent pod's system prompt. The name is role-neutral because the
// same slot carries the CPA's prompt or a department's, selected at create time.
const SystemPromptFile = "SYSTEM_PROMPT.md"

// SystemPromptPath is where an agent pod reads its purpose from: the
// <pod>-prompt ConfigMap mounts the agent's embedded system prompt
// (agents/freehold/prompt.md for the CPA, agents/<department>/prompt.md for a
// department — both embedded via the freehold/agents package) read-only into
// the pod, and the agent re-reads it on every spawn — never cached.
const SystemPromptPath = "/srv/freehold/SYSTEM_PROMPT.md"

// LiteLLMServiceURL is the in-kube OpenAI-compatible endpoint the agent
// harness reaches the litellm gateway at (ClusterIP Service litellm.litellm
// port 4000 — litellm's OpenAI-compat API lives under /v1). The CPA's
// reasoning model (D1 wiring) routes here.
const LiteLLMServiceURL = "http://litellm.litellm:4000/v1"

// CpaLiteLLMModel is the litellm model name the CPA talks to. It must equal the
// model registered at deploy time (litellm.tf model_registration) — no alias:
// the CPA either routes or it 400s.
const CpaLiteLLMModel = "deepseek-v4-flash"

// AgentLiteLLMKeySecretKey is the k8s Secret literal that carries the pod's
// minted litellm key (referenced by secretKeyRef, never in the manifest).
const AgentLiteLLMKeySecretKey = "key"

// AgentPodManifest is the agent Pod + Service manifest for a named agent. The
// agent is ONE pod (at-most-one-live-instance, I4); the harness is the
// container's PID-1 process (entrypoint `exec`), presence is kind:20001, and
// the pod is reaped by the k3s namespace's own lifecycle. `restartPolicy:
// Never` honors I5 — an intentional clean exit stays terminal; the kubelet
// must not resurrect a pod that stopped on purpose.
//
// systemPrompt is the FULL text of the agent's embedded prompt (the CPA's
// agents/freehold/prompt.md, or a department's agents/<department>/prompt.md;
// the caller passes the file contents, not a path): it embeds as the
// <pod>-prompt ConfigMap's content (indented four spaces per line so the `|`
// block scalar is valid YAML) and the pod mounts that ConfigMap read-only at
// SystemPromptPath; the pod re-reads the mounted file on every spawn — never
// cached. Editing the prompt and redeploying is the only way the agent's
// behavior changes.
//
// The reasoning model rides litellm as an OpenAI-compatible endpoint: the pod
// points the buzz-agent harness at litellmBaseURL with litellmModel and an API
// key (OPENAI_COMPAT_API_KEY from the `<pod>-litellm-key` Secret by
// secretKeyRef). TODAY that key is the litellm gateway's admin master key
// (litellm's /key/generate still needs a bootstrap virtual key before scoped
// keys can be minted — see AGENTS.md "Known gaps"); the key NEVER rides the
// manifest.
//
// The nsec also NEVER rides the manifest: it comes from the `<pod>-identity`
// Secret (a `secretKeyRef`), which the deploy step writes ONLY when absent —
// the same first-run-wins discipline as litellm's keys. The object names are
// derived from the agent's sanitized name, so each agent owns its own Pod,
// Service, and Secrets.
// agentBridgeBootstrap returns the pod container command that fetches this
// binary's `mcp` stdio bridge from the CP's freehold-agent-tools server and
// points BUZZ_ACP_MCP_COMMAND at it (each boot); on fetch failure it falls back
// to the plain buzz-dev-mcp MCP command so the agent never loses message tools.
// The image runs non-root, so the bridge + config land in /tmp (world-writable),
// not /usr/local. Empty agentToolsURL => no bridge (plain buzz-dev-mcp).
func agentBridgeBootstrap(agentToolsURL, agentToolsPubkey string) string {
	if agentToolsURL == "" {
		return "exec buzz-acp"
	}
	// Single-line key=value config: no embedded newlines or quotes, so the
	// bootstrap command embeds cleanly in the Pod manifest's JSON string.
	conf := "url=" + agentToolsURL + " pubkey=" + agentToolsPubkey
	return "if curl -fsSL --max-time 25 '" + agentToolsURL + "/freehold-agent-tools-binary' -o /tmp/freehold-agent-tools && chmod +x /tmp/freehold-agent-tools 2>/dev/null && printf '" + conf + "' > /tmp/freehold-agent-tools.conf; then export BUZZ_ACP_MCP_COMMAND=/tmp/freehold-agent-tools; fi; exec buzz-acp"
}

// AgentPodManifest is the agent Pod + Service manifest for a named agent. The
// agent is ONE pod (at-most-one-live-instance, I4); the harness is the
// container's PID-1 process (entrypoint `exec`), presence is kind:20001, and
// the pod is reaped by the k3s namespace's own lifecycle. `restartPolicy:
// Never` honors I5 — an intentional clean exit stays terminal; the kubelet
// must not resurrect a pod that stopped on purpose.
//
// systemPrompt is the FULL text of the agent's embedded prompt (the CPA's
// agents/freehold/prompt.md, or a department's agents/<department>/prompt.md;
// the caller passes the file contents, not a path): it embeds as the
// <pod>-prompt ConfigMap's content (indented four spaces per line so the `|`
// block scalar is valid YAML) and the pod mounts that ConfigMap read-only at
// SystemPromptPath; the pod re-reads the mounted file on every spawn — never
// cached. Editing the prompt and redeploying is the only way the agent's
// behavior changes.
//
// The reasoning model rides litellm as an OpenAI-compatible endpoint: the pod
// points the buzz-agent harness at litellmBaseURL with litellmModel and an API
// key (OPENAI_COMPAT_API_KEY from the `<pod>-litellm-key` Secret by
// secretKeyRef). TODAY that key is the litellm gateway's admin master key
// (litellm's /key/generate still needs a bootstrap virtual key before scoped
// keys can be minted — see AGENTS.md "Known gaps"); the key NEVER rides the
// manifest.
//
// When agentToolsURL is set, the pod's MCP command is the freehold-agent-tools
// stdio bridge (agentBridgeBootstrap): it aggregates buzz-dev-mcp's message
// tools with create/grant/manage-agent (signed as this agent's nsec), so the
// agent can drive the CP toolset from conversation.
//
// The nsec also NEVER rides the manifest: it comes from the `<pod>-identity`
// Secret (a `secretKeyRef`), which the deploy step writes ONLY when absent —
// the same first-run-wins discipline as litellm's keys. The object names are
// derived from the agent's sanitized name, so each agent owns its own Pod,
// Service, and Secrets.
func AgentPodManifest(agentName, relayURL, systemPrompt, litellmBaseURL, litellmModel, litellmKeySecret, agentToolsURL, agentToolsPubkey string) string {
	pod := sanitizePodName(agentName)
	secret := pod + "-identity"
	promptCm := pod + "-prompt"
	podCmd := agentBridgeBootstrap(agentToolsURL, agentToolsPubkey)
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: agents
data:
  %s: |
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
  # The agent must reach the community relay over the LAN (its own DNS +
  # the relay LXC), but pod-CNI egress to non-cluster LAN IPs is often
  # blocked (kube-router FORWARD policy DROP without SNAT). hostNetwork puts
  # the pod on the node's network so it resolves via the node (CP resolver)
  # and reaches the relay directly, like any guest. The agent is the
  # appliance's own trained identity on the operator's relay — it does not
  # need pod-CNI isolation from its own control plane.
  hostNetwork: true
  containers:
  - name: %s
    image: %s
    command: ["/bin/bash", "-c", "%s"]
    env:
    - {name: BUZZ_RELAY_URL, value: %q}
    - {name: BUZZ_ACP_SYSTEM_PROMPT_FILE, value: %q}
    - {name: BUZZ_ACP_AGENT_COMMAND, value: "buzz-agent"}
    - {name: BUZZ_ACP_RESPOND_TO, value: "allowlist"}
    - {name: BUZZ_AGENT_PROVIDER, value: "openai-compat"}
    - {name: RUST_LOG, value: "debug"}
    - {name: BUZZ_ACP_MCP_COMMAND, value: "/usr/local/bin/buzz-dev-mcp"}
    - {name: FREEHOLD_AGENT_TOOLS_URL, value: %q}
    - {name: FREEHOLD_AGENT_TOOLS_PUBKEY, value: %q}
    - {name: PATH, value: "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"}
    - {name: OPENAI_COMPAT_BASE_URL, value: %q}
    - {name: OPENAI_COMPAT_MODEL, value: %q}
    - name: OPENAI_COMPAT_API_KEY
      valueFrom:
        secretKeyRef: {name: %s, key: %s}
    - name: BUZZ_PRIVATE_KEY
      valueFrom:
        secretKeyRef: {name: %s, key: nsec}
    - name: BUZZ_ACP_AGENT_OWNER
      valueFrom:
        secretKeyRef: {name: %s, key: owner}
    - name: BUZZ_ACP_RESPOND_TO_ALLOWLIST
      valueFrom:
        secretKeyRef: {name: %s, key: owner}
    volumeMounts:
    - {name: prompt, mountPath: %s, readOnly: true, subPath: %s}
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
		promptCm, SystemPromptFile, indentSystemPrompt(systemPrompt),
		pod, pod, agentName, pod, SprigImage, podCmd, relayURL, SystemPromptPath,
		agentToolsURL, agentToolsPubkey,
		litellmBaseURL, litellmModel,
		litellmKeySecret, AgentLiteLLMKeySecretKey,
		secret, secret, secret, SystemPromptPath, SystemPromptFile, promptCm, pod, pod)
}

// indentSystemPrompt indents every prompt line by four spaces so it embeds as
// a valid k8s ConfigMap block scalar (the `data:` key `  SYSTEM_PROMPT.md:
// |` is at 2 spaces, so the content must sit at 4 to parse as one scalar).
func indentSystemPrompt(prompt string) string {
	lines := strings.Split(strings.TrimRight(prompt, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

// CPAPodManifest is the CPA's pod manifest — AgentPodManifest with the CPA's
// display name (A1's stored value, default freehold) wired to the litellm
// gateway (LiteLLMServiceURL + CpaLiteLLMModel).
func CPAPodManifest(cpaName, relayURL, systemPrompt string) string {
	return AgentPodManifest(cpaName, relayURL, systemPrompt, LiteLLMServiceURL, CpaLiteLLMModel, sanitizePodName(cpaName)+"-litellm-key", "", "")
}

// AgentManifestScript applies an agent's Pod inside the k3s LXC, mirroring
// the litellm workload pattern. agentName is the display name (sanitized into
// the pod name). The nsec is provided separately via the identity-secret step
// (never embedded here). agentToolsURL/pubkey wires the CP toolset bridge when
// non-empty.
func AgentManifestScript(k3sVmid uint32, relayURL, systemPrompt, litellmBaseURL, litellmModel, agentName, litellmKeySecret, agentToolsURL, agentToolsPubkey string) string {
	pod := sanitizePodName(agentName)
	return fmt.Sprintf(`set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec %d -- sh -c"
$EX "$K create ns agents 2>/dev/null || true"
# The pct push destination needs /tmp/agent-manifests to EXIST IN THE GUEST;
# a bare host-side mkdir is not enough (the guest mount is separate).
$EX "mkdir -p /tmp/agent-manifests"
mkdir -p /tmp/agent-manifests
cat >/tmp/agent-manifests/%s.yaml <<'YAML'
%s
YAML
pct push %d /tmp/agent-manifests/%s.yaml /tmp/agent-manifests/%s.yaml
# A Pod's spec is immutable: re-applying a changed Pod (e.g. a new litellm URL
# or prompt ConfigMap ref) errors. Delete it first so apply recreates it with
# the current manifest. ConfigMap/Service are mutable and apply cleanly.
$EX "$K delete pod %s -n agents --ignore-not-found=true >/dev/null 2>&1 || true"
$EX "$K apply -f /tmp/agent-manifests/%s.yaml"
$EX "$K wait --for=condition=Ready pod/%s -n agents --timeout=300s"
echo AGENT_LEG1_OK`,
		k3sVmid, pod, AgentPodManifest(agentName, relayURL, systemPrompt, litellmBaseURL, litellmModel, litellmKeySecret, agentToolsURL, agentToolsPubkey),
		k3sVmid, pod, pod, pod, pod, pod)
}

// CPAManifestScript applies the CPA pod (AgentManifestScript with the CPA
// display name and the reachable litellm gateway base URL). The CPA runs
// hostNetwork (it must reach the relay over the LAN), so it resolves via the
// NODE's resolver and cannot see the in-kube service name `litellm.litellm` —
// litellmBaseURL must therefore be the recorded NodePort URL (cfg.Litellm.URL,
// e.g. http://192.168.30.8:31400/v1), which the node itself answers. It wires
// the agent-tools stdio bridge (agentToolsURL/pubkey) so the CPA can drive the
// CP toolset.
func CPAManifestScript(k3sVmid uint32, relayURL, systemPrompt, cpaName, litellmBaseURL, litellmKeySecret, agentToolsURL, agentToolsPubkey string) string {
	keySec := litellmKeySecret
	if keySec == "" {
		keySec = sanitizePodName(cpaName) + "-litellm-key"
	}
	return AgentManifestScript(k3sVmid, relayURL, systemPrompt, litellmBaseURL, CpaLiteLLMModel, cpaName, keySec, agentToolsURL, agentToolsPubkey)
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

// AgentLiteLLMKeyScript seeds the agents-namespace Secret the pod's
// OPENAI_COMPAT_API_KEY references (first-run-wins, like the identity secret).
// The value comes from the runner-injected $LITELLM env (requested by name in
// the exec) — never a shell literal, so no credential crosses the audited
// command.
func AgentLiteLLMKeyScript(k3sVmid uint32, agentName string) string {
	pod := sanitizePodName(agentName)
	secret := pod + "-litellm-key"
	return fmt.Sprintf(`set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec %d -- sh -c"
$EX "$K create ns agents 2>/dev/null || true"
$EX "$K get secret %s -n agents >/dev/null 2>&1 || $K create secret generic %s -n agents --from-literal=key=\"$LITELLM\""
echo AGENT_LITELLM_KEY_OK`,
		k3sVmid, secret, secret)
}

// shQ single-quotes a value for a shell-embedded literal (no embedded quotes
// in the values we pass — 64-hex nsec and owner pubkey).
func shQ(s string) string {
	return "'" + s + "'"
}
