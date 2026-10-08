package config

import (
	"strings"
	"testing"
)

func TestHostGatewayScriptShape(t *testing.T) {
	script := HostGatewayScript("10.77.0.0/24", "203.0.113.7", "10.77.0.13", "10.77.0.12", "10.77.0.11", "vmbr0")
	for _, want := range []string{
		"address 10.77.0.1/24",          // the host owns the subnet's .1
		"ip daddr 203.0.113.7",          // the DNATs match the public edge
		"dnat to 10.77.0.13",            // 80/443/6443 -> the k3s node
		"dnat to 10.77.0.12:8080",       // console
		"dnat to 10.77.0.11:3000",       // relay dial
		"policy drop;",                  // the input policy a public edge needs
		"tcp dport 22 accept",           // ssh stays
		"ip saddr 10.77.0.0/24 masquerade",
		"interface=vmbr0",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q", want)
		}
	}
	// Single-quote discipline: the script rides sh -c '...' and ssh verbatim.
	if strings.Contains(script, "'") {
		t.Error("script must not contain apostrophes")
	}
	// The public NIC is never touched: no bridge-ports, no eth0 stanza.
	if strings.Contains(script, "eth0") {
		t.Errorf("script must not name a WAN interface: %s", script)
	}
}

func TestHostGatewayDNATOnlyToEdge(t *testing.T) {
	conf := HostGatewayNftConf("10.77.0.0/24", "203.0.113.7", "10.77.0.13", "10.77.0.12", "10.77.0.11", "vmbr0")
	// An unconstrained dport DNAT hijacks the guests' own egress — every DNAT
	// must be pinned to the edge IP (the hairpin + the world's egress both
	// depend on it).
	if strings.Count(conf, "ip daddr 203.0.113.7") != 5 {
		t.Errorf("expected 5 edge-pinned DNAT rules, got:\n%s", conf)
	}
	if strings.Contains(conf, "eth0") {
		t.Error("the ruleset is interface-agnostic; no WAN ifname may leak in")
	}
}
