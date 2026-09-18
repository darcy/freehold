package proxmox

import (
	"encoding/base64"
	"strings"
	"testing"
)

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

// TestDnsApexCmd pins the apex command's FLAG-BEFORE-VERB order: Go's
// flag.Parse stops at the first non-flag arg, so --state-dir/--apex/--ip must
// precede the `apex` verb or they are silently dropped (state.Open("") fails).
func TestDnsApexCmd(t *testing.T) {
	cmd := DnsApexCmd(101, "/srv/data/cp/bin", "/srv/data/cp/control-plane", "librem.freehold.technology", "192.168.30.8")
	for _, mustAfter := range []struct{ before, after string }{
		{"--state-dir '/srv/data/cp/control-plane'", "'apex'"},
		{"--apex 'librem.freehold.technology'", "'apex'"},
		{"--ip '192.168.30.8'", "'apex'"},
	} {
		b := strings.Index(cmd, mustAfter.before)
		a := strings.Index(cmd, mustAfter.after)
		if b < 0 || a < 0 || b > a {
			t.Errorf("apex flags must precede the verb: %q before %q in %s", mustAfter.before, mustAfter.after, cmd)
		}
	}
	if !strings.HasPrefix(cmd, "pct exec 101 -- sh -c") {
		t.Errorf("unexpected apex cmd shape: %s", cmd)
	}
}

// TestDnsAddCmd pins the register command shape: the inner control-plane
// invocation is single-quoted once (the values are validated names/IPs), and a
// search base rides --domain when supplied.
func TestDnsAddCmd(t *testing.T) {
	cmd := DnsAddCmd(101, "/srv/data/cp/bin", "/srv/data/cp/control-plane", "relay", "10.0.0.5", "world-build relay", "d")
	for _, want := range []string{
		"pct exec 101 -- sh -c",
		"'/srv/data/cp/bin/freehold-console' 'dns' --state-dir '/srv/data/cp/control-plane' 'add' 'relay' '10.0.0.5' 'world-build relay' '--domain' 'd'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("DnsAddCmd missing %q:\n%s", want, cmd)
		}
	}
	noBase := DnsAddCmd(101, "/srv/data/cp/bin", "/srv/data/cp/control-plane", "cp", "10.0.0.6", "world-build cp", "")
	if strings.Contains(noBase, "--domain") {
		t.Errorf("no --domain when search base empty:\n%s", noBase)
	}
}

// TestDnsPointCmd: the pct set carries the resolver (+ the router only when
// passed, i.e. the CP guest keeps upstream), and the resolv.conf rewrite is a
// literal printf (no nested-quote hang).
func TestDnsPointCmd(t *testing.T) {
	pctSet, resolvConf := DnsPointCmd(100, "10.0.0.6", "", "d")
	if !strings.Contains(pctSet, "pct 'set' '100' '--nameserver' '10.0.0.6' '--searchdomain' 'd'") {
		t.Errorf("unexpected pct set:\n%s", pctSet)
	}
	if !strings.Contains(resolvConf, "printf 'search d\nnameserver 10.0.0.6\n' > /etc/resolv.conf") {
		t.Errorf("unexpected resolv.conf rewrite:\n%s", resolvConf)
	}
	cpSet, cpResolv := DnsPointCmd(101, "10.0.0.6", "10.0.0.1", "d")
	if !strings.Contains(cpSet, "'--nameserver' '10.0.0.6 10.0.0.1'") {
		t.Errorf("CP guest must keep the router as secondary:\n%s", cpSet)
	}
	if !strings.Contains(cpResolv, "nameserver 10.0.0.1") {
		t.Errorf("CP resolv.conf must list the router second:\n%s", cpResolv)
	}
}

// TestDnsVerifyCmd pins the honest-gate probe (dig against the resolver's own
// loopback, exact-IP grep).
func TestDnsVerifyCmd(t *testing.T) {
	cmd := DnsVerifyCmd(101, "relay", "10.0.0.5")
	for _, want := range []string{"dig +short +time=2 +tries=1 relay @127.0.0.1", "grep -qx '10.0.0.5'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("DnsVerifyCmd missing %q:\n%s", want, cmd)
		}
	}
}

// TestParsePctGateway ports the box parser: static guests carry gw= on net0;
// DHCP guests (ip=dhcp) have none.
func TestParsePctGateway(t *testing.T) {
	if got := ParsePctGateway("net0: name=eth0,bridge=vmbr0,gw=10.0.0.1,ip=10.0.0.6/24"); got != "10.0.0.1" {
		t.Errorf("static gw = %q", got)
	}
	if got := ParsePctGateway("net0: name=eth0,bridge=vmbr0,ip=dhcp"); got != "" {
		t.Errorf("dhcp gw must be empty, got %q", got)
	}
}
