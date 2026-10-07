package cpbuild

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Kube slots — a dynamic kube door's namespace-scoped slice of the cluster.
// The unit of kube access for a custom agent is a SLOT: one namespace holding
// a ServiceAccount bound to a full-access-within-the-namespace Role (never
// cluster scope), an optional ResourceQuota, and the SA-token Secret the door
// seals. The slot is carved by COMPUTE through its audited kube-api-root door
// (in conversation — provision_runner never mutates the cluster); this file
// holds the manifest both sides agree on, the validators, and the CP-side
// verify/read legs (the same box-runner exec the build's doorToken uses).
// The token passes through CP memory only — it is sealed into the runner's
// package, never stored or logged.

// kubeDoorNamePrefix is the required name prefix for a dynamic kube door —
// <target>-<protocol>-<identity> with target=kube, protocol=api (the static
// doors' shape; keeps kube doors tell-apart-able in audit streams).
const kubeDoorNamePrefix = "kube-api-"

// reservedKubeNamespaces are refused as slot namespaces: the control-plane
// namespaces (kube-*), the default, and the platform's own service namespaces
// — caddy, litellm, and agents (the pods' namespace: a slot's ns-admin Role
// would read every pod's identity Secret out of it).
var reservedKubeNamespaces = map[string]bool{
	"kube-system": true, "kube-public": true, "kube-node-lease": true,
	"default": true, "caddy": true, "litellm": true, "agents": true,
}

// kubeNSRe is a DNS-1123 label — the strictest form a namespace name takes.
var kubeNSRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// quotaKeyRe matches a ResourceQuota hard key (cpu, memory, pods,
// requests.cpu, limits.memory, count/deployments.apps, ...).
var quotaKeyRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?(/[a-z0-9]([-a-z0-9.]*[a-z0-9])?)?$`)

// quotaValRe matches a quantity or count (4, 8Gi, 500m, 32).
var quotaValRe = regexp.MustCompile(`^[0-9a-zA-Z.\-+]+$`)

// validateKubeNS checks a slot namespace: a DNS-1123 label that is not one of
// the platform's namespaces.
func validateKubeNS(ns string) error {
	if !kubeNSRe.MatchString(ns) {
		return fmt.Errorf("kube slot ns %q must be a DNS label ([a-z0-9-], ≤63 chars)", ns)
	}
	if reservedKubeNamespaces[ns] {
		return fmt.Errorf("kube slot ns %q is a platform namespace — a slot is a tenant slice, pick a fresh name", ns)
	}
	return nil
}

// parseKubeQuota parses "cpu=4,memory=8Gi,pods=32" into the ResourceQuota
// hard map. Loose charset validation here (the cluster rejects invalid
// quantities loudly at apply — in Compute's report, or the build log).
func parseKubeQuota(quota string) (map[string]string, error) {
	hard := map[string]string{}
	for _, pair := range strings.Split(quota, ",") {
		k, v, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found || k == "" || v == "" {
			return nil, fmt.Errorf("kube quota %q: want key=value pairs (e.g. \"cpu=4,memory=8Gi,pods=32\")", quota)
		}
		if !quotaKeyRe.MatchString(k) || !quotaValRe.MatchString(v) {
			return nil, fmt.Errorf("kube quota %q: %q is not a resource=value pair", quota, pair)
		}
		hard[k] = v
	}
	return hard, nil
}

// kubeSlotManifest renders the slot as a kubectl-applicable JSON List —
// Namespace, SA <ns>-door, the full-access-within-the-namespace Role,
// its Binding, the optional ResourceQuota, and the SA-token Secret. The names
// mirror doors.tf's conventions (<service>-door / <service>-door-token); the
// token Secret's token is minted by the cluster's token controller — the
// manifest carries no credential.
func kubeSlotManifest(ns, quota string) ([]byte, error) {
	items := []map[string]any{
		{
			"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{
				"name":   ns,
				"labels": map[string]string{"app.kubernetes.io/managed-by": "freehold"},
			},
		},
		{
			"apiVersion": "v1", "kind": "ServiceAccount",
			"metadata": map[string]any{"name": kubeDoorSA(ns), "namespace": ns},
		},
		{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
			"metadata": map[string]any{"name": kubeDoorSA(ns), "namespace": ns},
			"rules": []map[string]any{{
				"apiGroups": []string{"*"}, "resources": []string{"*"}, "verbs": []string{"*"},
			}},
		},
		{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
			"metadata": map[string]any{"name": kubeDoorSA(ns), "namespace": ns},
			"roleRef": map[string]any{
				"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": kubeDoorSA(ns),
			},
			"subjects": []map[string]any{{
				"kind": "ServiceAccount", "name": kubeDoorSA(ns), "namespace": ns,
			}},
		},
	}
	if quota != "" {
		hard, err := parseKubeQuota(quota)
		if err != nil {
			return nil, err
		}
		items = append(items, map[string]any{
			"apiVersion": "v1", "kind": "ResourceQuota",
			"metadata": map[string]any{"name": ns + "-quota", "namespace": ns},
			"spec":     map[string]any{"hard": hard},
		})
	}
	items = append(items, map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name":        kubeDoorToken(ns),
			"namespace":   ns,
			"annotations": map[string]string{"kubernetes.io/service-account.name": kubeDoorSA(ns)},
		},
		"type": "kubernetes.io/service-account-token",
	})
	list := map[string]any{"apiVersion": "v1", "kind": "List", "items": items}
	return json.Marshal(list)
}

// kubeDoorSA / kubeDoorToken are the derived in-cluster names the manifest
// creates and the CP reads back (doors.tf's <service>-door / <service>-door-token,
// with the slot ns as the service).
func kubeDoorSA(ns string) string    { return ns + "-door" }
func kubeDoorToken(ns string) string { return ns + "-door-token" }

// kubeKubectl is the kubectl prefix inside the k3s guest (the kubeconfig is
// the guest's own — the same path the build's doorToken reads through).
func kubeKubectl() string {
	return "/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
}

// applyKubeSlot applies the slot manifest inside the k3s guest (through the
// box runner — the same exec path every build stage uses). Idempotent: apply
// merges, and the token Secret's data (the token the controller minted) is
// not in the manifest, so re-applying never disturbs a live token.
func (s *Spec) applyKubeSlot(ns, quota string) error {
	manifest, err := kubeSlotManifest(ns, quota)
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf(
		`pct exec %d -- sh -c "printf '%%s' '%s' | base64 -d | %s apply -f -"`,
		s.K3sVmid, base64.StdEncoding.EncodeToString(manifest), kubeKubectl())
	if _, err := s.execOut(cmd, 120); err != nil {
		return fmt.Errorf("apply kube slot %s: %w", ns, err)
	}
	return nil
}

// kubeSlotReady verifies the slot's ServiceAccount exists — the object the
// token belongs to. It is the provision flow's check that COMPUTE carved the
// slot before the CP reads and seals the token.
func (s *Spec) kubeSlotReady(ns string) error {
	out, err := s.execOut(fmt.Sprintf(
		`pct exec %d -- %s get sa %s -n %s -o jsonpath='{.metadata.name}'`,
		s.K3sVmid, kubeKubectl(), kubeDoorSA(ns), ns), 60)
	if err != nil || strings.TrimSpace(out) != kubeDoorSA(ns) {
		return fmt.Errorf("kube slot %s is not carved (its ServiceAccount is absent)", ns)
	}
	return nil
}

// kubeSlotToken reads the slot's SA token back (the doorToken read, retried:
// a just-carved Secret is populated by the token controller asynchronously).
// The token passes through CP memory only — it is sealed into the door
// runner's package, never stored or logged.
func (s *Spec) kubeSlotToken(ns string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		token, err := s.doorToken(kubeDoorToken(ns), ns)
		if err == nil && len(token) > 0 {
			return token, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, fmt.Errorf("read kube slot token %s/%s: %w", ns, kubeDoorToken(ns), lastErr)
	}
	return nil, fmt.Errorf("read kube slot token %s/%s: the token controller never populated it", ns, kubeDoorToken(ns))
}

// kubeCarveMissing is the provision flow's error when the slot is not carved:
// the CPA does not mutate the cluster — COMPUTE does, through its audited
// kube-api-root door — so the error carries the manifest to hand it.
func kubeCarveMissing(ns, quota string, err error) error {
	manifest, merr := kubeSlotManifest(ns, quota)
	if merr != nil {
		manifest = []byte(fmt.Sprintf("(manifest render failed: %v)", merr))
	}
	return fmt.Errorf("kube slot %s is not carved — the carve is COMPUTE's audited leg (kube-api-root), never this flow's: hand the manifest below to Compute to apply (kubectl apply -f - through its kube door), then re-run this provision. %v\n%s",
		ns, err, manifest)
}
