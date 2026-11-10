package cpbuild

import (
	"strings"
	"testing"
)

// TestWorldGatewayDisablesUFW pins the hosted-gateway fix: ufw's default
// deny-routed policy drops the forwarded 80/443 traffic even when the
// nftables DNAT is present. The freehold ruleset is the firewall, so every
// gateway assert must keep ufw out.
func TestWorldGatewayDisablesUFW(t *testing.T) {
	var got []string
	s := &Spec{
		GatewayCIDR:   "10.77.0.0/24",
		HostedGateway: true,
		ProxyIP:       "203.0.113.7",
		K3sIP:         "10.77.0.13",
		CpIP:          "10.77.0.12",
		RelayIP:       "10.77.0.11",
		Bridge:        "vmbr0",
		execHook: func(cmd string, _ uint64, _ ...string) (string, error) {
			got = append(got, cmd)
			return "", nil
		},
	}
	if err := s.worldGateway(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "ufw disable") {
		t.Fatalf("world gateway must disable ufw: %s", joined)
	}
	if strings.Contains(joined, "pct exec") {
		t.Fatal("a hosted gateway must run on the host, not in a guest")
	}
}

// TestWorldGatewayGuestDisablesUFW pins the guest-gateway shape: the script
// rides `pct exec ... -- sh -c '<script>'`, so the script itself must stay
// single-quote-free while still disabling ufw.
func TestWorldGatewayGuestDisablesUFW(t *testing.T) {
	var got []string
	s := &Spec{
		GatewayCIDR: "10.77.0.0/24",
		GatewayLxc:  103,
		ProxyIP:     "192.168.30.9",
		K3sIP:       "10.77.0.13",
		CpIP:        "10.77.0.12",
		RelayIP:     "10.77.0.11",
		RelayGW:     "192.168.30.1",
		Bridge:      "vmbr1",
		execHook: func(cmd string, _ uint64, _ ...string) (string, error) {
			got = append(got, cmd)
			return "", nil
		},
	}
	if err := s.worldGateway(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "ufw disable") {
		t.Fatalf("world gateway must disable ufw: %s", joined)
	}
	if strings.Count(joined, "'") != 2 {
		t.Fatalf("the pct sh -c wrapper is the only place single quotes may appear: %s", joined)
	}
}
