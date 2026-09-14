package deploy

import (
	"encoding/json"
	"testing"

	"freehold/contract/wire"
)

// TestMergeRunnerSecrets verifies the re-ship merge keeps the CP-only secrets
// the build added (litellm trio) and the console's grants, while letting the
// box package's own target + its per-target `secret` link win.
func TestMergeRunnerSecrets(t *testing.T) {
	box := []byte(`{"secrets":{"proxmox-box":"NEWBOX"},
		"targets":{"proxmox-box":{"kind":"ssh","address":"root@host:22","secret":"proxmox-box"}},
		"grants":["boxgrant"]}`)
	cp := []byte(`{"secrets":{"proxmox-box":"OLDCP","litellm":"L","postgres-pw":"P","provider-key":"K"},
		"targets":{"proxmox-box":{"kind":"ssh","address":"root@old:22","secret":"proxmox-box"},"extra":{"kind":"local","address":"","secret":""}},
		"grants":["consolegrant"]}`)

	merged, err := mergeRunnerSecrets(box, cp)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var got wire.SecretPackage
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	for k, want := range map[string]string{
		"proxmox-box":  "NEWBOX", // box wins on overlap
		"litellm":      "L",      // CP-only names preserved
		"postgres-pw":  "P",
		"provider-key": "K",
	} {
		if got.Secrets[k] != want {
			t.Errorf("secret %q = %q, want %q", k, got.Secrets[k], want)
		}
	}
	if got.Targets["proxmox-box"].Address != "root@host:22" {
		t.Errorf("box target must win: %+v", got.Targets["proxmox-box"])
	}
	if got.Targets["proxmox-box"].Secret != "proxmox-box" {
		t.Errorf("target secret link lost: %+v", got.Targets["proxmox-box"])
	}
	if _, ok := got.Targets["extra"]; !ok {
		t.Errorf("CP-only target lost: %+v", got.Targets)
	}
	if len(got.Grants) != 1 || got.Grants[0] != "consolegrant" {
		t.Errorf("CP grants must be preserved, got %v", got.Grants)
	}

	// A missing/corrupt remote package must not fail — the box package stands alone.
	alone, err := mergeRunnerSecrets(box, nil)
	if err != nil {
		t.Fatalf("merge with nil remote: %v", err)
	}
	var only wire.SecretPackage
	_ = json.Unmarshal(alone, &only)
	if len(only.Secrets) != 1 || only.Secrets["proxmox-box"] != "NEWBOX" {
		t.Errorf("nil remote should yield the box package alone, got %+v", only.Secrets)
	}
}
