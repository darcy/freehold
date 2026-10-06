package cpbuild

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestValidateKubeNS pins the slot namespace guard: a DNS-1123 label that is
// never a platform namespace.
func TestValidateKubeNS(t *testing.T) {
	for _, ns := range []string{"yuvomi", "homelab-2026", "a"} {
		if err := validateKubeNS(ns); err != nil {
			t.Fatalf("ns %q should validate: %v", ns, err)
		}
	}
	for _, ns := range []string{"Yuvomi", "-bad", "bad-", "a_b", "",
		"kube-system", "kube-public", "default", "caddy", "litellm"} {
		if err := validateKubeNS(ns); err == nil {
			t.Fatalf("ns %q must be refused", ns)
		}
	}
}

// TestParseKubeQuota pins the ResourceQuota hard-spec parse: key=value pairs,
// invalid shapes refused.
func TestParseKubeQuota(t *testing.T) {
	hard, err := parseKubeQuota("cpu=4,memory=8Gi,pods=32,requests.cpu=2")
	if err != nil {
		t.Fatal(err)
	}
	if hard["cpu"] != "4" || hard["memory"] != "8Gi" || hard["pods"] != "32" || hard["requests.cpu"] != "2" {
		t.Fatalf("hard: %+v", hard)
	}
	for _, bad := range []string{"cpu", "cpu=", "=4", "cpu=4;rm -rf /", "cpu='4'", ",,"} {
		if _, err := parseKubeQuota(bad); err == nil {
			t.Fatalf("quota %q must be refused", bad)
		}
	}
}

// TestKubeSlotManifest pins the rendered slot: the six objects (five without a
// quota) under doors.tf's naming conventions, the Role scoped to full access
// WITHIN the namespace, and the token Secret carrying the SA annotation —
// never a credential.
func TestKubeSlotManifest(t *testing.T) {
	render := func(quota string) []map[string]any {
		t.Helper()
		raw, err := kubeSlotManifest("yuvomi", quota)
		if err != nil {
			t.Fatal(err)
		}
		var list struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("the manifest must render a JSON List: %v", err)
		}
		return list.Items
	}

	items := render("")
	wantKinds := []string{"Namespace", "ServiceAccount", "Role", "RoleBinding", "Secret"}
	if len(items) != len(wantKinds) {
		t.Fatalf("want %d objects, got %d", len(wantKinds), len(items))
	}
	for i, kind := range wantKinds {
		if items[i]["kind"] != kind {
			t.Fatalf("item %d kind = %v, want %s", i, items[i]["kind"], kind)
		}
	}
	meta := items[1]["metadata"].(map[string]any)
	if meta["name"] != "yuvomi-door" || meta["namespace"] != "yuvomi" {
		t.Fatalf("the SA: %v", meta)
	}
	secret := items[4]
	if secret["type"] != "kubernetes.io/service-account-token" {
		t.Fatalf("the token Secret's type: %v", secret["type"])
	}
	sm := secret["metadata"].(map[string]any)
	if sm["name"] != "yuvomi-door-token" {
		t.Fatalf("the token Secret's name: %v", sm)
	}
	if _, hasData := secret["data"]; hasData {
		t.Fatal("the manifest must never carry credential data — the token is the cluster's to mint")
	}

	quotaItems := render("cpu=4,memory=8Gi")
	if len(quotaItems) != 6 {
		t.Fatalf("a quota adds the ResourceQuota, got %d objects", len(quotaItems))
	}
	rq := quotaItems[4]
	if rq["kind"] != "ResourceQuota" {
		t.Fatalf("item 4 kind = %v, want ResourceQuota", rq["kind"])
	}
	hard := rq["spec"].(map[string]any)["hard"].(map[string]any)
	if hard["cpu"] != "4" || hard["memory"] != "8Gi" {
		t.Fatalf("the quota hard map: %v", hard)
	}

	if _, err := kubeSlotManifest("yuvomi", "not-a-quota"); err == nil {
		t.Fatal("an invalid quota must refuse to render")
	}
	if !strings.Contains(string(mustManifest(t, "yuvomi", "cpu=4")), `"kind":"List"`) {
		t.Fatal("the manifest renders as a List")
	}
}

func mustManifest(t *testing.T, ns, quota string) []byte {
	t.Helper()
	raw, err := kubeSlotManifest(ns, quota)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
