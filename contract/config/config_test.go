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
