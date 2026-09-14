package config

import "testing"

// TestStripCIDR covers the recorded-ip CIDR strip (the config records ips
// as "1.2.3.4/24"; every URL built from them must drop the prefix).
func TestStripCIDR(t *testing.T) {
	cases := map[string]string{
		"192.168.30.240/24": "192.168.30.240",
		"10.0.0.1":          "10.0.0.1",
		"":                  "",
	}
	for in, want := range cases {
		if got := StripCIDR(in); got != want {
			t.Errorf("StripCIDR(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestK3sLiveNeedsRecordedIp: no k3s ip recorded => down (never derives the
// host from relay_url — the old accident that probed the proxy's 443).
func TestK3sLiveNeedsRecordedIp(t *testing.T) {
	cfg := &Config{RelayURL: "https://relay.example.test"}
	if K3sLive(cfg) {
		t.Error("K3sLive must be false when no k3s ip is recorded")
	}
}

// TestK3sLiveUnreachableIp: a recorded ip that answers nothing is down.
func TestK3sLiveUnreachableIp(t *testing.T) {
	cfg := &Config{}
	ip := "192.0.2.1/24" // TEST-NET-1: guaranteed unroutable
	cfg.Lxc.K3s.Ip = &ip
	if K3sLive(cfg) {
		t.Error("K3sLive must be false when the API does not answer")
	}
}

func str(s string) *string { return &s }

func strU32(s uint32) *uint32 { return &s }

// TestResolveTarget returns the recorded bare LAN IP per role (relay/cp/proxy)
// so pre-DNS steps connect to the guest instead of the not-yet-public host.
func TestResolveTarget(t *testing.T) {
	cfg := &Config{
		RelayURL: "https://relay.example.test",
		CPURL:    "https://cp.example.test",
		Lxc: LxcSpec{
			Relay: LxcGuest{Vmid: strU32(100), Ip: str("192.168.30.230/24")},
			Cp:    LxcGuest{Vmid: strU32(101), Ip: str("192.168.30.214/24")},
		},
		Proxy: ProxySpec{Ip: str("192.168.30.7/24")},
	}
	cases := []struct {
		role string
		want string
	}{
		{"relay", "192.168.30.230"},
		{"cp", "192.168.30.214"},
		{"proxy", "192.168.30.7"},
		{"k3s", ""},       // k3s sits behind the proxy; no own record
		{"bogus", ""},     // unknown role => no target
		{"unknown", ""},
	}
	for _, c := range cases {
		if got := cfg.ResolveTarget(c.role); got != c.want {
			t.Errorf("ResolveTarget(%q) = %q, want %q", c.role, got, c.want)
		}
	}
	// Bare cfg (nothing recorded) resolves to "": caller falls back to URL.
	empty := &Config{}
	if got := empty.ResolveTarget("relay"); got != "" {
		t.Errorf("empty ResolveTarget(relay) = %q, want empty", got)
	}
}

// TestLxcIP strips the CIDR and returns "" when no ip is recorded.
func TestLxcIP(t *testing.T) {
	if got := LxcIP(LxcGuest{}); got != "" {
		t.Errorf("LxcIP empty = %q, want empty", got)
	}
	if got := LxcIP(LxcGuest{Ip: str("10.9.8.7/16")}); got != "10.9.8.7" {
		t.Errorf("LxcIP CIDR = %q, want 10.9.8.7", got)
	}
}

// TestResolveTargetPrefersIp: the recorded IP wins over the encoded host
// regardless of what the URL says (the host may not resolve yet).
func TestResolveTargetPrefersIp(t *testing.T) {
	cfg := &Config{RelayURL: "https://relay.example.test", Lxc: LxcSpec{Relay: LxcGuest{Ip: str("172.16.1.5/24")}}}
	if got := cfg.ResolveTarget("relay"); got != "172.16.1.5" {
		t.Errorf("ResolveTarget(relay) = %q, want 172.16.1.5", got)
	}
}
