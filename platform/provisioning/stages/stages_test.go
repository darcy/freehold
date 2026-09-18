package stages

import (
	"encoding/hex"
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

// TestDnsRecords: the split-horizon record set — bare guest names + the dotted
// public hosts resolving to the PROXY (never a LXC); empty fields drop records.
func TestDnsRecords(t *testing.T) {
	recs := DnsRecords("relay.d", "10.0.0.5", "cp.d", "10.0.0.6", "10.0.0.7", "10.0.0.7")
	var got []string
	for _, r := range recs {
		got = append(got, r.Name)
	}
	for _, want := range []string{"relay", "cp", "proxy", "k3s", "litellm", "relay.d", "cp.d"} {
		if !containsStr(got, want) {
			t.Errorf("records missing %q: %v", want, got)
		}
	}
	for _, r := range recs {
		switch r.Name {
		case "relay":
			if r.IP != "10.0.0.5" {
				t.Errorf("relay must resolve to the relay LXC IP, got %s", r.IP)
			}
		case "relay.d", "cp.d":
			if r.IP != "10.0.0.7" {
				t.Errorf("%s must resolve to the proxy (Caddy), got %s", r.Name, r.IP)
			}
		}
	}
	noHost := DnsRecords("", "10.0.0.5", "", "10.0.0.6", "10.0.0.7", "")
	for _, r := range noHost {
		if r.Name == "relay.d" || r.Name == "cp.d" || r.Name == "litellm" {
			t.Errorf("empty field must drop the record, got %+v", r)
		}
	}
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
