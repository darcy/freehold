package cpdeploy

import (
	"encoding/json"
	"strings"
	"testing"

	"freehold/contract/wire"
)

// TestRunnerShipPlanAdoptKeepsIdentity guards the PR1 adopt invariant: on a
// re-adopt the box must NOT ship identity.json and must NOT merge its
// (differently sealed) secrets.json; on a mint it ships the identity and merges.
func TestRunnerShipPlanAdoptKeepsIdentity(t *testing.T) {
	files, merge := runnerShipPlan(true)
	for _, f := range files {
		if f == "identity.json" {
			t.Fatal("adopt must never ship identity.json over the plane's")
		}
	}
	if merge {
		t.Fatal("adopt must not merge the box's differently-sealed secrets.json")
	}
	files, merge = runnerShipPlan(false)
	hasIdentity := false
	for _, f := range files {
		if f == "identity.json" {
			hasIdentity = true
		}
	}
	if !hasIdentity || !merge {
		t.Fatalf("mint must ship the identity and merge secrets (files=%v merge=%v)", files, merge)
	}
}

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

// TestRunnerRunsCheckPayload: the probe rides LxcExec's single-quoted sh -c —
// a single apostrophe inside it terminates the wrapper and the payload dies as
// a shell syntax error BEFORE the probe runs (which once aborted every
// deploy-cp). The payload must therefore be single-quote-free, and the failure
// message must stay double-quoted with no substitutions or parens.
func TestRunnerRunsCheckPayload(t *testing.T) {
	payload := runnerRunsCheck("/srv/data/cp/bin/freehold-runner")
	if strings.Contains(payload, "'") {
		t.Error("payload contains a single quote — it dies inside the LxcExec single-quoted wrapper")
	}
	if strings.Contains(payload, "$(") {
		t.Error("payload contains a substitution — it would expand in the wrong shell")
	}
	if strings.Contains(payload, "docker run") {
		t.Error("the build recipe belongs in AGENTS.md, not in a guest-side echo")
	}
	// The failure path must exit non-zero (execToOK fails the deploy) and name
	// the actual failure (the glibc loader error is catted through).
	if !strings.Contains(payload, "exit 1") || !strings.Contains(payload, "cat /tmp/.fh-glibc") {
		t.Error("probe failure must exit 1 after printing the loader error")
	}
}
