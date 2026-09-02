// Package agent: Chunk 4 CPA (Control Plane Agent) deployment.
//
// The CPA is a real reasoning agent running on the SAME harness class expert
// agents use — Buzz's remote-agent mechanism (VISION_REMOTE_AGENTS.md): a
// kube Pod running the digest-pinned `ghcr.io/block/buzz-sprig` image, whose
// entrypoint `exec`s `buzz-acp`, wired to the community's relay with the
// agent's own nsec. Freehold already owns a k3s LXC (Chunk 3), so the CPA
// deploys as a Pod into it — no separate substrate.
//
// This package builds the CPA pod manifest and the kube-apply script, mirroring
// the litellm workload pattern (stageLitellm): manifests written host-side,
// pushed into the k3s LXC, applied via the in-guest kubectl.
package agent

import (
	"fmt"
)

// SprigImage is the default CPA harness image (buzz multipain — buzz-acp,
// buzz-agent, buzz-dev-mcp, rg, tree; Alpine + bash + git + CA certs).
// Pinning: the provider bakes a digest; here we use the moving main tag and
// record the resolved image ID in a pod annotation (the digest pin needs the
// multi-arch manifest resolved at build time — a named follow-up).
const SprigImage = "ghcr.io/block/buzz-sprig:main"

// DefaultCPAName is the default CPA display name (A1's fallback).
const DefaultCPAName = "freehold"

// CPAPodManifest is the CPA Pod + Service manifest. The CPA is ONE pod (the
// replicas:1 at-most-one-live-instance invariant, I4): the harness is the
// container's PID-1 process (entrypoint `exec`), presence is kind:20001, and
// the pod is reaped by the k3s namespace's own lifecycle — no supervisor
// resurrects an intentional exit (I5).
//
// The nsec NEVER rides the manifest: it comes from the `cpa-identity` Secret
// (a `secretKeyRef`), which the deploy step writes ONLY when absent — the
// same first-run-wins discipline as litellm's keys.
func CPAPodManifest(cpaName, relayURL, systemPromptPath string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: cpa
  namespace: agents
  labels:
    app: cpa
    app.kubernetes.io/managed-by: freehold
  annotations:
    freehold.fh/cpa-name: %s
spec:
  restartPolicy: Always
  containers:
  - name: cpa
    image: %s
    command: ["/bin/bash", "-c", "exec buzz-acp"]
    env:
    - {name: BUZZ_RELAY_URL, value: %q}
    - {name: BUZZ_ACP_SYSTEM_PROMPT_FILE, value: %q}
    - {name: BUZZ_ACP_AGENT_COMMAND, value: "buzz-agent"}
    - {name: BUZZ_ACP_RESPOND_TO, value: "allowlist"}
    - name: BUZZ_PRIVATE_KEY
      valueFrom:
        secretKeyRef: {name: cpa-identity, key: nsec}
    - name: BUZZ_ACP_AGENT_OWNER
      valueFrom:
        secretKeyRef: {name: cpa-identity, key: owner}
---
apiVersion: v1
kind: Service
metadata:
  name: cpa
  namespace: agents
spec:
  selector: {app: cpa}
  ports:
  - {port: 443}
`, cpaName, SprigImage, relayURL, systemPromptPath)
}

// CPAManifestScript applies the CPA pod + secret inside the k3s LXC, mirroring
// litellmManifestScript. k3sVmid is the k3s LXC's vmid; relayURL is the relay
// the CPA joins; ownerPub is the operator's pubkey (the response-to allowlist
// owner); cpaName is the display name. The nsec is provided separately via
// the cpa-identity secret-creation step (never embedded here).
func CPAManifestScript(k3sVmid uint32, relayURL, systemPromptPath, cpaName string) string {
	return fmt.Sprintf(`set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec %d -- sh -c"
$EX "$K create ns agents 2>/dev/null || true"
mkdir -p /tmp/cpa-manifests
cat >/tmp/cpa-manifests/cpa.yaml <<'YAML'
%s
YAML
pct push %d /tmp/cpa-manifests/cpa.yaml /tmp/cpa-manifests/cpa.yaml
$EX "$K apply -f /tmp/cpa-manifests/cpa.yaml"
$EX "$K wait --for=condition=Ready pod/cpa -n agents --timeout=300s"
echo CPA_LEG1_OK`,
		k3sVmid, CPAPodManifest(cpaName, relayURL, systemPromptPath), k3sVmid)
}

// CPAIdentityScript creates the cpa-identity Secret (nsec + owner pubkey)
// inside the agents namespace. First-run-wins: a re-run must never re-roll
// the CPA's key (identity continuity across rebuilds). The nsec travels as a
// shell-quoted literal in the exec script — the same shape the CP's deploy
// flags use for generated material; it never rides the persisted manifest.
func CPAIdentityScript(k3sVmid uint32, nsecSecretHex, ownerPub string) string {
	return fmt.Sprintf(`set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec %d -- sh -c"
$EX "$K create ns agents 2>/dev/null || true"
$EX "$K get secret cpa-identity -n agents >/dev/null 2>&1 || $K create secret generic cpa-identity -n agents --from-literal=nsec=%s --from-literal=owner=%s"
echo CPA_IDENTITY_OK`,
		k3sVmid, shQ(nsecSecretHex), ownerPub)
}

// shQ single-quotes a value for a shell-embedded literal (no embedded quotes
// in the values we pass — 64-hex nsec and owner pubkey).
func shQ(s string) string {
	return "'" + s + "'"
}
