package config

import "testing"

func TestNthIP(t *testing.T) {
	if got := nthIP("10.77.0.0/24", 1); got != "10.77.0.1" {
		t.Fatalf("gateway internal = %q", got)
	}
	if got := InternalIPFor("10.77.0.0/24", "relay"); got != "10.77.0.11" {
		t.Fatalf("relay internal = %q", got)
	}
	if got := InternalIPFor("10.77.0.0/24", "cp"); got != "10.77.0.12" {
		t.Fatalf("cp internal = %q", got)
	}
	if got := InternalIPFor("10.77.0.0/24", "k3s"); got != "10.77.0.13" {
		t.Fatalf("k3s internal = %q", got)
	}
	if got := nthIP("not a cidr", 1); got != "" {
		t.Fatalf("bad cidr should be empty, got %q", got)
	}
	// Cross an octet boundary: a /23's 300th host rides into the next octet.
	if got := nthIP("10.77.0.0/23", 300); got != "10.77.1.44" {
		t.Fatalf("carry = %q", got)
	}
}

func TestGatewayNftConf(t *testing.T) {
	conf := GatewayNftConf("10.77.0.0/24", "192.168.30.8", "10.77.0.13", "eth0")
	for _, want := range []string{
		"ip saddr 10.77.0.0/24 oifname \"eth0\" masquerade",
		"tcp dport { 80, 443 } dnat to 192.168.30.8",
		"tcp dport 6443 dnat to 10.77.0.13:6443",
	} {
		if !contains(conf, want) {
			t.Fatalf("ruleset missing %q:\n%s", want, conf)
		}
	}
	if contains(conf, "'") {
		t.Fatal("ruleset must be single-quote-free (rides sh -c verbatim)")
	}
}

func TestResolveTargetGateway(t *testing.T) {
	gw := "10.77.0.0/24"
	proxy := "192.168.30.8"
	cfg := &Config{
		Gateway: GatewaySpec{Cidr: &gw},
		Proxy:   ProxySpec{Ip: &proxy},
	}
	if got := cfg.ResolveTarget("cp"); got != "192.168.30.8" {
		t.Fatalf("behind a gateway ResolveTarget(cp) must be the edge, got %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
