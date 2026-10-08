package certcred

import (
	"testing"

	cert "freehold/platform/services/certificates/letsencrypt"
)

// TestPromptDNSMode locks the Phase-1 interactive surface: the binary ask —
// blank/1 = automated (cloudflare), 2/m/manual = the manual marker.
func TestPromptDNSMode(t *testing.T) {
	cases := map[string]string{
		"":           "cloudflare",
		"1":          "cloudflare",
		"cloudflare": "cloudflare",
		"2":          cert.ManualProviderName,
		"m":          cert.ManualProviderName,
		"manual":     cert.ManualProviderName,
		"  MANUAL  ": cert.ManualProviderName,
	}
	for in, want := range cases {
		e := &Engine{Prompt: func(string) (string, error) { return in, nil }}
		got, err := e.PromptDNSMode()
		if err != nil {
			t.Fatalf("PromptDNSMode(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("PromptDNSMode(%q) = %q, want %q", in, got, want)
		}
	}
}
