package cpbuild

import (
	"strings"
	"testing"
)

// readScript returns one embedded terraform module script by name.
func readScript(t *testing.T, name string) string {
	t.Helper()
	b, err := terraformFS.ReadFile("terraform/scripts/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestPinK3sBringup pins the single owner of k3s install + the durable
// local-path carve-out: it must use the PINNED k3s version, carry the
// `KubeletInUserNamespace` flag AFTER the `server` subcommand (k3s rejects it
// before), and point the local-path provider at the durable /srv/data plane.
func TestPinK3sBringup(t *testing.T) {
	s := readScript(t, "k3s-bringup.sh")
	for _, want := range []string{
		"v1.36.4+k3s1",                 // pinned deterministic install
		"server --disable traefik --disable servicelb --kubelet-arg", // flag AFTER subcommand + ingress/LB disabled (caddy binds 80/443)
		"KubeletInUserNamespace=true",
		"/srv/data/k8s-volumes",        // durable local-path carve-out
		"local-path-config",            // local-path provider ConfigMap
		"get.k3s.io",                   // fetches the real installer
		"DEFAULT_PATH_FOR_NON_LISTED_NODES", // k3s node wildcard (NOT "k3s" - a wrong node name fails provisioning)
	} {
		if !strings.Contains(s, want) {
			t.Errorf("k3s-bringup.sh missing %q", want)
		}
	}
	// The inner script (between the `bash -c '` opening and the trailing `'`
	// close) must be single-quote-free: any apostrophe inside it terminates the
	// single-quoted wrapper and becomes a shell syntax error on the box (this
	// bit us once). Only the closing wrapper quote is allowed after the open.
	openMarker := "bash -c '"
	if open := strings.Index(s, openMarker); open >= 0 {
		openPos := open + len(openMarker)
		closePos := strings.LastIndex(s, "'")
		if closePos > openPos {
			if inner := s[openPos:closePos]; strings.Contains(inner, "'") {
				t.Errorf("k3s-bringup.sh has an apostrophe inside the bash -c '...' wrapper")
			}
		}
	}
}

// TestPinLitellmTf pins the deterministic service definition in litellm.tf: the
// fireworks egress pin, NodePort 31400, and that the master key / postgres
// password are VARIABLES (sensitive), never literals. Declarative via
// kubernetes_manifest (server-side apply, no local-path PVC bound-wait deadlock).
func TestPinLitellmTf(t *testing.T) {
	s, err := readTf(t, "litellm.tf")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"35.207.52.96",           // fireworks egress pin
		"nodePort   = 31400",     // litellm NodePort
		"var.litellm_master_key", // secret as a VARIABLE, never a literal
		"kubernetes_manifest",    // declarative k8s (SSA)
		"docker.litellm.ai/berriai/litellm:main-stable",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("litellm.tf missing %q", want)
		}
	}
	for _, forbidden := range []string{"fw_", "sk-", "postgres-pw =", "master-key ="} {
		if strings.Contains(s, forbidden) {
			t.Errorf("litellm.tf embedded a secret literal: %q", forbidden)
		}
	}
}

// TestPinPostgresTf pins postgres.tf: durable local-path PVC + postgres password
// as a VARIABLE (never a literal), declaratively via kubernetes_manifest.
func TestPinPostgresTf(t *testing.T) {
	s, err := readTf(t, "postgres.tf")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`storageClassName = "local-path"`,
		"var.postgres_password", // secret as a VARIABLE
		"postgres:16",
		"kubernetes_manifest",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("postgres.tf missing %q", want)
		}
	}
}

// readTf returns one embedded terraform module file by name.
func readTf(t *testing.T, name string) (string, error) {
	t.Helper()
	b, err := terraformFS.ReadFile("terraform/" + name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TestPinTfNoSecretVars guards that the module declares NO secret variables
// (secret values must ride the runner-injected env / TF_VAR_, never tfvars).
func TestPinTfNoSecretVars(t *testing.T) {
	b, err := terraformFS.ReadFile("terraform/main.tf")
	if err != nil {
		t.Fatalf("read main.tf: %v", err)
	}
	s := string(b)
	for _, forbidden := range []string{
		`variable "litellm"`, `variable "provider_key"`,
		`variable "postgres"`, `variable "api_key"`, `variable "secret"`,
	} {
		if strings.Contains(s, forbidden) {
			t.Errorf("main.tf declares a secret variable: %q", forbidden)
		}
	}
}
