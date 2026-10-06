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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"freehold/contract/identity"
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

// AgentWorkspaceRoot is the durable-plane dir (on the k3s guest, under the
// locked /srv/data/k8s-volumes carve-out — backup=1, survives a compute
// teardown) holding every agent pod's workspace. Keyed by the SANITIZED POD
// NAME — deterministic across rebuilds, unlike a local-path PVC's uid-keyed
// directory (a re-applied PVC reattaches, but a REBUILT k3s mints a fresh PVC
// uid and orphans the old dir). The deploy script creates + chowns the dir
// before the pod applies, so the uid-1000 agent user can write it.
const AgentWorkspaceRoot = "/srv/data/k8s-volumes/agent-home"

// AgentHomePath is where an agent pod's workspace mounts in the container: the
// sprig image's agent user home AND the harness's working directory (buzz-acp
// runs there), so files an agent creates survive pod re-applies and rebuilds.
const AgentHomePath = "/home/agent"

// AgentWorkspaceDir is the durable hostPath dir for one agent pod.
func AgentWorkspaceDir(podName string) string {
	return AgentWorkspaceRoot + "/" + sanitizePodName(podName)
}

// LiteLLMServiceURL is the in-kube OpenAI-compatible endpoint the agent
// harness reaches the litellm gateway at (ClusterIP Service litellm.litellm
// port 4000 — litellm's OpenAI-compat API lives under /v1). The CPA's
// reasoning model (D1 wiring) routes here.
const LiteLLMServiceURL = "http://litellm.litellm:4000/v1"

// BaseLiteLLMModel is the model registered on the gateway at deploy time
// (litellm.tf model_registration) — the underlying entry the default aliases
// below are cloned from by cpbuild.stageLitellmAliases. Keep it equal to
// litellm.tf's registered model_name; bumping one means bumping both.
const BaseLiteLLMModel = "glm-5p3-flash"

// The default litellm alias set (the names agents actually request; each is
// registered on the gateway pointing at BaseLiteLLMModel's underlying model
// for now). CoreLiteLLMModel is pinned to the core identities (the CPA + the
// departments); the other three are what a created custom agent may run,
// General being the default.
const (
	CodeLiteLLMModel          = "Code"          // coding agents
	DefaultAgentLiteLLMModel  = "General"       // default for custom agents
	CoreLiteLLMModel          = "Freehold"      // the CPA + departments
	ExtraThinkingLiteLLMModel = "ExtraThinking" // complex architecture / deep thinking
)

// LiteLLMAliases is the full alias set the build stage ensures on the gateway.
var LiteLLMAliases = []string{CodeLiteLLMModel, DefaultAgentLiteLLMModel, CoreLiteLLMModel, ExtraThinkingLiteLLMModel}

// CustomLiteLLMModels are the aliases a create_agent may pick for a custom
// agent — CoreLiteLLMModel is reserved for the core identities.
var CustomLiteLLMModels = []string{CodeLiteLLMModel, DefaultAgentLiteLLMModel, ExtraThinkingLiteLLMModel}

// AgentLiteLLMKeySecretKey is the k8s Secret literal that carries the pod's
// minted litellm key (referenced by secretKeyRef, never in the manifest).
const AgentLiteLLMKeySecretKey = "key"

// RunnerCoords describes one capability runner an agent pod may exec through
// — the runner's dial URL + nostr pubkey (the MCP audience), plus the single
// target + credential NAME the runner is scoped to (the target-protocol-
// identity runner name). A pod may hold SEVERAL (one per capability its role
// grants it — the grant unit is the runner); empty list = no runner access
// (the pod's bridge advertises conversation + create only). The CPA and
// custom agents get none.
type RunnerCoords struct {
	URL    string
	Pubkey string
	Target string
	Secret string
}

// firstRunner is gone: a pod may hold several capability runners and the
// bridge routes by target (agent.go: agentBridgeBootstrap).

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
func agentBridgeBootstrap(agentToolsURL, agentToolsPubkey string, runner ...RunnerCoords) string {
	if agentToolsURL == "" {
		return "exec buzz-acp"
	}
	// Single-line key=value config: no embedded newlines or quotes, so the
	// bootstrap command embeds cleanly in the Pod manifest's JSON string. The
	// capability-runner coords ride HERE (not only the pod env): buzz-acp spawns
	// the bridge as its MCP server and reads this file, so env alone is not a
	// reliable channel. Aligned comma lists — one entry per capability runner.
	conf := "url=" + agentToolsURL + " pubkey=" + agentToolsPubkey
	if len(runner) > 0 {
		urls, pubs, targets, secrets := runnerLists(runner)
		conf += " runner_urls=" + urls + " runner_pubkeys=" + pubs +
			" runner_targets=" + targets + " runner_secrets=" + secrets
	}
	return "if curl -fsSL --max-time 25 '" + agentToolsURL + "/freehold-agent-tools-binary' -o /tmp/freehold-agent-tools && chmod +x /tmp/freehold-agent-tools 2>/dev/null && printf '" + conf + "' > /tmp/freehold-agent-tools.conf; then export BUZZ_ACP_MCP_COMMAND=/tmp/freehold-agent-tools; fi; exec buzz-acp"
}

// runnerLists renders aligned comma lists of a pod's capability-runner coords.
func runnerLists(runner []RunnerCoords) (urls, pubs, targets, secrets string) {
	u := make([]string, len(runner))
	p := make([]string, len(runner))
	t := make([]string, len(runner))
	s := make([]string, len(runner))
	for i, r := range runner {
		u[i], p[i], t[i], s[i] = r.URL, r.Pubkey, r.Target, r.Secret
	}
	return strings.Join(u, ","), strings.Join(p, ","), strings.Join(t, ","), strings.Join(s, ",")
}

// authTagEnvLine renders the BUZZ_AUTH_TAG pod env line for an agent's memory
// plane. The value is a NIP-OA attestation — a public claim plus the owner's
// signature, carrying NO private key — so it rides the manifest as a plain
// literal, exactly as the respond-to allowlist does; the nsec/owner stay in the
// identity Secret. Empty renders no line.
func authTagEnvLine(authTag string) string {
	if authTag == "" {
		return ""
	}
	return fmt.Sprintf("    - {name: BUZZ_AUTH_TAG, value: %q}\n", authTag)
}

// tzPodBits renders the TZ extras for an agent pod. The env line serves the
// container itself (kubectl exec honors it), but buzz's harness env-clears
// before spawning the MCP servers, so nothing env-based reaches a tool shell;
// the init container instead materializes the zone as /etc/localtime —
// filesystem state an env_clear cannot strip — into an emptyDir file the main
// container mounts over /etc/localtime, so every process in the pod (harness,
// MCP servers, tool shells) reads the operator's local time from libc. The
// image runs non-root (user `agent`), so the one-shot copy runs as root in the
// throwaway init container. A node without the zone's file degrades to UTC
// (touch fallback), matching the zoneinfo mount's DirectoryOrCreate fallback.
// Empty tz (no setting) renders nothing — the pod runs UTC, the pre-settings
// shape.
func tzPodBits(tz, image string) (envLine, initBlock, mountLine, volume string) {
	if tz == "" {
		return "", "", "", ""
	}
	envLine = fmt.Sprintf("    - {name: TZ, value: %q}\n", tz)
	initBlock = fmt.Sprintf(`  initContainers:
  - name: tz
    image: %s
    securityContext: {runAsUser: 0}
    command: ["/bin/sh", "-c", "cp /usr/share/zoneinfo/%s /tz/localtime 2>/dev/null || touch /tz/localtime"]
    env:
    - {name: TZ, value: %q}
    volumeMounts:
    - {name: zoneinfo, mountPath: /usr/share/zoneinfo, readOnly: true}
    - {name: tz, mountPath: /tz}
`, image, tz, tz)
	mountLine = "    - {name: zoneinfo, mountPath: /usr/share/zoneinfo, readOnly: true}\n" +
		"    - {name: tz, mountPath: /etc/localtime, subPath: localtime, readOnly: true}\n"
	volume = "  - name: zoneinfo\n    hostPath: {path: /usr/share/zoneinfo, type: DirectoryOrCreate}\n" +
		"  - name: tz\n    emptyDir: {}\n"
	return envLine, initBlock, mountLine, volume
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
// respondTo + respondAllowlist wire buzz-acp's inbound author gate — which
// authors' mentions wake the agent. The CPA passes "anyone" (relay membership
// is the bound); every other agent passes "allowlist" with an explicit
// comma-separated pubkey list (a core department: the operator + the core
// agents; a custom agent: its asker + the CPA). The allowlist is a plain env
// value — pubkeys are public (the relay publishes them) — while the nsec and
// agent-owner keep riding the identity Secret.
//
// The nsec also NEVER rides the manifest: it comes from the `<pod>-identity`
// Secret (a `secretKeyRef`), which the deploy step writes ONLY when absent —
// the same first-run-wins discipline as litellm's keys. The object names are
// derived from the agent's sanitized name, so each agent owns its own Pod,
// Service, and Secrets.
//
// authTag is the rendered NIP-OA attestation for the agent's memory plane
// (BUZZ_AUTH_TAG) — see authTagEnvLine. Empty omits the env: the agent then has
// the harness but no writable long-term memory, which is the pre-fix shape.
func AgentPodManifest(agentName, relayURL, systemPrompt, litellmBaseURL, litellmModel, litellmKeySecret, agentToolsURL, agentToolsPubkey, respondTo, respondAllowlist, authTag, operatorTZ string, runner ...RunnerCoords) string {
	pod := sanitizePodName(agentName)
	secret := pod + "-identity"
	promptCm := pod + "-prompt"
	podCmd := agentBridgeBootstrap(agentToolsURL, agentToolsPubkey, runner...)
	respondAllowlistEnv := ""
	if respondAllowlist != "" {
		respondAllowlistEnv = fmt.Sprintf("    - {name: BUZZ_ACP_RESPOND_TO_ALLOWLIST, value: %q}\n", respondAllowlist)
	}
	authTagEnv := authTagEnvLine(authTag)
	tzEnv, tzInit, tzMount, tzVolume := tzPodBits(operatorTZ, SprigImage)
	runnerEnv := ""
	if len(runner) > 0 {
		urls, pubs, targets, secrets := runnerLists(runner)
		runnerEnv = fmt.Sprintf(`    - {name: FREEHOLD_RUNNER_URLS, value: %q}
    - {name: FREEHOLD_RUNNER_PUBKEYS, value: %q}
    - {name: FREEHOLD_RUNNER_TARGETS, value: %q}
    - {name: FREEHOLD_RUNNER_SECRETS, value: %q}
`, urls, pubs, targets, secrets)
	}
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
%s  containers:
  - name: %s
    image: %s
    command: ["/bin/bash", "-c", "%s"]
    env:
    - {name: BUZZ_RELAY_URL, value: %q}
    - {name: BUZZ_ACP_SYSTEM_PROMPT_FILE, value: %q}
    - {name: BUZZ_ACP_AGENT_COMMAND, value: "buzz-agent"}
    - {name: BUZZ_ACP_RESPOND_TO, value: %q}
    - {name: BUZZ_AGENT_PROVIDER, value: "openai-compat"}
    - {name: BUZZ_AGENT_REQUIRE_REPLY, value: "1"}
    - {name: RUST_LOG, value: "debug"}
    - {name: BUZZ_ACP_MCP_COMMAND, value: "/usr/local/bin/buzz-dev-mcp"}
    - {name: FREEHOLD_AGENT_TOOLS_URL, value: %q}
    - {name: FREEHOLD_AGENT_TOOLS_PUBKEY, value: %q}
%s%s%s%s    - {name: PATH, value: "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"}
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
    volumeMounts:
    - {name: workspace, mountPath: %s}
    - {name: prompt, mountPath: %s, readOnly: true, subPath: %s}
%s  volumes:
  # The workspace: a name-keyed dir on the durable plane (the deploy script
  # mkdirs + chowns it to the agent user before this manifest applies), so an
  # agent's files survive pod re-applies AND a rebuilt k3s guest. hostPath is
  # deliberate — a PVC's local-path dir is keyed by the PVC uid, so a rebuilt
  # cluster re-mints the claim and silently orphans the data.
  - name: workspace
    hostPath: {path: %s, type: DirectoryOrCreate}
  - name: prompt
    configMap: {name: %s}
%s---
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
		pod, pod, agentName, tzInit, pod, SprigImage, podCmd, relayURL, SystemPromptPath,
		respondTo, agentToolsURL, agentToolsPubkey, respondAllowlistEnv, authTagEnv, runnerEnv, tzEnv,
		litellmBaseURL, litellmModel,
		litellmKeySecret, AgentLiteLLMKeySecretKey,
		secret, secret, AgentHomePath, SystemPromptPath, SystemPromptFile, tzMount,
		AgentWorkspaceDir(pod), promptCm, tzVolume, pod, pod)
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

// AgentManifestScript applies an agent's Pod inside the k3s LXC, mirroring
// the litellm workload pattern. agentName is the display name (sanitized into
// the pod name). The nsec is provided separately via the identity-secret step
// (never embedded here). agentToolsURL/pubkey wires the CP toolset bridge when
// non-empty. respondTo/respondAllowlist wire the inbound author gate (see
// AgentPodManifest).
//
// authTag is the rendered NIP-OA attestation for this pod's memory plane
// (BUZZ_AUTH_TAG; see AgentPodManifest). It travels with the manifest, so the
// delete-then-apply below carries it on every re-apply — a re-apply must never
// be the thing that silently drops an agent's memory.
//
// operatorTZ sets the pod's TZ, the node's zoneinfo mount, and the
// /etc/localtime init-container mount — see tzPodBits. Empty = the pod runs
// UTC.
func AgentManifestScript(k3sVmid uint32, relayURL, systemPrompt, litellmBaseURL, litellmModel, agentName, litellmKeySecret, agentToolsURL, agentToolsPubkey, respondTo, respondAllowlist, authTag, operatorTZ string, runner ...RunnerCoords) string {
	pod := sanitizePodName(agentName)
	wsDir := AgentWorkspaceDir(pod)
	return fmt.Sprintf(`set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec %d -- sh -c"
$EX "$K create ns agents 2>/dev/null || true"
# The durable workspace dir must EXIST IN THE GUEST (and be writable by the
# image's agent user, uid 1000) before the pod mounts it — a rebuilt k3s guest
# comes back with the durable dataset intact but this dir absent. Strict (no
# || true): the durable mount missing means the plane stage did not run.
$EX "mkdir -p %s && chown 1000:1000 %s"
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
		k3sVmid, wsDir, wsDir, pod, AgentPodManifest(agentName, relayURL, systemPrompt, litellmBaseURL, litellmModel, litellmKeySecret, agentToolsURL, agentToolsPubkey, respondTo, respondAllowlist, authTag, operatorTZ, runner...),
		k3sVmid, pod, pod, pod, pod, pod)
}

// CPAManifestScript applies the CPA pod (AgentManifestScript with the CPA
// display name and the reachable litellm gateway base URL). The CPA runs
// hostNetwork (it must reach the relay over the LAN), so it resolves via the
// NODE's resolver and cannot see the in-kube service name `litellm.litellm` —
// litellmBaseURL must therefore be the recorded NodePort URL (cfg.Litellm.URL,
// e.g. http://192.168.30.8:31400/v1), which the node itself answers. It wires
// the agent-tools stdio bridge (agentToolsURL/pubkey) so the CPA can drive the
// CP toolset. The CPA's inbound author gate is "anyone" — relay membership is
// the bound; the CPA is the system's main user touchpoint and every agent's
// delegate.
func CPAManifestScript(k3sVmid uint32, relayURL, systemPrompt, cpaName, litellmBaseURL, litellmKeySecret, agentToolsURL, agentToolsPubkey, authTag, operatorTZ string) string {
	keySec := litellmKeySecret
	if keySec == "" {
		keySec = sanitizePodName(cpaName) + "-litellm-key"
	}
	return AgentManifestScript(k3sVmid, relayURL, systemPrompt, litellmBaseURL, CoreLiteLLMModel, cpaName, keySec, agentToolsURL, agentToolsPubkey, "anyone", "", authTag, operatorTZ)
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

// AgentRetireScript deletes an agent's derived k8s objects (pod, service,
// prompt ConfigMap, identity + litellm-key secrets) inside the k3s LXC — the
// object half of taking an agent away (update_agent's rename and manage_agent's
// remove). The durable workspace dir is deliberately NOT touched: it is data,
// not an object. --ignore-not-found keeps every line idempotent, and a missing
// litellm-key secret is expected (custom agents share the CPA's).
func AgentRetireScript(k3sVmid uint32, agentName string) string {
	pod := sanitizePodName(agentName)
	return fmt.Sprintf(`set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec %d -- sh -c"
$EX "$K delete pod %s -n agents --ignore-not-found=true --wait=false"
$EX "$K delete service %s -n agents --ignore-not-found=true"
$EX "$K delete configmap %s-prompt -n agents --ignore-not-found=true"
$EX "$K delete secret %s-identity -n agents --ignore-not-found=true"
$EX "$K delete secret %s-litellm-key -n agents --ignore-not-found=true"
echo AGENT_RETIRE_OK`, k3sVmid, pod, pod, pod, pod, pod)
}

// shQ single-quotes a value for a shell-embedded literal (no embedded quotes
// in the values we pass — 64-hex nsec and owner pubkey).
func shQ(s string) string {
	return "'" + s + "'"
}

// mintIdentityIn mints a fresh runner-style identity (nostr + enc secrets) in
// dir, persisting it on first use. Reuse is decided by the caller re-loading; a
// raced re-mint would orphan a grant, so callers re-check via LoadIdentity.
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
	id := identity.Identity{
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
	id, err := identity.Load(dir)
	if err != nil {
		return "", err
	}
	return id.NostrPubkeyHex()
}

// ensureIdentity mints a runner-style identity in dir if absent, returns nil.
func ensureIdentity(dir string) error {
	if _, err := identity.Load(dir); err == nil {
		return nil
	}
	// Mint on demand (same shape as min identity in the rebuild engine's
	// stageCpa). The identity must survive compute-only teardown, so the
	// caller passes a durable dir (under the CP's durable-plane area).
	return mintIdentityIn(dir)
}
