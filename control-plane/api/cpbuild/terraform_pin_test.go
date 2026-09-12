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
		"v1.36.4+k3s1",              // pinned deterministic install
		"server --kubelet-arg",      // flag AFTER the subcommand (k3s rejects it before)
		"KubeletInUserNamespace=true",
		"/srv/data/k8s-volumes",     // durable local-path carve-out
		"local-path-config",         // local-path provider ConfigMap
		"get.k3s.io",                // fetches the real installer
	} {
		if !strings.Contains(s, want) {
			t.Errorf("k3s-bringup.sh missing %q", want)
		}
	}
}

// TestPinLitellmTf pins the deterministic service definition in litellm.tf: the
// fireworks egress pin, NodePort 31400, first-run-wins Secret (ignore_changes),
// and that the master key / postgres password are VARIABLES (sensitive), never
// literals.
func TestPinLitellmTf(t *testing.T) {
	s, err := readTf(t, "litellm.tf")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"35.207.52.96",           // fireworks egress pin
		"node_port   = 31400",    // litellm NodePort
		"ignore_changes = [data]", // first-run-wins Secret
		"var.litellm_master_key",  // secret as a VARIABLE, never a literal
		"kubernetes_secret",       // real declarative k8s resources
		"kubernetes_deployment",
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

// TestPinPostgresTf pins postgres.tf: durable local-path PVC + first-run-wins
// password Secret (a re-apply must never rotate Postgres's initialized data).
func TestPinPostgresTf(t *testing.T) {
	s, err := readTf(t, "postgres.tf")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`storage_class_name = "local-path"`,
		"var.postgres_password",       // secret as a VARIABLE
		"ignore_changes = [data]",     // first-run-wins
		"postgres:16",
		"kubernetes_persistent_volume_claim",
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
