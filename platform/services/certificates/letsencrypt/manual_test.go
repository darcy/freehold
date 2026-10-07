package cert

import (
	"strings"
	"testing"

	"github.com/go-acme/lego/v4/challenge/dns01"
)

// dnsInfo computes the record the operator is shown for a challenge.
func dnsInfo(t *testing.T, domain, keyAuth string) dns01.ChallengeInfo {
	t.Helper()
	return dns01.GetChallengeInfo(domain, keyAuth)
}

// TestManualProviderInstruction locks the manual DNS contract: Present hands
// the operator the record (fqdn + value) as an instruction error and marks
// itself instructional; buildProvider resolves the name to it.
func TestManualProviderInstruction(t *testing.T) {
	const keyAuth = "token-abc.defghi"
	const domain = "relay.librem.freehold.technology"

	p, err := buildProvider(ManualProviderName, map[string]string{})
	if err != nil {
		t.Fatalf("buildProvider(manual): %v", err)
	}
	m, ok := p.(instructional)
	if !ok || !m.Instructional() {
		t.Fatal("manual provider is not instructional — the order would not persist on Present failure")
	}
	err = p.Present(domain, "token-abc", keyAuth)
	if err == nil {
		t.Fatal("expected Present to fail with the instruction")
	}
	info := dnsInfo(t, domain, keyAuth)
	for _, want := range []string{info.EffectiveFQDN, info.Value, "freehold build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("instruction error missing %q:\n%s", want, err)
		}
	}
	if err := p.CleanUp(domain, "token-abc", keyAuth); err != nil {
		t.Errorf("CleanUp: %v", err)
	}

	// buildProvider must resolve "manual" to OUR shim, not lego's stock
	// DNSProviderManual (which waits on stdin — wrong topology for a
	// server-side CP). The instructional assertion above proves the shadow.
	if !IsProvider(ManualProviderName) {
		t.Error("lego's registry has manual; IsProvider should agree (the shim shadows it in buildProvider)")
	}
}
