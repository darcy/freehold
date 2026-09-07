package stages

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// TestGenSecretHex mints unique 32-byte hex secrets.
func TestGenSecretHex(t *testing.T) {
	a, b := GenSecretHex(), GenSecretHex()
	if len(a) != 64 || len(b) != 64 {
		t.Fatalf("secret must be 32 bytes hex, got %q / %q", a, b)
	}
	if a == b {
		t.Fatal("two mints must differ")
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Errorf("not hex: %v", err)
	}
}

// TestK3sScriptsSingleQuoteFree guards the wrapping invariant: every script
// that travels inside a `pct exec ... -- bash -c '%s'` single-quote wrapper must
// contain no single-quote, or the wrapped command is truncated/breaks.
func TestK3sScriptsSingleQuoteFree(t *testing.T) {
	for _, s := range []string{K3sInstallScript, K3sLocalPathDurableScript} {
		if strings.Contains(s, "'") {
			t.Errorf("k3s script must be single-quote-free (travels in a bash -c '...'):\n%s", s)
		}
		if !strings.Contains(s, "set -euo pipefail") {
			t.Errorf("k3s script must be fail-fast: %s", s)
		}
	}
	if !strings.Contains(K3sInstallScript, "get.k3s.io") {
		t.Error("k3s install must fetch the installer")
	}
	if !strings.Contains(K3sLocalPathDurableScript, "/srv/data/k8s-volumes") {
		t.Error("local-path must point at the durable plane")
	}
}

// TestLitellmManifestScript: the apply script creates the Secrets FROM ENV
// (never a literal), embeds both workloads, and pins the durable PVC + the
// fireworks egress — the C0 kube surface.
func TestLitellmManifestScript(t *testing.T) {
	out := LitellmManifestScript(102, "masterkey", "pgpw", "providerkey")
	for _, want := range []string{
		"pct exec 102 -- sh -c",
		"master-key='masterkey'",
		"postgres-pw='pgpw'",
		"storageClassName: local-path",
		"nodePort: 31400",
		"35.207.52.96",                  // fireworks egress pin
		"LITELLM_MASTER_KEY",            // pod reads the k8s Secret
		"rollout status deploy/litellm", // readiness gate
		"LEG1_OK",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("manifest script missing %q", want)
		}
	}
	// The script must NEVER carry a literal secret value.
	for _, forbidden := range []string{"fw_", "masterKey", "postgresPw", "$LITELLM"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("script leaked a literal: %q", forbidden)
		}
	}
	if strings.Contains(out, "FREEHOLD_LITELLM_MASTER=") {
		t.Error("script must not embed the secret value")
	}
}

// TestLitellmRegisterScript: the registration leg asks the runner for the
// two secrets BY NAME and uses them in env, never literal.
func TestLitellmRegisterScript(t *testing.T) {
	out := LitellmRegisterScript("http://192.168.30.7:31400", "ControlPlaneAgent")
	for _, want := range []string{"$LITELLM", "$PROVIDER_KEY", "/model/new", "deepseek-v4-flash", "LEG2_OK", "http://192.168.30.7:31400", "ControlPlaneAgent"} {
		if !strings.Contains(out, want) {
			t.Errorf("register script missing %q", want)
		}
	}
	if strings.Contains(out, "127.0.0.1:31400") {
		t.Error("register script must target the real gateway URL, not loopback")
	}
	if strings.Contains(out, "fw_") {
		t.Error("register script leaked the provider key")
	}
}

// TestCaddyCertInstallScript guards the review finding that the cert-install
// helper script must FAIL (not silently succeed) when the PVC write doesn't
// happen: both shells run set -e, and the base64 payloads are single-quoted.
func TestCaddyCertInstallScript(t *testing.T) {
	fc := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	key := []byte("-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n")
	s := CaddyCertInstallScript(102, "relay", fc)

	for _, want := range []string{"$PDIR/tls/relay", "spec.local.path",
		"caddy-edge/relay", "rollout restart deploy/caddy", "pct push 102", "chmod 600"} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q", want)
		}
	}
	// The public fullchain is base64-embedded; the PRIVATE KEY must NOT be —
	// it comes from $CERT_KEY_RELAY on the HOST (runner-injected env).
	fcB64 := base64.StdEncoding.EncodeToString(fc)
	if !strings.Contains(s, "'"+fcB64+"'") {
		t.Errorf("fullchain base64 not single-quoted-embedded")
	}
	if strings.Contains(s, string(key)) || strings.Contains(s, base64.StdEncoding.EncodeToString(key)) {
		t.Errorf("private key must not appear in the audited install script:\n%s", s)
	}
	if !strings.Contains(s, `"${CERT_KEY_RELAY}" > /tmp/fh-key.pem`) {
		t.Errorf("script must source the private key from ${CERT_KEY_RELAY} on the host")
	}
	if !strings.Contains(s, "rm -f /tmp/fh-key.pem /tmp/fh-fc.pem") {
		t.Errorf("host temp cert files must be cleaned up")
	}
	// the guest shell (after the pct exec marker) must NOT reference the key env.
	if i := strings.Index(s, "pct exec 102 -- sh -c"); i >= 0 &&
		strings.Contains(s[i:], "${CERT_KEY_RELAY}") {
		t.Errorf("the guest shell must not read ${CERT_KEY_RELAY} (pct exec does not inherit host env)")
	}
}
